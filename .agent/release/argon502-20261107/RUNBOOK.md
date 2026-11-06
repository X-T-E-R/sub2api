# Sub2 Argon502：f183 → 7c12 单活切换

本目录提供独立发布载体。产品源码固定为 `7c12de7380be0142e18bcdc6397b6537f2af9968`，直接父提交为 `b0e0fb9e671c0180d8e39c9ac5dffebdbc943bd9`，包含原两项窄修；不从带 Task88 五路径的 `78acffc48` 构建。

当前本机 `desktop-linux` 的 `docker info` 超时，尚未构建镜像。`release.py:IMAGE` 保持未绑定；此时 `prepare` 会拒绝。唯一构建依赖是本机 Docker Linux engine 恢复响应，由其 owner 处理，不自动重启。客户端原 wire＋托管插件联测已接受 `e19fd64e9`，正式 Kiki build／实际切换由 b3 和父持有。

## 本地完成候选

在本机 engine 恢复后，由 Sub2 候选 owner 执行一次：

```sh
python -B .agent/release/argon502-20261107/build_candidate.py
```

该命令以 `git archive 7c12de7380be0142e18bcdc6397b6537f2af9968` 为完整构建上下文，沿 f183 相同四个已固定 base image，单平台 `linux/amd64`、关闭 provenance/SBOM。成功后实际执行 `docker save → gzip → docker load`，创建但不启动临时容器，核对 `.Image`、OCI revision 和 created 状态后移除该容器。产物在 `.tmp/argon502-release-20261107/`：

- `sub2api-argon502-7c12de738-linux-amd64.tar.gz`：供父传输。
- `candidate.json`：实际 runtime image ID、源码、平台、字节数、传输 SHA-256 和 save/load/create 回执。

候选 owner 读回结果，将实际 `.Image` 绑定到本目录 `release.py:IMAGE` 及 `receipt.json` 后提交，交父和 244。244 只为这一组 **实际 runtime image ID＋完整 source revision** 增加备份准入；压缩包 SHA-256 只用于传输，不能代替运行身份。新容器 ID 和 StartedAt 在切换后产生，由载体按实际 `source.app.{id,name,started_at}` 三字段提交并 source-check。未得到本地 pair 前，不进行备份准入或远端 stage。

## 父切换前核对

父只在真实客户端切换完成且另获发布窗口后消费本载体。先 fresh 核以下旧基线与当前 source-check 一致；漂移时回候选 owner 更新绑定，不沿旧 run 强行执行：

| 对象 | 精确基线 |
|---|---|
| 旧容器 | `b7df1c4ed5b02e8b6a8d3f1999d7d2eb368f76b3bb965f14aa6eb89d8d2de7f7` |
| 旧 image runtime ID | `sha256:06ee81a40b6a67759d9d286a302b1ac25adf236f8bcbe623b2e33d34c20384f4` |
| 旧源码 | `f18388a440ddce04c5da0d0917ba2e4db04a5b0e` |
| 旧 StartedAt | `2026-11-06T17:43:56.662935341Z` |

候选 `Env` 与旧 app 的原始 `Env` **逐项同序相等**，不追加、不重排。旧环境必须已有且仅有一次 `GATEWAY_STREAM_PREHEADER_KEEPALIVE_ENABLED=true`；缺失、false 或重复时拒绝，不帮现场改配置。回退仍使用保存的原 `Env`。现库 A=true/B=false/C=false，由父切前及切后经原管理 API 和运行快照读回；载体不写这些设置。账号 73 当前可调度，载体不改账号调度、OAuth、Key、路由或任何数据。

原目录 `/opt/sub2api-stream-preheader-20261106`、已完成旧 run 及其恢复入口保留。新目录固定 `/opt/sub2api-argon502-20261107`，新 unit 前缀 `sub2-argon502-`，新 candidate label 为 `sub2.argon502.run`；仍共用原单活锁，不能并行切换。

## 获准后的 prepare 与 launch

父完成目标机归档校验、一次 load、不启动容器身份核对和双端备份准入后，将本目录 `release.py`、`ingress_gate.py` 安装到新的 root 私有目录，权限 0600。以下仅是正式窗口的操作入口，本轮本地准备不执行：

```sh
root=/opt/sub2api-argon502-20261107
run="$root/run-$(date -u +%Y%m%dT%H%M%SZ)"
/usr/bin/python3 -B "$root/release.py" prepare --run-dir "$run"
# prepare、备份身份和窗口获准后，仅 launch 一次：
/usr/bin/python3 -B "$root/release.py" launch --run-dir "$run"
unit="sub2-argon502-${run##*/}.service"
/usr/bin/python3 -B "$root/release.py" status --run-dir "$run"
journalctl -u "$unit" -n 40 --no-pager
```

`prepare` 核实际 image/source、精确旧容器/StartedAt、三字段日备绑定、原 Env、PG/Redis 健康及网络、timer enabled/active、backup service inactive、Nginx UID 33 和 `br-522fc2807c92`。它只做 `nft --check`，不封口或停 app。

`launch` 沿原 Linux 生命周期，单事务安装两路新连接封口并以实际计数器证明命中：公网 Nginx UID 33 到 loopback 17861 在 DNAT 前 RST；Axon 私网同 bridge 到 8080 的新 SYN 丢弃。既有连接可完成，Sub2 入站连接须在 135 秒内连续三次为零，否则取消切换并开门。只临时停止日备 timer，不改 enabled；备份已运行时不终止备份或 app。旧 app 确认退出后才启动候选，PG、Redis、appdata 原样保留。候选健康、Env、runtime 和 source-check 三字段相等后，恢复 timer 并解除本 run 的两张表。

父终验同时保留直接 origin 与 Axon 私网 health、source-check、timer 和 A/B/C 运行读回：

```sh
curl -fsS --max-time 5 http://127.0.0.1:17861/health
docker exec axonhub-gcp wget -q -O - -T 5 http://sub2api-gcp:8080/health
curl -fsS --max-time 10 https://sukaka.littleamgroup.online/health
systemctl show -p ActiveState,SubState,UnitFileState,NextElapseUSecRealtime sub2-core-backup.timer
```

## 同 run 恢复与回退

`verified` 且 `gate_families: []` 表示候选/source-check/timer 已闭合并开门。`aborted` 或 `rollback_aborted` 且空 gate 表示未停当前 app；明确得到 `rollback_aborted` 后，才可在新窗口对同一 run 再提交 `launch-rollback`。

进程中断或 launch/上游 ACK 未知时，先查同一 run 的 status、unit、journal，不重发。`ExecStopPost` 沿原状态日志恢复旧 app 和实际日备绑定；候选已提交时只恢复 timer/开门，不再切 app。恢复失败时保门，修报出的健康、绑定或规则漂移；确认原 unit 已退出后，用同一 run：

```sh
/usr/bin/python3 -B "$root/release.py" launch-recover --run-dir "$run"
```

候选已 verified 后的业务回退，另获窗口再运行：

```sh
/usr/bin/python3 -B "$root/release.py" launch-rollback --run-dir "$run"
```

回退仍封两路并 drain，先确认候选退出，再启动保留的 f183 容器；保最新 PG/Redis/appdata，不恢复数据库快照，日备绑定使用旧容器本次实际 StartedAt。未知结果不另开 run、不手工启动容器或删表。`private-old.json` 含运行配置，仅留 GCP root 私有目录，不传输或打印。
