// Command model-test-smoke 对固定账号 64 的固定模型 gemini-3.8-flash 发起**一次**逻辑生成，
// 用线上正式装配（配置加载 / Ent 账号仓储 / AntigravityTokenProvider / 正式网关），
// 并把可见结果限制成白名单字段。
//
// 用法:
//
//	CONFIG_FILE=/path/to/config.yaml go run ./cmd/model-test-smoke -execute
//	go run ./cmd/model-test-smoke            # dry-run：只输出空汇总，无任何副作用
//
// 契约:
//   - 目标写死：账号 64、模型 gemini-3.8-flash、ReasoningEffort=medium、prompt 留空（由全局
//     model_test_prompt 设置生效）。没有账号/模型/effort 参数，避免误伤其他账号。
//   - 只调用一次 service.TestAccountConnection(真实 gin.Context, 64, "gemini-3.8-flash", "",
//     AccountTestModeDefault, AccountTestOptions{ReasoningEffort:"medium"})，走的正是正式
//     routeAntigravityTest → streamAntigravityAccountTest → AntigravityGatewayService.Forward；
//     一次逻辑生成内部官方网关仍可能自行重试（429/503 智能重试），本工具不额外重试、不做矩阵。
//   - stdout 只有一个汇总 JSON（流身份 / effective_prompt / effective_effort / stream_mode /
//     conflict / chunk_count / text_chars / first_content_ms / elapsed_ms / success / stage）。
//     **没有正文、工具载荷或原始上游字节**：SSE 帧只被增量计数解析、原始字节不落缓冲区，
//     signature 一类字段在结构上没有出口。
//   - 库日志出口（os.Stdout / log / slog / gin 的两个 writer）在加载配置前接到 os.DevNull；
//     失败只有有限 stage，panic 也折成有限 stage 且不打印堆栈。
//
// 写副作用与退出码见 -h 与本目录 README.md（令牌刷新写 accounts.credentials/Redis、
// 数据库只读打开、无 HTTP 服务与后台 worker；0 成功 / 1 生成失败 / 2 无法产出汇总）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

const (
	// 固定目标：改任何一项都要改代码，而不是改命令行。
	fixedAccountID = int64(64)
	fixedModel     = "gemini-3.8-flash"
	fixedEffort    = "medium"

	defaultTimeout = 90 * time.Second
	maxTimeout     = 120 * time.Second
	configEnvVar   = "CONFIG_FILE" // 正式配置加载器识别的配置文件路径环境变量
	// maxPendingBytes 是未成帧字节上限：正常 SSE 行只有几 KB，超过它说明对端在写非 SSE 的
	// 大块内容，直接丢弃而不是留在内存里。
	maxPendingBytes = 8 << 20
	// accountMissingText 是正式代码在账号行不可用时的固定字面量（精确等值匹配，不回声）。
	accountMissingText = "Account not found"

	exitOK     = 0
	exitFailed = 1
	exitFatal  = 2
)

// 有限 stage：stdout 的 stage 与 stderr 的 fatal 行都只取这些值。
const (
	stageOK               = "ok"
	stageStreamIncomplete = "stream_incomplete"
	stageAccountNotFound  = "account_not_found"
	stageGenerationFailed = "generation_failed"
	stageTimeout          = "generation_timeout"
	stageCanceled         = "generation_canceled"
	stageInternalPanic    = "internal_panic"
	stageUnknown          = "unknown_failure"
	stageDryRun           = "dry_run_no_generation"

	stageTimeoutOutOfRange = "invalid_arguments_timeout_out_of_range"
	stageLibrarySilence    = "library_output_silence_failed"
	stageConfigLoad        = "config_load_failed"
	stageDatabase          = "database_unreachable"
	stageRedis             = "redis_unreachable"
	stageSetting           = "setting_load_failed"
	stageEnvironment       = "environment_failed"
	stageOutputWrite       = "output_write_failed"
)

// usageText 是 -h/--help 的全部输出（只写 stderr，且不触发任何配置/数据库/网络访问）。
const usageText = `model-test-smoke: 对固定账号 64 + 模型 ` + fixedModel + ` 发起一次逻辑生成

用法:
  model-test-smoke -execute [-config /path/to/config.yaml] [-timeout 90s]
  model-test-smoke            # dry-run：只输出空汇总（stage=dry_run_no_generation），无副作用

参数: -execute 必须显式给出；-timeout 默认 90s（>0 且 <=120s）；-config 覆盖 CONFIG_FILE。
目标固定: 账号 64、模型 ` + fixedModel + `、ReasoningEffort=medium、prompt 留空（全局
model_test_prompt 设置生效）。只发起一次逻辑生成（网关在一次生成内部可能自行重试）。

stdout: 一个汇总 JSON，只含流身份/effective_prompt/effective_effort/stream_mode/conflict、
chunk_count/text_chars/first_content_ms/elapsed_ms/success/stage；不含正文或原始上游内容。
写副作用: 令牌正常刷新会写 accounts.credentials 与 Redis oauth:token/oauth:refresh_lock；
数据库只读打开（不迁移、不补默认分组）；不启动 HTTP 服务与后台 worker。
退出码: 0 成功；1 汇总已输出但生成失败；2 无法产出汇总（参数/配置/数据库/Redis/装配）。
`

type options struct {
	timeout time.Duration
	config  string
	execute bool
}

// generationFunc 是唯一的重活入口；dry-run / -h / 参数错误必须在调用它之前返回。
type generationFunc func(ctx context.Context, opts options, stats *eventStats) error

func main() {
	// 库输出必须先去出口：复用到的正式代码会把上游错误体、令牌回显、带 DSN 的数据库错误
	// 原样打印（fmt.Printf / log / slog / gin）。全部接到 os.DevNull，只保留原 stdout（写汇总）
	// 与原 stderr（写本工具自己的有限 stage 诊断）。
	stdout, stderr, devNull, err := silenceLibraryOutput()
	if err != nil {
		fmt.Fprint(os.Stderr, fatalLine(stageOf(err)))
		os.Exit(exitFatal)
	}
	code := run(os.Args[1:], stdout, stderr, runGeneration)
	_ = devNull.Close()
	os.Exit(code)
}

func newFlagSet(opts *options, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("model-test-smoke", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = fmt.Fprint(stderr, usageText) }
	flags.BoolVar(&opts.execute, "execute", false, "必须显式给出才会真正发起一次逻辑生成")
	flags.DurationVar(&opts.timeout, "timeout", defaultTimeout, "整轮上限（>0 且 <=120s）")
	flags.StringVar(&opts.config, "config", "", "config.yaml 路径；非空时覆盖 CONFIG_FILE")
	return flags
}

// run 执行一轮冒烟：汇总写 stdout，失败信号（有限 stage）写 stderr。
func run(args []string, stdout, stderr io.Writer, generate generationFunc) int {
	var opts options
	flags := newFlagSet(&opts, stderr)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFatal
	}
	failf := func(stage string) int {
		fmt.Fprint(stderr, fatalLine(stage))
		return exitFatal
	}
	// 入参先于任何副作用校验：dry-run / -h / 坏参数因此不可能触碰配置、数据库或网络。
	if opts.timeout <= 0 || opts.timeout > maxTimeout {
		return failf(stageTimeoutOutOfRange)
	}
	started := time.Now()
	stats := &eventStats{start: started}
	if !opts.execute {
		if err := writeSummary(stdout, opts, stats, started, started, stageDryRun, true); err != nil {
			return failf(stageOutputWrite)
		}
		return exitOK
	}
	if path := strings.TrimSpace(opts.config); path != "" {
		if err := os.Setenv(configEnvVar, path); err != nil {
			return failf(stageConfigLoad)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	generateErr := guardedGenerate(generate, ctx, opts, stats)
	finished := time.Now()

	stage := stageOK
	if generateErr != nil {
		// 生成器只返回有限 stage；万一返回别的错误，也折成固定 stage 而不回声文本。
		if stage = stageOf(generateErr); stage == stageUnknown {
			stage = stageGenerationFailed
		}
		// 环境/装配失败发生在汇总结构建立之前：不输出 JSON，只给 stderr 有限 stage。
		if fatalStage(stage) {
			return failf(stage)
		}
	} else if !stats.completeSeen {
		stage = stageStreamIncomplete // 服务没有送出收尾事件：流没走完，不算成功
	}
	if err := writeSummary(stdout, opts, stats, started, finished, stage, false); err != nil {
		return failf(stageOutputWrite)
	}
	if stage == stageOK {
		return exitOK
	}
	return exitFailed
}

// guardedGenerate 把 panic 折成有限 stage：任何情况下都不打印堆栈或 panic 值（panic 值里可能
// 内嵌带 DSN 的错误、上游 URL、令牌）。
func guardedGenerate(generate generationFunc, ctx context.Context, opts options, stats *eventStats) (err error) {
	defer func() {
		if recover() != nil {
			err = newStageError(stageInternalPanic)
		}
	}()
	return generate(ctx, opts, stats)
}

// stageError 携带有限 stage 字符串。
type stageError struct{ stage string }

func (e *stageError) Error() string { return e.stage }

func newStageError(stage string) error { return &stageError{stage: stage} }

// stageOf 把任意错误压成有限 stage；未知错误也落在一个固定的 stage 上。
func stageOf(err error) string {
	var staged *stageError
	if errors.As(err, &staged) {
		return staged.stage
	}
	return stageUnknown
}

// fatalStage 判定“无法产出汇总”的阶段：这些失败发生在汇总建立之前，统一走退出码 2。
func fatalStage(stage string) bool {
	switch stage {
	case stageLibrarySilence, stageConfigLoad, stageDatabase, stageRedis, stageSetting, stageEnvironment, stageOutputWrite:
		return true
	}
	return false
}

// fatalLine 是 stderr 上唯一的诊断行：工具名 + 有限 stage，不含任何库文本。
func fatalLine(stage string) string {
	return fmt.Sprintf("model-test-smoke: fatal: %s\n", stage)
}

// summary 是 stdout 上唯一的产物：只有固定目标、白名单流元数据与有限计数/计时，
// 没有任何能装下正文、工具载荷或原始上游内容的字段。
type summary struct {
	Tool                          string `json:"tool"`
	DryRun                        bool   `json:"dry_run"`
	AccountID                     int64  `json:"account_id"`
	Model                         string `json:"model"`
	ReasoningEffort               string `json:"reasoning_effort"`
	TimeoutSeconds                int64  `json:"timeout_seconds"`
	RequestedModel                string `json:"requested_model,omitempty"`
	UpstreamSentModel             string `json:"upstream_sent_model,omitempty"`
	UpstreamResponseModel         string `json:"upstream_response_model,omitempty"`
	UpstreamResponseModelConflict bool   `json:"upstream_response_model_conflict"`
	EffectivePrompt               string `json:"effective_prompt,omitempty"`
	EffectiveEffort               string `json:"effective_effort,omitempty"`
	StreamMode                    string `json:"stream_mode,omitempty"`
	ChunkCount                    int    `json:"chunk_count"`
	TextChars                     int    `json:"text_chars"`
	FirstContentMs                *int64 `json:"first_content_ms,omitempty"`
	ElapsedMs                     int64  `json:"elapsed_ms"`
	Success                       bool   `json:"success"`
	Stage                         string `json:"stage"`
	Network                       string `json:"network,omitempty"`
}

// writeSummary 是唯一允许写 stdout 的地方（dry-run 时 stats 全为零值，正好是“什么都没发生”）。
func writeSummary(w io.Writer, opts options, stats *eventStats, started, finished time.Time, stage string, dryRun bool) error {
	out := summary{
		Tool:                          "model-test-smoke",
		DryRun:                        dryRun,
		AccountID:                     fixedAccountID,
		Model:                         fixedModel,
		ReasoningEffort:               fixedEffort,
		TimeoutSeconds:                int64(opts.timeout / time.Second),
		RequestedModel:                stats.requestedModel,
		UpstreamSentModel:             stats.upstreamSentModel,
		UpstreamResponseModel:         stats.upstreamResponseModel,
		UpstreamResponseModelConflict: stats.conflict,
		EffectivePrompt:               stats.effectivePrompt,
		EffectiveEffort:               stats.effectiveEffort,
		StreamMode:                    stats.streamMode,
		ChunkCount:                    stats.chunkCount,
		TextChars:                     stats.textChars,
		FirstContentMs:                stats.firstContentMs,
		ElapsedMs:                     finished.Sub(started).Milliseconds(),
		Success:                       stage == stageOK,
		Stage:                         stage,
	}
	if dryRun {
		out.Network = "not_attempted" // 显式声明：没有发生过配置/数据库/网络动作
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(out)
}

// eventStats 是本工具出站的**全部**事实：只有下面这些标量，没有字符串切片/缓冲区可以装下
// 正文或原始载荷。字段都来自白名单事件（统一由 sendEvent 产生）。
type eventStats struct {
	start                 time.Time
	requestedModel        string
	upstreamSentModel     string
	upstreamResponseModel string
	conflict              bool
	effectivePrompt       string
	effectiveEffort       string
	streamMode            string
	chunkCount            int
	textChars             int
	firstContentMs        *int64
	completeSeen          bool
}

// observe 处理一条 `data:` 载荷。非白名单事件（error / image / 原始协议帧等）直接忽略；
// content 只计数，正文文本没有落点。
func (s *eventStats) observe(payload string) {
	var raw struct {
		Type                          string `json:"type"`
		Text                          string `json:"text"`
		RequestedModel                string `json:"requested_model"`
		UpstreamSentModel             string `json:"upstream_sent_model"`
		UpstreamResponseModel         string `json:"upstream_response_model"`
		UpstreamResponseModelConflict *bool  `json:"upstream_response_model_conflict"`
		EffectivePrompt               string `json:"effective_prompt"`
		EffectiveEffort               string `json:"effective_effort"`
		StreamMode                    string `json:"stream_mode"`
	}
	if json.Unmarshal([]byte(payload), &raw) != nil {
		return
	}
	switch raw.Type {
	case "test_start", "test_metadata", "test_complete":
		assign(&s.requestedModel, raw.RequestedModel)
		assign(&s.upstreamSentModel, raw.UpstreamSentModel)
		assign(&s.upstreamResponseModel, raw.UpstreamResponseModel)
		assign(&s.effectivePrompt, raw.EffectivePrompt)
		assign(&s.effectiveEffort, raw.EffectiveEffort)
		assign(&s.streamMode, raw.StreamMode)
		// 冲突标志只上锁一次：TestEvent 该字段没有 omitempty，连收尾的 test_complete 都会带
		// false，按“后到覆盖”会把 test_metadata 刚报出的 true 抹掉。
		if raw.UpstreamResponseModelConflict != nil && *raw.UpstreamResponseModelConflict {
			s.conflict = true
		}
		if raw.Type == "test_complete" {
			s.completeSeen = true
		}
	case "content":
		// 正式代码在 tool_use 分支会把 content_block 的 Raw JSON 当正文发出来；只统计真正的
		// 模型文本，JSON 形状的载荷不计数（也不保留）。
		if trimmed := strings.TrimSpace(raw.Text); trimmed == "" || trimmed[0] == '{' || trimmed[0] == '[' {
			return
		}
		s.chunkCount++
		s.textChars += utf8.RuneCountInString(raw.Text)
		if s.firstContentMs == nil {
			elapsed := time.Since(s.start).Milliseconds()
			s.firstContentMs = &elapsed
		}
	}
}

// assign 做“非空覆盖”：后到的非空元数据优先（test_start 之后还有 test_metadata）。
func assign(target *string, value string) {
	if value != "" {
		*target = value
	}
}

// smokeWriter 是交给 gin 的 writer：Header/Flush/Status 语义继承 httptest.ResponseRecorder，
// Write 只做增量计数解析，**不保留也不转发原始字节**。原始上游内容（含 signature）因此既没有
// 缓冲区也没有出口。
type smokeWriter struct {
	*httptest.ResponseRecorder
	stats   *eventStats
	pending []byte
}

func newSmokeWriter(stats *eventStats) *smokeWriter {
	return &smokeWriter{ResponseRecorder: httptest.NewRecorder(), stats: stats}
}

// WriteString 必须显式转发到 Write：ResponseRecorder 自带 WriteString，不覆盖就会绕开计数。
func (w *smokeWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *smokeWriter) Write(p []byte) (int, error) {
	// 永远返回“全部收下”：过滤层不改变被观察代码的控制流。
	w.pending = append(w.pending, p...)
	if len(w.pending) > maxPendingBytes {
		w.pending = nil
		return len(p), nil
	}
	for {
		index := bytes.IndexByte(w.pending, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimSuffix(string(w.pending[:index]), "\r")
		w.pending = append([]byte(nil), w.pending[index+1:]...)
		// 只接受 `data:` 行；原始 JSON 错误体等其他行直接忽略。
		if strings.HasPrefix(line, "data:") {
			if payload := strings.TrimSpace(strings.TrimPrefix(line, "data:")); payload != "" {
				w.stats.observe(payload)
			}
		}
	}
	return len(p), nil
}

// runGeneration 是唯一的重活入口：正式加载配置 → 装配正式依赖 → 发起一次逻辑生成。
// 刻意不调用 wire 生成的 initializeApplication（会拉起 HTTP 服务/定时 worker/调度器），
// 也不调用 repository.InitEnt（会执行迁移、补启动密钥、补默认分组等启动期写入）。
func runGeneration(ctx context.Context, _ options, stats *eventStats) error {
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		return newStageError(stageConfigLoad)
	}
	if err := timezone.Init(cfg.Timezone); err != nil {
		return newStageError(stageEnvironment)
	}
	// 数据库只做 InitEnt 中“能读”的那一段：正式 DSN + 正式驱动构造 + ProvideSQLDB + Ping。
	driver, err := entsql.Open(dialect.Postgres, cfg.Database.DSNWithTimezone(cfg.Timezone))
	if err != nil {
		return newStageError(stageDatabase) // DSN/驱动错误可能内嵌 password=，不外传原文
	}
	entClient := dbent.NewClient(dbent.Driver(driver))
	defer func() { _ = entClient.Close() }() // 一并关闭底层 *sql.DB
	db, err := repository.ProvideSQLDB(entClient)
	if err != nil {
		return newStageError(stageDatabase)
	}
	db.SetMaxOpenConns(2) // 冒烟是串行短流程，不占线上按并发调优的连接数
	if err := db.PingContext(ctx); err != nil {
		return newStageError(stageDatabase)
	}
	rdb := repository.ProvideRedis(cfg)
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(ctx).Err(); err != nil {
		// Redis 不可达时令牌/调度缓存会在生成中途以自由文本错误炸掉，这里先行判空成有限 stage。
		return newStageError(stageRedis)
	}

	settingService := service.NewSettingService(repository.NewSettingRepository(entClient), cfg)
	if err := settingService.LoadAntigravityModelEffortSettings(ctx); err != nil {
		return newStageError(stageSetting) // effort 策略是包级快照，必须在生成前加载
	}
	if err := settingService.LoadAntigravityThoughtPolicySettings(ctx); err != nil {
		return newStageError(stageSetting)
	}
	accountRepo := repository.NewAccountRepository(entClient, db, repository.ProvideSchedulerCache(rdb, cfg))
	tokenCache := repository.NewGeminiTokenCache(rdb)
	// 该 service 内部有一个会话清理 ticker goroutine，退出前必须 Stop。
	oauthService := service.NewAntigravityOAuthService(repository.NewProxyRepository(entClient, db))
	defer oauthService.Stop()
	tokenProvider := service.ProvideAntigravityTokenProvider(
		accountRepo, tokenCache, oauthService,
		service.ProvideOAuthRefreshAPI(accountRepo, tokenCache), repository.NewTempUnschedCache(rdb),
	)
	httpUpstream := repository.NewHTTPUpstream(cfg)
	// 与线上 wire_gen 的差异（都不启动后台活动）：schedulerSnapshot / rateLimitService /
	// internal500Cache 传 nil —— 网关对三者都判空，缺失只让失败路径少一层惩罚/计数；
	// gemini/claude/grok token provider 传 nil —— 本工具只走 Antigravity 账号路径。
	// settingService 与 tlsFingerprintProfileService 必须是真的：网关直接解引用 cfg，
	// 测试服务直接调用 ResolveTLSProfile。
	gatewayService := service.NewAntigravityGatewayService(
		accountRepo, repository.NewGatewayCache(rdb), nil, tokenProvider, nil, httpUpstream, settingService, nil,
	)
	tlsFPProfileService := service.NewTLSFingerprintProfileService(
		repository.NewTLSFingerprintProfileRepository(entClient), repository.NewTLSFingerprintProfileCache(rdb),
	)
	accountTest := service.NewAccountTestService(accountRepo, nil, nil, nil, gatewayService, httpUpstream, cfg, tlsFPProfileService)
	accountTest.SetSettingService(settingService) // prompt 留空时由全局 model_test_prompt 生效

	ginCtx, _ := gin.CreateTestContext(newSmokeWriter(stats))
	target := "/api/v1/admin/accounts/" + strconv.FormatInt(fixedAccountID, 10) + "/test"
	ginCtx.Request = httptest.NewRequest(http.MethodPost, target, nil).WithContext(ctx)
	err = accountTest.TestAccountConnection(ginCtx, fixedAccountID, fixedModel, "", service.AccountTestModeDefault,
		service.AccountTestOptions{ReasoningEffort: fixedEffort})
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// 取消/超时在库侧也被拼成自由文本错误，只能靠上下文判断。
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return newStageError(stageTimeout)
		}
		return newStageError(stageCanceled)
	}
	if err.Error() == accountMissingText {
		return newStageError(stageAccountNotFound)
	}
	return newStageError(stageGenerationFailed)
}

// silenceLibraryOutput 把进程级日志出口全部改接到 os.DevNull，并返回原始 stdout/stderr 句柄：
// stdout 只写汇总，stderr 只写本工具自己的有限 stage 诊断。
//
// 被复用代码会原样打印上游错误体与令牌回显（antigravity_oauth_service / refresher 的
// fmt.Printf）、带 DSN 的数据库与配置告警（log / slog），gin 也有自己的 writer 变量（包初始化时
// 抓走当时的 os.Stdout），因此四个出口都必须显式接走而不是转发。打不开 devNull 时失败关闭：
// 拿不到静音出口就无法保证上面的不泄露约定。
func silenceLibraryOutput() (stdout, stderr *os.File, devNull *os.File, err error) {
	devNull, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, nil, nil, newStageError(stageLibrarySilence)
	}
	stdout = os.Stdout
	stderr = os.Stderr
	os.Stdout = devNull
	os.Stderr = devNull
	// 两行都要接 devNull：slog.SetDefault 会把标准库 log 的输出换成指向自己 handler 的
	// slog.handlerWriter，因此它同时也是 log 的出口。
	log.SetOutput(devNull)
	slog.SetDefault(slog.New(slog.NewTextHandler(devNull, nil)))
	gin.DefaultWriter = devNull
	gin.DefaultErrorWriter = devNull
	gin.SetMode(gin.ReleaseMode)
	return stdout, stderr, devNull, nil
}
