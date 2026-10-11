// Command catalog-inspect 是一次性的、有界的 Antigravity 模型目录取数工具：
// 复用线上正式装配（配置加载、Ent 账号仓储、AntigravityTokenProvider/OAuth/客户端），
// 对固定账号集合各发起一次逻辑 FetchAvailableModels，把结果投影成 stdout 上的单个 JSON。
//
// 用法：
//
//	CONFIG_FILE=/path/to/config.yaml go run ./cmd/catalog-inspect -accounts 63,64,69
//
// 契约：
//   - stdout 只输出一个 JSON 文档（顶层 generated_at/source/accounts；每账号
//     account_id/fetched_at/endpoint/configured_endpoints/model_count/model_ids/
//     models/error），不含原始响应、token、project、email、请求头。
//   - 所有失败都是有限 stage 字符串（+ 可选 HTTP 状态码），不输出任何自由文本错误：
//     上游错误体、libpq DSN、令牌端点回显都没有出口；被复用代码的日志出口
//     （os.Stdout / 标准库 log / 默认 slog）在启动时即接到 os.DevNull。
//   - 账号集合固定为 {63,64,69}，只接受它的非空子集；集合外 ID 直接失败关闭。
//   - 退出码：0 全部成功（含显式 -h）；1 报告已输出但至少一个账号失败；2 无法产出报告。
//
// 写副作用（必须知悉）：
//   - 令牌正常刷新是必要写入：access token 临近过期时，正式刷新链会把轮换后的
//     access_token/refresh_token/_token_version 写回 accounts.credentials（DB），
//     并写 Redis oauth:token/oauth:refresh_lock 键；刷新失败时还会写
//     accounts.temp_unschedulable_* 与 Redis 临时不可调度键。
//   - 数据库只做只读打开（正式 DSN + 正式驱动构造 + ProvideSQLDB + Ping）：
//     不执行迁移、不补齐启动密钥、不补默认分组。
//   - 本工具不启动 HTTP 服务、不注册任何后台 worker；每个账号只调用一次
//     FetchAvailableModels，不做重试、不取配额汇总。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"

	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	// configEnvVar 是正式配置加载器识别的配置文件路径环境变量。
	configEnvVar     = "CONFIG_FILE"
	defaultTimeout   = 90 * time.Second
	exitOK           = 0
	exitAccountError = 1
	exitFatal        = 2
)
const usageText = `catalog-inspect: 读取 Antigravity 账号的真实可用模型目录

用法:
  catalog-inspect [-accounts 63,64,69] [-timeout 90s] [-config /path/to/config.yaml]
  -h/--help    显示本说明（退出码 0，不产生 stdout 输出）

说明:
  复用线上正式装配（配置加载、Ent 账号仓储、AntigravityTokenProvider/OAuth/客户端），
  对每个账号依次发起一次逻辑 FetchAvailableModels，stdout 只输出一个 JSON 文档。
  -accounts 只接受固定账号集合 {63,64,69} 的非空子集，集合外 ID 一律失败关闭。
  -config 非空时写入 CONFIG_FILE 环境变量；它优先于 DATA_DIR/默认搜索路径。

写副作用:
  临近过期时令牌会按正式刷新链写入 accounts.credentials（access_token/refresh_token/
  _token_version）、Redis oauth:token/oauth:refresh_lock，刷新失败还会写临时不可调度状态。
  数据库只做只读打开：不执行迁移、不补齐启动密钥、不补默认分组。

失败信号:
  stdout 的 error 是有限 stage（可选 http_status=100..599），stderr 的失败行也是有限 stage。
  被复用代码的日志（含上游错误体、令牌回显、带 DSN 的数据库错误）全部丢弃到 os.DevNull，
  不会出现在 stdout 或 stderr 上；stderr 只承载本工具自己的有限 stage 诊断。

退出码:
  0 全部账号成功（-h 也是 0）；1 报告已输出但存在失败账号；2 无法产出报告（参数/配置/数据库）。
`

// options 是本工具的全部入参。
type options struct {
	accounts string
	timeout  time.Duration
	config   string
}

func main() {
	// 库输出必须先去出口：复用到的正式代码会把上游错误体、令牌回显、带 DSN 的数据库
	// 错误原样打印。这里把进程级日志出口全部接到 os.DevNull，只保留原 stdout（写 JSON）
	// 与原 stderr（写本工具自己的有限 stage 诊断）。
	stdout, stderr, devNull, err := silenceLibraryOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "catalog-inspect: fatal: %s\n", stageOf(err))
		os.Exit(exitFatal)
	}
	code := run(os.Args[1:], stdout, stderr)
	_ = devNull.Close()
	os.Exit(code)
}

// newFlagSet 构造独立 FlagSet，使调用方能拿到解析错误而不是直接终止进程。
func newFlagSet(opts *options, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("catalog-inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = fmt.Fprint(stderr, usageText) }
	flags.StringVar(&opts.accounts, "accounts", fixedAccountIDsList(), "固定账号集合 "+fixedAccountSetDescription()+" 的非空子集")
	flags.DurationVar(&opts.timeout, "timeout", defaultTimeout, "整轮巡检的整体超时")
	flags.StringVar(&opts.config, "config", "", "config.yaml 路径；非空时覆盖 CONFIG_FILE")
	return flags
}

// run 执行一轮巡检，报告写入 stdout；失败信号（有限 stage）写入 stderr。
func run(args []string, stdout, stderr io.Writer) int {
	var opts options
	flags := newFlagSet(&opts, stderr)
	if err := flags.Parse(args); err != nil {
		// -h/-help 与非法参数都由 FlagSet 打印用法，这里只统一退出码。
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFatal
	}
	failf := func(err error) int {
		fmt.Fprintf(stderr, "catalog-inspect: fatal: %s\n", stageOf(err))
		return exitFatal
	}

	ids, err := resolveFixedAccountIDs(opts.accounts)
	if err != nil {
		return failf(err)
	}
	if path := strings.TrimSpace(opts.config); path != "" {
		if err := os.Setenv(configEnvVar, path); err != nil {
			return failf(newStageError(stageConfigLoadFailed))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	rep, err := collectReport(ctx, ids)
	if err != nil {
		return failf(err)
	}
	if err := writeReport(stdout, rep); err != nil {
		return failf(newStageError(stageReportEncodeFailed))
	}
	if failed, ok := firstAccountError(rep); ok {
		fmt.Fprintln(stderr, describeAccountFailure(failed.AccountID, failed.Error))
		return exitAccountError
	}
	return exitOK
}

// silenceLibraryOutput 把进程级日志出口全部改接到 os.DevNull，并返回原始
// stdout/stderr 句柄：stdout 只用于最终 JSON，stderr 只用于本工具自己的有限 stage 诊断。
//
// 为什么不把库输出转发到 stderr：被复用的正式代码会原样打印上游错误体与令牌回显
// （antigravity_oauth_service / antigravity_token_refresher 的 fmt.Printf）、
// 带 DSN 的数据库与配置告警（log / slog）。这些文本一旦落到用户可见的任何流上，
// 就可能带出 token/email/project/DSN，因此整体丢弃而不是转发。
//
// 实际被触发的三个出口：os.Stdout（fmt.Printf）、标准库 log（antigravity 客户端/时区/
// refresher）、默认 slog（provider / OAuthRefreshAPI / 配置校验）。三者都指向 devNull。
//
// 打开 devNull 失败时返回有限 stage 错误并由调用方失败关闭：拿不到“静音出口”就
// 无法保证上面的不泄露约定，不能继续执行。
func silenceLibraryOutput() (stdout, stderr *os.File, devNull *os.File, err error) {
	devNull, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, nil, nil, newStageError(stageLibrarySilenceFailed)
	}
	stdout = os.Stdout
	stderr = os.Stderr
	os.Stdout = devNull
	os.Stderr = devNull
	// 两行都要接 devNull，且顺序无关：slog.SetDefault 会把标准库 log 的输出换成指向
	// 自己 handler 的 slog.handlerWriter，因此它同时也是 log 的出口；去掉任何一行，
	// 都会留下一条回到原始终端的日志通道。
	log.SetOutput(devNull)
	slog.SetDefault(slog.New(slog.NewTextHandler(devNull, nil)))
	return stdout, stderr, devNull, nil
}

// writeReport 把报告编码为 JSON 写入目标（生产为原 stdout 句柄）。
func writeReport(w io.Writer, rep *report) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(rep)
}

// collectReport 装配最小依赖集并逐账号取数。
//
// 这里刻意不调用 wire 生成的 initializeApplication：那条路径会拉起 HTTP 服务、
// 定时 worker 与调度器，超出本工具“有界、无服务、无 worker”的范围；也不调用
// repository.InitEnt，因为它会执行迁移、补齐启动密钥、补默认分组等启动期写入。
func collectReport(ctx context.Context, ids []int64) (*report, error) {
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		return nil, newStageError(stageConfigLoadFailed)
	}

	entClient, db, err := openCatalogDB(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = entClient.Close() }()

	redisClient := repository.ProvideRedis(cfg)
	defer func() { _ = redisClient.Close() }()

	accountRepo := repository.NewAccountRepository(entClient, db, repository.ProvideSchedulerCache(redisClient, cfg))
	tokenCache := repository.NewGeminiTokenCache(redisClient)
	antigravityOAuthService := service.NewAntigravityOAuthService(repository.NewProxyRepository(entClient, db))
	// 该 service 内部会起一个会话清理 ticker goroutine；必须显式停掉，避免留下后台活动。
	defer antigravityOAuthService.Stop()

	tokenProvider := service.ProvideAntigravityTokenProvider(
		accountRepo,
		tokenCache,
		antigravityOAuthService,
		service.ProvideOAuthRefreshAPI(accountRepo, tokenCache),
		repository.NewTempUnschedCache(redisClient),
	)

	in := &inspector{
		tokens: tokenProvider,
		newFetcher: func(account *service.Account) (modelFetcher, error) {
			// 代理跟随账号行，与线上模型同步路径 upstreamModelsProxyURL 一致。
			return antigravity.NewClient(accountProxyURL(account))
		},
		bodyLimit: resolveModelsBodyLimit(cfg),
		now:       time.Now,
	}

	rep := &report{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Source:      reportSource,
		Accounts:    make([]accountReport, 0, len(ids)),
	}
	for _, id := range ids {
		account, err := accountRepo.GetByID(ctx, id)
		if err != nil {
			rep.Accounts = append(rep.Accounts, missingAccountReport(id, err))
			continue
		}
		rep.Accounts = append(rep.Accounts, in.inspect(ctx, account))
	}
	return rep, nil
}

// missingAccountReport 在账号行不可用时仍输出同形状报告，便于下游逐账号统计。
// 错误只落到有限 stage：仓储错误文本可能内嵌 DSN，绝不外传。
func missingAccountReport(id int64, err error) accountReport {
	stage := stageAccountLoadFailed
	if errors.Is(err, service.ErrAccountNotFound) {
		stage = stageAccountNotFound
	}
	return accountReport{
		AccountID:           id,
		FetchedAt:           time.Now().UTC().Format(time.RFC3339),
		ConfiguredEndpoints: append([]string(nil), antigravity.BaseURLs...),
		ModelIDs:            []string{},
		Models:              []modelCapability{},
		Error:               newReportError(stage),
	}
}

// accountProxyURL 复刻正式模型同步路径的代理选择语义。
func accountProxyURL(account *service.Account) string {
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}
