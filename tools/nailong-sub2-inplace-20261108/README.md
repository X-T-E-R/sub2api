# Nailong 原位发布与恢复

`inplace_window.py` 只切换 `sub2api-gcp`，保留旧容器、PostgreSQL、Redis 和 appdata。每次发布使用独立收据目录，其中放置与本次镜像匹配的 runner、`go.template.json`、`core_identity_candidate.py` 和明确授权的 GO 文件。

## 发布前检查

1. 用精确提交的 `git archive` 构建并加载目标镜像，核对 revision、linux/amd64 和 image config ID。
2. 在收据目录运行 `python -B inplace_window.py stage-carrier`，备份并安装 runner，向 Nailong 和 lag 的备份身份策略追加新版本/镜像对。旧准入对必须保留。runner 和原 unit 都先备份；仅将此 carrier unit 的 `TimeoutStartSec` 从 600 改为 0，并执行 `daemon-reload`。
3. 运行 `python -B inplace_window.py ready`，确认当前容器、依赖容器、备份 source-check、timer 和目标镜像符合模板。
4. 运行 `python -B inplace_window.py forward --go <本次-GO.json>`。操作由 Nailong 的 `sub2-nailong-inplace@.service` 独立完成；控制器断线不会中断窗口。

## 如何保护正在进行的请求

新版应用收到 `SIGUSR1` 后关闭新请求准入，但 `/health` 仍返回 200。runner 等待 `draining=true` 且 `in_flight=0` 后才发送 `SIGTERM`。180 秒内未排空或探测失败时发送 `SIGUSR2` 取消 hold，不停止应用。`/readyz` 在 draining 时返回 503。

旧版没有排空字段时，runner 最多采样 60 秒本机 Redis 的账号/用户活跃槽位、live 槽位和等待队列，归零即进入标准 Shutdown。HTTP keep-alive 空闲连接会由 Go Shutdown 关闭，不以原始 TCP 连接数归零作为门禁，也不安装防火墙规则。60 秒内槽位未归零时默认放弃停止；仅在本次 GO 明确带有 `allow_legacy_busy_after_wait: true` 的授权下，记录末次活跃数后继续切换。旧版只有 5 秒 Shutdown 预算，这个限时放行可能截断尚未完成的请求，不能作为新版日常发布的替代路径。

应用的 `SERVER_SHUTDOWN_TIMEOUT_SECONDS` 为 300，Docker `StopTimeout` 为 360。各个等待阶段有独立截止时间；systemd 不再用 600 秒外层期限打断自动恢复。runner 不发送 `SIGKILL`，不修改宿主防火墙。停止、启动、备份重绑定和终态证据写入远程及本机 `windows/<window>/`。

## 恢复到本次保留的旧容器

在同一个发布收据目录运行：

```text
python -B inplace_window.py recover --go <本次-recover-GO.json>
```

恢复使用相同的排空/空闲保护，先验证新容器已退出，才重命名并启动旧容器，随后把备份 source 绑定回实际容器 ID 和 StartedAt，并检查 source-check 与 timer。新版请求未排空时不停止候选；健康接口不可用时使用活跃槽位检查。无法停止候选时，恢复停止在 `recovery_requires_operator`，不会启动双活容器。

恢复后核对 `/health`、实际版本、旧版本准入和备份 source-check。不要用 `docker stop` 的默认 10 秒超时替代此入口，也不要向模型发送合成探针；线上流量验证只观察自然请求。

## 离线验证

```text
python -B -m unittest discover -s tools/nailong-sub2-inplace-20261108 -p "test*.py"
```
