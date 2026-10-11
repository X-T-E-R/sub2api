# model-test-smoke

对账号 **64 / gemini-3.8-flash / medium** 发起一次有界模型测试。复用正式配置、账号仓储、token provider、AccountTestService 和 Antigravity Forward；不启动 HTTP 服务或后台 worker，不执行迁移。

```bash
cd <worktree>/backend
go run ./cmd/model-test-smoke  # dry-run：不读配置、不连数据库或网络
CONFIG_FILE=/path/to/config.yaml go run ./cmd/model-test-smoke -execute
```

只有 `-execute`、`-timeout`（默认 90s，范围 >0 且 ≤120s）、`-config` 三个参数。目标写死，避免误测其他账号；prompt 留空，由当前全局 `model_test_prompt` 的有效值决定。只调用一次 `TestAccountConnection`，其正式网关内部仍可能按原策略重试 429/503。

## 输出与判断

stdout 只输出一个 JSON：请求/发送/上游回显模型、冲突标志、有效 prompt/effort、响应模式、文本 chunk/字符计数、首次文本与总耗时、`success` 和有限 `stage`。**不输出正文、工具载荷、签名、媒体、原始响应或自由文本错误。** 工具 JSON 载荷不计入文本计数。

- 退出码 0：dry-run、`-h` 或测试成功；成功汇总为 `stage: "ok"`。
- 退出码 1：已产出汇总，但生成失败/取消/超时或流未完成。
- 退出码 2：参数、配置、数据库、Redis 或输出失败；stderr 只有有限 stage，不泄露底层错误。

测试必须产生可用输出并到达正常终态才成功；模型缺省回显保持空，不从请求模型补造。

## 执行副作用

真实 `-execute` 会调用供应商。token 临近过期时，正式刷新链可能写入 `accounts.credentials` 和 Redis OAuth token/refresh lock；刷新失败可能写临时不可调度状态。账号仓储按正式 DSN 打开，仅做连接检查，不补启动密钥或默认分组。OAuth 清理 ticker 在退出时停止。

库的 stdout/stderr、log/slog 和 Gin 日志在配置加载前接到 `os.DevNull`，只保留安全汇总出口；dry-run 和 `-h` 不产生以上副作用。

```bash
go test ./cmd/model-test-smoke/ -count=1 && go vet ./cmd/model-test-smoke/
```
