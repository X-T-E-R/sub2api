# Gemini 临时会话取证（默认关闭）

这是一次性故障取证开关，不是通用 body 日志。缺少配置、lease 文件不存在、lease 无效、过期或 `enabled:false` 时不会落盘，也不会改变转换、重试、路由或 usage 语义。

## 部署入口

设置配置键或等价环境变量（配置键优先）：

```yaml
gateway:
  gemini_capture_lease_file: /app/data/gemini-capture/lease.json
```

```bash
GATEWAY_GEMINI_CAPTURE_LEASE_FILE=/app/data/gemini-capture/lease.json
```

生产建议复用现有 `/app/data` bind：先由 UID/GID `1000:1000` 的 `sub2api` 创建并设置 `/app/data/gemini-capture` 为 `0700`；lease 文件 `0600`，父目录不要让其他用户可读。输出目录也必须是绝对路径、已存在、目录权限 `0700`，例如 `/app/data/gemini-capture/output`。进程只读 lease，不创建或修改 lease 文件。

## Lease JSON

所有字段均为必需条件；任一不满足即关闭采集。`metadata_user_ids` 是逐字精确 allowlist，不做前缀、group、account 或 agent 扩大；请求路径严格为 `/v1/messages`，请求模型和最终映射模型严格为 `gemini-3.8-flash`。

```json
{
  "enabled": true,
  "metadata_user_ids": ["<exact metadata.user_id>"],
  "model": "gemini-3.8-flash",
  "expires_at": "2026-10-09T20:00:00Z",
  "output_dir": "/app/data/gemini-capture/output"
}
```

`expires_at` 只要求是未来 RFC3339/RFC3339Nano 时间；没有内置 10 分钟或响应条数上限。需要延长时原子替换 lease 并续租；需要停用时原子替换为 `enabled:false` 或删除文件。推荐在同一目录写临时文件后 `chmod 0600`、`rename(2)` 替换，避免读到半个 JSON。已在飞的记录在 disarm/过期后停止继续写入，并在 manifest 标为 `incomplete`。

## Artifact

每个匹配请求写一个 `gemini-<uuid>/`（目录 `0700`，文件 `0600`）：

- `manifest.json`：schema、请求/client/request ID、metadata.user_id、用户/组/账号非 secret ID、模型、attempt 数、每次 attempt 的 status/response ID/结果、原始 `finishReason` 集合、usage、EOF/read_error/client_disconnect/timeout/ctx_cancel、权限/容量失败和 `incomplete` 原因。
- `inbound.bin`：匹配后、rewrite 前的完整 inbound body。
- `gemini_request.bin`：最终 Claude→Gemini 转换 body。
- `attempts/NNN/request.bin`：该 HTTP attempt 实际发送的 Gemini wrapper body。
- `attempts/NNN/upstream.bin`：按实际读取顺序的原始 upstream bytes，保留 CRLF/LF、注释和空行。
- `converted.bin`：实际交给客户端 writer 的转换后 bytes（包含 partial/write failure 信息）。非流式路径同样记录最终 JSON body。

只记录上述 body/bytes，不记录任何 HTTP request/response headers；Auth、cookie、API key、secret 等 JSON 字段替换为 `[REDACTED]` 并在 manifest 标明。provider `thoughtSignature`/其他 opaque 签名不是 auth token，会保留。单请求 inbound 截取上限为 64 MiB，异步写队列上限为 4 MiB，单请求累计 artifact 写入上限为 128 MiB；同一 output 目录（含已有私有原件与并发请求）共享 512 MiB 总预算，parser carry 上限为 1 MiB。任一上限、redaction 解析失败或磁盘/权限失败都会停止对应采集并标 `incomplete`，不会把改写结果伪称完整。收尾 drain、文件同步和 manifest 写入均有界，超时同样标记失败而不阻塞业务响应。

关闭/恢复时由外部 controller 独立 disarm（写 `enabled:false` 或移除 lease）并恢复/确认空的 `GATEWAY_GEMINI_CAPTURE_LEASE_FILE` 配置；私有 artifact 保留在原 output 目录，不随 disarm、配置恢复或服务启停自动删除。本模块不执行部署、启停或消费动作。
