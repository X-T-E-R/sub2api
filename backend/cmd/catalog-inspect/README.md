# catalog-inspect

一次性、有界的 Antigravity 模型目录取数工具。它复用线上正式装配（正式配置加载器、
Ent 账号仓储、`AntigravityTokenProvider`/OAuth 服务/`antigravity.Client`），对固定账号
集合各发起一次逻辑 `fetchAvailableModels`，把结果投影成 stdout 上的单个 JSON 文档，
用于确认上游真实返回了哪些模型、哪几个能力字段、以及实际命中的基础 URL。

它不启动 HTTP 服务、不注册任何后台 worker、不执行迁移、不做重试、不取配额汇总。

## 构建与运行

```bash
cd backend
go build -o /tmp/catalog-inspect ./cmd/catalog-inspect

# 配置文件：正式加载器按 CONFIG_FILE → DATA_DIR → /app/data → ./ → ./config → /etc/sub2api 顺序查找
CONFIG_FILE=/path/to/config.yaml /tmp/catalog-inspect -accounts 63,64,69

# 或直接用 -config 覆盖 CONFIG_FILE
/tmp/catalog-inspect -config /path/to/config.yaml -accounts 63,64,69
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-accounts` | `63,64,69` | 固定账号集合 `{63,64,69}` 的非空子集；去重、输出恒按 `63,64,69` 顺序，集合外/非正整数/空列表一律失败关闭 |
| `-timeout` | `90s` | 整轮巡检的整体超时（每个 HTTP 客户端另有 10s 超时，请求路径上的令牌刷新另有 8s 上限） |
| `-config` | 空 | config.yaml 路径，非空时写入 `CONFIG_FILE` |
| `-h` | — | 打印用法（退出码 0，不产生 stdout 输出） |

退出码：`0` 全部账号成功（含显式 `-h`）；`1` 报告已输出但至少一个账号失败；
`2` 无法产出报告（参数、配置、数据库）。`2` 时 stdout 为空，原因是一个有限 stage。

## stdout 契约

stdout 上只有这一个 JSON 文档，可以直接管道给 `jq`。

```json
{
  "generated_at": "2026-10-11T09:30:00Z",
  "source": "antigravity.fetchAvailableModels",
  "accounts": [
    {
      "account_id": 63,
      "fetched_at": "2026-10-11T09:30:01Z",
      "endpoint": "https://cloudcode-pa.googleapis.com",
      "configured_endpoints": ["https://cloudcode-pa.googleapis.com", "https://daily-cloudcode-pa.googleapis.com"],
      "model_count": 2,
      "model_ids": ["gemini-2.5-flash", "gemini-3-pro"],
      "models": [
        {
          "id": "gemini-3-pro",
          "display_name": "Gemini 3 Pro",
          "supports_images": true,
          "supports_thinking": true,
          "thinking_budget": 32768,
          "recommended": true,
          "max_tokens": 1048576,
          "max_output_tokens": 65536,
          "supported_mime_types": ["image/png", "text/plain"],
          "quota_remaining_fraction": 0.85,
          "quota_reset_time": "2026-10-11T10:00:00Z"
        }
      ],
      "deprecated_model_ids": { "gemini-2.0-flash": "gemini-2.5-flash" },
      "error": null
    }
  ]
}
```

- `endpoint` 是客户端实际使用的基础 URL（成功路径由 `FetchAvailableModelsResponse.QuotaBaseURL`
  直接给出）。当上游第一个地址失败并降级到下一个时，`endpoint != configured_endpoints[0]`。
- 失败时 `endpoint` 为空：客户端不暴露“最后尝试了哪个地址”，因此只报告
  `configured_endpoints` 的配置顺序。这一条属于上游 API 的缺口，见下节。
- 输出**不含**原始响应、token、project、email、请求头。
- 指针类字段（`supports_images` 等）缺失表示上游未给该字段，`false` 表示上游显式给了 false。
- `supported_mime_types` 只列上游显式标记为 `true` 的类型；`quota_*` 来自模型的 `quotaInfo`。
- `model_count == 0` 且 `error == null` 是合法观察：上游确实返回了空目录。
- 每账号只调用一次逻辑 `FetchAvailableModels`；客户端在内部按 `configured_endpoints` 顺序降级，
  所以一次逻辑调用可能对应 1~2 个 HTTP 请求。

## 失败信号：有限 stage

`error` 恒存在（成功为 `null`），形如 `{"stage": "...", "http_status": 403}`。`http_status`
只在能从错误里精确提取到 `(HTTP <ddd>)`（100..599）时出现。**不输出任何自由文本错误**：
上游错误体、令牌回显、带 DSN 的数据库错误都没有出口。

| stage | 含义 |
| --- | --- |
| `invalid_arguments_account_list_empty` | `-accounts` 为空 |
| `invalid_arguments_account_id_invalid` | 账号 ID 非正整数 |
| `invalid_arguments_account_id_outside_fixed_set` | 出现 `{63,64,69}` 之外的 ID（整体失败，不静默丢弃） |
| `config_load_failed` | 配置加载失败 |
| `timezone_init_failed` | 时区初始化失败 |
| `database_open_failed` | 驱动打开 / 取底层 `*sql.DB` 失败 |
| `database_unreachable` | `PingContext` 不可达 |
| `library_output_silence_failed` | 打不开 `os.DevNull`，无法保证不泄露约定 |
| `report_encode_failed` | JSON 编码失败 |
| `account_not_found` | 账号行不存在 |
| `account_load_failed` | 读取账号行失败 |
| `not_antigravity_account` / `not_oauth_account` | 账号平台/类型不符 |
| `token_unavailable` | 取 access token 失败（可能带 `http_status`，例如 invalid_grant=400） |
| `token_missing` | token provider 返回空 token 或空快照 |
| `client_setup_failed` | 客户端构造失败（如代理 URL 非法） |
| `fetch_forbidden` | 上游 403（`http_status=403`） |
| `fetch_failed` | 其他取数失败（可带 `http_status`） |
| `empty_response` | 客户端返回空响应 |
| `unknown_failure` | 未归类的兜底 |

## 日志静音

被复用代码会把上游错误体、令牌回显、凭据 DSN 原样打印（`antigravity_oauth_service`、
`antigravity_token_refresher` 的 `fmt.Printf`；`antigravity` 客户端、时区初始化的
标准库 `log`；provider / `OAuthRefreshAPI` / 配置校验的 `slog`）。因此启动后立刻把
三个出口全部接到 `os.DevNull`：

1. `os.Stdout`（`fmt.Printf` 读取它）
2. 标准库 `log`（`log.SetOutput`）
3. 默认 `slog`（`slog.SetDefault`；它同时会把标准库 `log` 的 writer 换成指向自己 handler 的
   `slog.handlerWriter`，所以这两行互为保险）

只有两个句柄被保留给出口：原 stdout 写 JSON，原 stderr 写**本工具自己的**有限 stage 诊断
（含 `-h` 用法）。打不开 `/dev/null(NUL)` 时直接以 `library_output_silence_failed` 失败关闭。

已确认覆盖的机制：`fmt.Printf`、`fmt.Fprintf(os.Stderr, ...)`、标准库 `log.Printf`、
默认 `slog.Warn/Info/Error`，以及真实 `antigravity.Client` 在 URL 降级时打出的那条日志
（用本地 httptest 上游端到端验证）。

**已知残余缺口（未观察到，但结构上存在）**：任何绕过 `os.Stdout`/`os.Stderr` 变量、标准库
`log` 与默认 `slog` 的输出都不在覆盖范围内——例如某库在 `init()` 里先抓走当时的 `os.Stderr`
句柄再写它，或直接向 fd 1/2 写 syscall。对当前导入图未观察到这种写出：用真实二进制跑 `-h`
时 stdout 为 0 字节、stderr 只有本工具用法文本，说明没有 init 期写出者。另外
`internal/pkg/logger` 的 zap 输出只在 `logger.Init` 后生效，本工具从不调用它；即便将来被调用，
它也会抓到已经被改接到 `NUL` 的 `os.Stdout/os.Stderr` 句柄，仍然不会外泄。

## 写副作用

运行前必须知悉以下写入，全部来自被复用的正式路径：

1. **令牌正常刷新（必要写入）**：`GetAccessToken` 在剩余有效期 ≤3 分钟或 `expires_at` 缺失时进入
   刷新流程，执行器再按固定的 15 分钟窗口决定是否真正调用上游。真正刷新时，会把轮换后的
   `access_token`/`refresh_token`/`_token_version` 写回 `accounts.credentials`，并在 Redis 写
   `oauth:token:ag:v2:account:<id>` 与 `oauth:refresh_lock:ag:v2:account:<id>`；刷新失败时还会写
   `accounts.temp_unschedulable_until/reason` 与 Redis 临时不可调度键。
   取 token 走 `GetAccessTokenForQuota`，因此**不会**触发 project 发现的额外上游调用与回填写入。
2. **数据库只读打开**：`openCatalogDB` 只复用 `cfg.Database.DSNWithTimezone`（正式 DSN）、
   `internal/pkg/timezone.Init`（正式时区初始化）、`repository.ProvideSQLDB` 与
   `entsql.Open(dialect.Postgres, ...)` + `ent.NewClient`（与 `InitEnt` 相同的驱动构造），
   然后做一次 `PingContext`。**不调用** `repository.InitEnt`，因此不执行迁移
   (`applyMigrationsFS`)、不补齐启动密钥、不补 SIMPLE 模式默认分组。连接池另设上限 2，
   不继承线上按并发调优的池参数（`applyDBPoolSettings` 未导出）。
3. **Redis 写**：见第 1 条。Redis 不可用时相关调用按正式代码降级（无分布式锁、跳过缓存），
   不会因为缓存失败而中断巡检。
4. **会话 ticker**：`service.NewAntigravityOAuthService` 内部会起一个 5 分钟会话清理 goroutine，
   已用 `oauthService.Stop()` 显式关闭；这是复用正式 service 无法避免的，不构成后台 worker。

## 已覆盖的检查

```bash
go test ./cmd/catalog-inspect      # 32 个测试函数 / 60 个执行用例
go vet ./cmd/catalog-inspect
golangci-lint run ./cmd/catalog-inspect   # 仓库 .golangci.yml（CI 同版 v2.13）
```

- 排序与投影：模型 ID 排序在 128 次新 map 迭代下稳定（不被 Go map 随机序掩盖）；指针字段
  “缺失/显式 false” 保持区分；mime 类型与 quota 字段投影正确。
- 单次调用：每账号恰好一次 token 获取与一次逻辑取数；平台/类型不符时在取数前即拒绝。
- 端到端网络路径：用真实 `antigravity.Client` 对着本地 httptest 上游跑通成功、URL 降级
  （`endpoint` 等于实际服务地址，被拒端点恰好收到一次请求）、403 失败体只留下
  `fetch_forbidden` + `http_status=403`。
- 有限失败信号：403 响应体 / 令牌错误 / 代理配置错误 / 空响应 / 空 token 都只映射到枚举 stage；
  从含 token、DSN、email、project 的错误文本里只提取 100..599 的状态码。
- 账号集合失败关闭：集合外 ID、非正整数、空列表整体 exit 2 且 stdout 为空。
- 流纯度：`fmt.Printf`、`fmt.Fprintf(os.Stderr, ...)`、标准库 `log`、默认 `slog` 四个出口都被丢弃；
  真实客户端的降级日志也被丢弃；JSON 之外没有任何 stdout 内容；stderr 上只有本工具的有限 stage 行。
- 不打印原始错误：`-config <不存在>` 时 stderr 恰为 `catalog-inspect: fatal: config_load_failed`，
  不含路径、不含 `open `、不含 `config.yaml`；真实数据库不可达（配置里带 password）时 stderr 恰为
  `catalog-inspect: fatal: database_unreachable`，不含 host/port/password。
- stage 枚举自检：20 个 stage 取值唯一且为 snake_case（防止复制粘贴出重复值让枚举失去判别力）。

未在本工具内执行的部分：本租约没有连接任何真实数据库/Redis，也没有向真实上游取数，
因此“配置→DB→账号行→token”真实环境链路只做到编译期与替身验证，端到端真跑由运维执行。

## 未做的相邻改动（需上游 API 扩展，本工具不改 `pkg`/`service`）

- 失败路径缺少“最后尝试的地址”信息。若要精确报告失败时的实际端点，需要在
  `internal/pkg/antigravity.Client.FetchAvailableModels` 上返回带尝试地址的类型化错误
  （或提供只读访问器）。当前实现诚实报告配置顺序，不猜测实际地址。
- `repository` 只导出会执行迁移/密钥补齐的 `InitEnt`，没有只读打开入口。若在 repository 侧
  导出 `OpenEntWithoutMigrations`（或把 `applyDBPoolSettings` 一并导出），本工具可直接复用它，
  而不必自行拼 `entsql.Open` + `ent.NewClient`。
- `service.resolveModelsListReadLimit` 未导出，本工具按同样语义从
  `cfg.Gateway.ModelsListReadMaxBytes` / `config.DefaultModelsListReadMaxBytes` 重算读取上限。
- 失败时响应为 nil，无法观察“部分可用模型”。若需要部分结果，同样需要上游 API 扩展。
