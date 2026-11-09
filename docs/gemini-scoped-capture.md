# Gemini 临时会话取证（默认关闭）与内建搜索模型配置

这是一次性故障与对照实验取证开关，不是常驻全量日志。缺少配置、lease 文件不存在、lease 无效、过期或 `enabled:false` 时不会落盘，也不会改变转换、重试、路由或 usage 语义。

## 1. Antigravity 内建搜索目标模型配置 (`antigravity_web_search_model`)

当 `/v1/messages`（或对应兼容/v1internal 路径）请求显式携带内建搜索工具（`web_search` / `web_search_20250305` / `google_search`）时，`TransformClaudeToGeminiWithOptions` 将请求 `requestType` 设为 `"web_search"`，并将目标 wire `model` 切换为搜索模型；**未携带内建搜索工具的普通请求（如常规 Argon 请求）保持原模型与 `requestType="agent"` 不变**。

- **设置键**：`antigravity_web_search_model`（通过 `GET/PUT /api/v1/admin/settings` 管理，网关热路径走 `SettingService` 内存原子快照，零数据库查询）。
- **默认回退**：未配置或设为空字符串 `""` 时，保持旧默认 `"gemini-2.5-flash"`。
- **启用 `gemini-3.1-flash-lite`（partial PUT）**：
  ```json
  {
    "antigravity_web_search_model": "gemini-3.1-flash-lite"
  }
  ```
- **关闭/恢复默认（partial PUT）**：
  ```json
  {
    "antigravity_web_search_model": ""
  }
  ```

## 2. Capture 部署入口

设置配置键或等价环境变量（配置键优先）：

```yaml
gateway:
  gemini_capture_lease_file: /app/data/gemini-capture/lease.json
```

```bash
GATEWAY_GEMINI_CAPTURE_LEASE_FILE=/app/data/gemini-capture/lease.json
```

生产建议复用现有 `/app/data` bind：先由 UID/GID `1000:1000` 的 `sub2api` 创建并设置 `/app/data/gemini-capture` 及 `/app/data/gemini-capture/output` 为 `0700`；lease 文件 `0600`。输出目录必须是绝对路径、已存在、目录权限 `0700`。进程只读 lease，不创建或修改 lease 文件。

## 3. Lease JSON

请求路径严格为 `/v1/messages`，且必须激活到 Antigravity OAuth 账号才会落盘。

- **模型匹配（必填 `model` 或 `models` 其一）**：
  - 支持单个 `model`（兼容旧 lease）或 `models` 数组（如 `["gemini-4-argon", "gemini-3.1-flash-lite"]`），支持精确模型名、`-low`/`-medium`/`-high` effort 变体及 `*` 通配。
  - 当请求因携带内建搜索工具在转换后切换了 wire `model`（例如从 `gemini-4-argon-high` 或 `claude-sonnet-4-6` 切换为 `gemini-3.1-flash-lite`），只要入站 `request_model`、账号映射 `mapped_model` 或最终 wire `final_model` 命中 lease 模型列表，即允许激活并记录实际 wire 模型。
- **身份过滤（可选，交集匹配）**：
  - `metadata_user_ids`：可选。提供非空数组时逐字精确匹配 `metadata.user_id`（保持旧 lease 行为）；省略或为空时允许任意 session（含不固定 `metadata.user_id` 的子 agent）。
  - `api_key_ids`：可选。提供非空整数数组时仅匹配指定网关 `api_key.id`。
  - `user_ids`：可选。提供非空整数数组时仅匹配指定网关 `user.id`。

示例 1（覆盖本机 Argon 与 `gemini-3.1-flash-lite` 内建搜索实验，无需为每个子 agent 指定 `metadata_user_ids`）：

```json
{
  "enabled": true,
  "models": ["gemini-4-argon", "gemini-3.1-flash-lite", "gemini-2.5-flash"],
  "expires_at": "2026-11-10T20:00:00Z",
  "output_dir": "/app/data/gemini-capture/output"
}
```

示例 2（兼容旧版单模型 + `metadata_user_ids` 精确会话租约）：

```json
{
  "enabled": true,
  "metadata_user_ids": ["<exact metadata.user_id>"],
  "model": "gemini-3.8-flash",
  "expires_at": "2026-11-10T20:00:00Z",
  "output_dir": "/app/data/gemini-capture/output"
}
```

`expires_at` 要求是未来 RFC3339/RFC3339Nano 时间。需要延长时原子替换 lease 并续租；需要停用时原子替换为 `enabled:false` 或删除文件（推荐在同一目录写临时文件后 `chmod 0600`、`rename(2)` 替换）。已在飞的记录在 disarm/过期后停止继续写入，并在 manifest 标为 `incomplete`。

## 4. Artifact 与业务内容保护边界

每个匹配请求写一个 `gemini-<uuid>/`（目录 `0700`，文件 `0600`）：

- `manifest.json`：schema、请求/client/request ID、`metadata_user_id`、`user_id`/`api_key_id`/`group_id`/`selected_account_id`、入站 `request_model`、账号映射 `mapped_model`、最终实际 wire `final_model`、wire `wire_request_type`（`agent` 或 `web_search`）、attempt 数、每次 attempt 的 status/response ID/结果、原始 `finishReason` 集合、usage、EOF/read_error/client_disconnect/timeout/ctx_cancel、权限/容量失败和 `incomplete` 原因。
- `inbound.bin`：匹配后、rewrite 前的完整入站请求字节。
- `gemini_request.bin`：最终 Claude→Gemini 转换后字节。
- `attempts/NNN/request.bin`：该 HTTP attempt 实际发送的 Gemini wrapper 字节。
- `attempts/NNN/upstream.bin`：按实际读取顺序的原始 upstream 响应字节（含空 candidate/`groundingMetadata`/错误体），保留 CRLF/LF、注释和空行。
- `converted.bin`：实际交给客户端 writer 的转换后字节。

**业务内容不脱敏与认证隔离**：
- 绝不记录任何 HTTP request/response headers（如 `Authorization`、`x-api-key`、`Cookie`）。
- 业务内容容器（`messages`、`system`、`tools`、`tool_choice`、`thinking`、`output_config`、`contents`、`systemInstruction`、`toolConfig`、`generationConfig`、`candidates`、`delta`、`content_block`、`groundingMetadata`）及 `thoughtSignature` 完整原样保字节，不因业务参数或工具输出含有 `token`/`secret`/`api_key`/`password` 等键名而误脱敏或重序列化。
- 顶层/外层真实凭据字段（如顶层 `accessToken`、`refreshToken`、`api_key`、`clientSecret`）仍替换为 `[REDACTED]` 并在 manifest 标明 `credential_redaction_applied`。
- 预算与完整性：单请求 inbound 上限 64 MiB，异步写队列上限 4 MiB，单请求累计写入上限 128 MiB，同一 output 目录共享 512 MiB 总预算，parser carry 上限 1 MiB。任一上限或写入失败均停止对应采集并在 manifest 诚实标记 `incomplete: true`。
