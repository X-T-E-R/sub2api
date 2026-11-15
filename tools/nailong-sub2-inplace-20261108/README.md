# Nailong Sub2API 蓝绿发布与回滚

`inplace_window.py` 先预热空闲槽，再切换 Nginx。两个槽的宿主机端口是 `127.0.0.1:17861` 和 `127.0.0.1:17862`；对外只有一个 `sub2api_active_slot` upstream。生产容器继续使用 `sub2api-gcp`，doctor 按容器的实际发布端口探测。

## 安装路由与发布

首次安装时，将 `sukaka`、`/__task49_personal_sub2/v1/messages` 和 Channel 50 都接到同一个 upstream，仍指向当前槽。Channel 50 使用 Nailong app bridge 的宿主 Nginx listener；两槽各自保留 app 网络和 DB/Redis 网络，不给备用槽分配 `sub2api-gcp` 别名。`slot_routing.install()` 和后续切换都持有 `/run/nailong-nginx-edit.lock`；不要在锁外读取、修改或重载 Nginx 配置。

1. 用 `git archive` 从指定提交构建 `linux/amd64` 镜像。记录提交、镜像 config ID 和 `io.sub2api.peer-safe-startup=true`。该标签只用于启动时不会删除其他进程 Redis 并发槽位的镜像。
2. 从 `go.template.json` 复制本次 GO，填写当前容器 ID、`StartedAt`、端口、依赖标识和目标镜像。旧的 stop-first GO 不适用。如果当前是没有安全启动标签的旧镜像，填入 `rollback_compatible`（原版本的安全启动重建镜像，字段为 `image`、`revision`）。
3. `python -B inplace_window.py stage-carrier --go <GO.json>` 安装 runner，并在 Nailong 与 lag 追加备份镜像准入；`python -B inplace_window.py ready --go <GO.json>` 核对当前容器、依赖、镜像及备份状态。
4. `python -B inplace_window.py forward --go <GO.json>` 启动宿主机 systemd 发布任务。它等待备用槽的 Docker health、`/health` 和 `/readyz`，再重载 Nginx，确认新 worker，重绑并校验备份 source。

`forward_terminal` 表示目标槽和备份已切换。独立的 `sub2-slot-retire-*` unit 等待旧 Nginx worker 退出后才发送 `SIGTERM`，不发 admission hold，也不断开任何网络。应用 Shutdown 预算为 300 秒，Docker stop timeout 为 360 秒；runner 不发送 `SIGKILL`。在窗口目录查看 `retirement-status-<id>.json` 确认旧槽是否已退出。切换前后只用健康检查和自然流量记录，不发送合成模型请求。

## 回滚

使用与发布相同的 GO，将 `action` 改为 `recover`，执行 `python -B inplace_window.py recover --go <recover-GO.json>`。仍在运行的原槽会直接复用；已退出的原槽先预热，再按同一路由流程切回。冷回滚旧镜像使用已准入的 `rollback_compatible`，避免原镜像启动时清空新槽的 Redis 并发记录。不要用 `docker stop` 或手工重启替代此入口。

首次上线可在 GO 中设置 `verify_rollback: true`，在两槽仍热时执行一次来回切换并保留 `rollback-verified.json`；最终继续使用目标槽。切换完成后核对 doctor、`source-check`、`sub2-core-backup.timer` 和窗口内的 HTTP 状态计数。

## 离线回归

```text
python -B -m unittest discover -s tools/nailong-sub2-inplace-20261108 -p "test*.py"
```
