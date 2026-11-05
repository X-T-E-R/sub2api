# GCP Sub2：16cd 保活窄切换

在获准窗口提交一次 `launch`。GCP 的 systemd 服务会关闭两条入口的新 TCP 连接，排净旧 Sub2，切换应用镜像并核对日备，然后自动开放入口。SSH 断开不用重发命令，也不用手工维持或解除封口。

候选只含 `2dc8640c1` 加 `16cd6eab0`，固定镜像为 `sha256:a04689c655a9293b2805ab65e94c1c6439179c03fe4c5faa90b7a3e5d7094b73`。镜像已在 GCP 加载并用未启动探针核过，不要再传输或加载。GCP 日备源端、lag 接收/恢复端已于 21:01Z 安装双镜像身份兼容，现役 `source-check` 已通过；无需重装。镜像包与验证细节见 [receipt.json](receipt.json)。

## 发起切换

把本目录的 `release.py` 和 `ingress_gate.py` 一起安装到 GCP root 私有 `/opt/sub2api-argon-keepalive-20261106/`，两个文件均为 0600。窗口内不并行重载 Nginx、重启 Axon、重建 bridge 或改日备。先准备一次新 run：

```sh
root=/opt/sub2api-argon-keepalive-20261106
run="$root/run-$(date -u +%Y%m%dT%H%M%SZ)"
/usr/bin/python3 -B "$root/release.py" prepare --run-dir "$run"
```

`prepare` 记录旧 app、日备绑定、PG/Redis 和桥接身份，确认实际 Nginx worker 在宿主网络且 UID 33，日备 timer enabled/active、service inactive；还以 `nft --check` 验整批规则，不创建或删除规则。拿到 `prepared=true` 后，在获准窗口运行：

```sh
/usr/bin/python3 -B "$root/release.py" launch --run-dir "$run"
unit="sub2-argon-${run##*/}.service"
/usr/bin/python3 -B "$root/release.py" status --run-dir "$run"
journalctl -u "$unit" -n 40 --no-pager
```

同一服务原子安装两张仅本 run 拥有的临时表，读回规则，并用真实计数器核两路新连接都命中：

| 入口 | 封口动作 |
|---|---|
| 公网 `sukaka`（含 Worker 原 Key6 直连） | `inet/output` 在 DNAT 之前，只向 UID 33 新建到 `127.0.0.1:17861` 的 TCP 连接发 RST；root `/health` 仍可通。 |
| Axon 私网回源 | `bridge/forward` 只丢同一应用 bridge `br-522fc2807c92` 上目的端口 8080 的新 SYN，不绑定容器 IP。GCP 支持 bridge DROP；其 bridge TCP reset 不可用。 |

两路已建立连接可继续完成；长流或连接池仍不关闭时，Sub2 自身 8080 在 135 秒排空期限内达不到连续三次零连接，切换取消并自动开门。日备 timer 只临时停用、不改 enabled；如果备份已开始，不停备份进程、不停应用。脚本不修改依赖网络或 PG/Redis，保留运行环境、挂载和账号 73 暂停调度；旧容器确认退出才启动新容器。

## 核终态与恢复

- `verified` 且 `gate_families: []`：新 app 健康，`source-check` 认实际新 app 镜像/revision，日备 timer 已恢复，门已自动解除。下方检查宿主、内网、公开 origin 后，父用原 Key6 发一次新建短 `gemini-4-argon-anti` POST Messages SSE；确认出词和正常终帧、Sub2 同 correlation 恰好一次完成、Axon 无同 ID。结果未知就按该 ID 对账，不重发。
- `aborted` / `rollback_aborted` 且 `gate_families: []`：没有停 app，门已打开。明确得到 `rollback_aborted` 后，在新的获准窗口可重新提交 `launch-rollback`；程序不会重放未确认的回退。
- 进程中断或 launch ACK 丢失：用同一 `run` 查 `status`、unit 与 journal，不重发 `launch`。`ExecStopPost` 会恢复旧容器和实际日备绑定；如果新 app 和日备绑定已提交，则只完成恢复 timer 和开门，不再切 app。`rolled_back` 且 `gate_families: []` 表示旧 app 及日备恢复，入口已打开。
- 若自动恢复报错、门仍在，检查日志，处理报出的健康、绑定或规则漂移；确认原 unit 已退出后，对同一 run 执行 `python3 -B "$root/release.py" launch-recover --run-dir "$run"`，按其返回 unit 查看进展。不要另开切换 run、手工删表或启动旧容器；候选未确认退出时不会起旧 app，也不发 SIGKILL。`private-old.json` 含运行配置，只留 GCP root 私有目录，不传输或打印。
- 如果 `verified` 后需要业务回退，在获准窗口对同一 run 执行 `python3 -B "$root/release.py" launch-rollback --run-dir "$run"`。回退会重新验证并自动关闭两路，恢复旧 app 后绑定当前启动时间、自动开门；使用当前 PG、Redis、appdata，不恢复快照。保留原容器、候选与 run 供恢复，直到验收结束。

```sh
curl -fsS --max-time 5 http://127.0.0.1:17861/health
docker exec axonhub-gcp wget -q -O - -T 5 http://sub2api-gcp:8080/health
curl -fsS --max-time 10 https://sukaka.littleamgroup.online/health
systemctl show -p ActiveState,SubState,UnitFileState,NextElapseUSecRealtime sub2-core-backup.timer
```
