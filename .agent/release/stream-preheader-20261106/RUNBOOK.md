# Sub2：部署选择的头前 SSE

这是下一 epoch 的 Sub2 部署载体。候选 `SOURCE`（OCI revision）和 `IMAGE`（本地已保存/加载候选的 runtime ID）当前恢复为故意未绑定的占位符；在父提供并核对新的实际 pair 之前，不得执行 `prepare`、`launch` 或任何实际部署。此前的 `SOURCE=b4cdd97ec77dbd821e42f194ee80159d9d5910ce` 与 `IMAGE=sha256:9c16868f747946c0301a703cd6136b148e0b63aeef7c23e26cde8bca5e18052d` 已作废，禁止用于 `prepare`；本载体会以 `release_identity_unbound` 拒绝它们未被重新绑定的状态。此载体不拉取、不加载镜像、不发 provider 请求，也不改变产品源代码。

固定旧基线为 `BASE=16cd6eab05b4ef605d08d7980c2d2853741d5a17` 与 `OLD_IMAGE=sha256:a04689c655a9293b2805ab65e94c1c6439179c03fe4c5faa90b7a3e5d7094b73`。候选精确 pair 待父提供；已作废 pair 的本地归档和 receipt 只能作参考，不能作为本轮候选或生产证据。

候选容器的 `Env` 必须是旧 app 原始 `Env` 的同序列表，再追加且只追加：

```text
GATEWAY_STREAM_PREHEADER_KEEPALIVE_ENABLED=true
```

不改 `gateway.stream_keepalive_interval` 或任何回退 `Env`。若旧快照已经出现同名设置（无论值是什么），`prepare` 直接拒绝，避免重复配置；回退必须恢复旧快照的原始 `Env`。启用前必须同时满足：新的 Kiki consumer 已上线可验、244 完成 source-check receiver allowlist 前置项、远端 stage 已就绪且父明确批准 activation；这些前置项当前不在本载体中声称已完成。

## 安装与准备

把本目录的 `release.py` 和 `ingress_gate.py` 一起安装到目标 root 私有目录 `/opt/sub2api-stream-preheader-20261106/`，两个文件均为 0600。候选 pair 绑定前，`prepare` 必须因 `release_identity_unbound` 失败且不创建 run：

```sh
root=/opt/sub2api-stream-preheader-20261106
run="$root/run-$(date -u +%Y%m%dT%H%M%SZ)"
/usr/bin/python3 -B "$root/release.py" prepare --run-dir "$run"
```

候选 pair 绑定后，父只执行一次本地归档 load，并在未启动容器上核对现场 `.Image` 与 OCI revision；若任一不同，父先重绑候选，交 244 按同一 pair 复核后才能 `prepare`。`prepare` 本身不加载或传输镜像。它会记录旧 app、日备绑定、PG/Redis 和桥接身份，核对实际 Nginx worker 在宿主网络且 UID 33，确认日备 timer enabled/active、service inactive，执行旧 image/revision 与 `source.app` 三字段的严格 pair 核对，并用 `nft --check` 验整批规则；不创建或删除规则。

## 获准窗口执行

仅在新的 Kiki consumer live、244 allowlist 前置项、远端 stage 和父批准均已确认后运行：

```sh
/usr/bin/python3 -B "$root/release.py" launch --run-dir "$run"
unit="sub2-stream-preheader-${run##*/}.service"
/usr/bin/python3 -B "$root/release.py" status --run-dir "$run"
journalctl -u "$unit" -n 40 --no-pager
```

同一服务以单个事务安装两张仅本 run 拥有的临时表，读回精确规则，并用真实计数器核两路新连接都命中。`ingress_gate.py` 的 native bridge 规则保持原样：

| 入口 | 封口动作 |
|---|---|
| 公网 `sukaka`（含现有 Worker 直连） | `inet/output` 在 DNAT 之前，只向 UID 33 新建到 `127.0.0.1:17861` 的 TCP 连接发 RST；root `/health` 仍可通。 |
| Axon 私网回源 | `bridge/forward` 只丢同一应用 bridge `br-522fc2807c92` 上目的端口 8080 的新 SYN，不绑定容器 IP；bridge TCP reset 不使用。 |

两路已建立连接可继续完成；长流或连接池仍不关闭时，Sub2 自身 8080 在 135 秒期限内达不到连续三次零连接，切换取消并自动开门。日备 timer 只临时停用、不改 enabled；如果备份已开始，不停备份进程、不停应用。脚本不修改依赖网络或 PG/Redis，保留运行环境、挂载和账号 73 暂停调度；旧容器确认退出后才启动候选，始终保持单 active app。

## 终态、恢复与回退

- `verified` 且 `gate_families: []`：候选 app 健康，source-check 认实际候选 image/revision 与 `source.app` 三字段 pair，日备 timer 已恢复，门已自动解除。父验收必须同时检查直接 origin 与 Axon 私网路径；公网 origin health 是额外检查。本载体不发 provider 请求。
- `aborted` / `rollback_aborted` 且 `gate_families: []`：没有停 app，门已打开。明确得到 `rollback_aborted` 后，在新的获准窗口可重新提交 `launch-rollback`；程序不会重放未确认的回退。
- 进程中断或 launch ACK 丢失：用同一 `run` 查 `status`、新 unit 与 journal，不重发 `launch`。`ExecStopPost` 会恢复旧容器和实际日备绑定；如果候选 app 和日备绑定已提交，则只完成恢复 timer 和开门，不再切 app。`rolled_back` 且 `gate_families: []` 表示旧 app 及日备恢复，入口已打开。
- 若自动恢复报错、门仍在，检查日志，处理报出的健康、绑定或规则漂移；确认原 unit 已退出后，对同一 run 执行 `python3 -B "$root/release.py" launch-recover --run-dir "$run"`，按其返回 unit 查看进展。不要另开 run、手工删表或启动旧容器；候选未确认退出时不会起旧 app，也不发 SIGKILL。`private-old.json` 含运行配置，只留 root 私有目录，不传输或打印。
- 如果 `verified` 后需要业务回退，在获准窗口对同一 run 执行 `python3 -B "$root/release.py" launch-rollback --run-dir "$run"`。回退重新验证两路封口，恢复旧 app 后绑定当前启动时间，恢复旧 `Env`，再自动开门；使用当前 PG、Redis、appdata，不恢复快照。保留原容器、候选与 run 供父验收结束。

父验收至少保留以下两条 health 证据，并与 source-check receipt 一起对账：

```sh
# 直接 origin / 当前 active app
curl -fsS --max-time 5 http://127.0.0.1:17861/health
# Axon -> Sub2 私网路径
docker exec axonhub-gcp wget -q -O - -T 5 http://sub2api-gcp:8080/health
# 公开 origin（额外）
curl -fsS --max-time 10 https://sukaka.littleamgroup.online/health
systemctl show -p ActiveState,SubState,UnitFileState,NextElapseUSecRealtime sub2-core-backup.timer
```
