# Sub2 7c12 本地候选载体

独立单活载体已准备；镜像构建停在本机 Docker engine 准入。`desktop-linux` context 已确认，`docker info` 未返回 server，单次有界构建入口在 15 秒 engine preflight 超时退出 2；build/save/load/create 都未执行。未重启 Docker，也未进行远端 stage、写入、启停或模型请求。

## 精确来源与载体

产品源码仍为 `7c12de7380be0142e18bcdc6397b6537f2af9968`，直接父为 `b0e0fb9e671c0180d8e39c9ac5dffebdbc943bd9`；包含原两窄修。不使用带额外 Task88 五路径的父主线 `78acffc48`。本轮只增加 `.agent/release/argon502-20261107/` 和本报告，不改产品。

新远端 root `/opt/sub2api-argon502-20261107`、unit 前缀 `sub2-argon502-`、候选 label `sub2.argon502.run`；保留旧 root/run/恢复入口，继续共用 `/run/lock/sub2-gcp-app-switch.lock`。`release.py` 从已验 f183 生命周期冻结，仅改源码/image/旧精确运行身份、独立命名及 Env 规则。候选 Env 逐项同序等于原 Env，且原 Env 必须恰有一次已启用的 `GATEWAY_STREAM_PREHEADER_KEEPALIVE_ENABLED=true`，不追加或修现场配置。A=true/B=false/C=false 由父切前切后通过管理 API/运行快照读回；当前 73 可调度，不改调度。

旧基线按用户输入绑定为容器 `b7df1c4ed5b02e8b6a8d3f1999d7d2eb368f76b3bb965f14aa6eb89d8d2de7f7`、runtime image `sha256:06ee81a40b6a67759d9d286a302b1ac25adf236f8bcbe623b2e33d34c20384f4`、source `f18388a440ddce04c5da0d0917ba2e4db04a5b0e`、StartedAt `2026-11-06T17:43:56.662935341Z`，正式 prepare 前父 fresh 复核。

## 验证

- 在新载体目录运行 `python -B -m unittest -v test_release`，11 tests，0.024s，exit 0。覆盖旧容器/时间/image 漂移拒绝、未绑定镜像拒绝、已有唯一保活 Env 原序保留、缺失/false/重复拒绝、候选和回退 Env 精确检查、source-check image/revision 与 app 三字段、独立 unit 和同脚本 recovery。
- 本轮直接比较：`ingress_gate.py` 与旧载体逐字节相等；除 `original_env`、`candidate_env`、`prepare`、`service_command` 外，所有生命周期函数/类 AST 与旧载体相等。沿用旧 receipt 的 Linux 16 项、Docker 三个生命周期场景和九个 TCP 样本、native bridge 证据，不把本次 Windows 差异测试写成重跑 Linux。
- 原产品 7c12 四协议/handler 与真实双 HTTP wire 的证据见 `argon-502-20261107/REPORT.md`、`evidence.json`。客户端 `e19fd64e9` 原 wire＋托管插件联测由父已接受，正式 build 由 b3 持有。
- 本次 engine 失败原件：task `bash-jymxmgt7`，`docker --context desktop-linux info --format {{json .}}` 15 秒超时。两条早期挂起的本 owner 只读探针已停止。没有产生候选 `.Image`、压缩包、大小或传输 SHA-256。

## 唯一依赖与下一动作

本机 Docker Linux engine 的 owner 恢复 API 响应后，由 Sub2 候选 owner 执行一次：

```sh
python -B .agent/release/argon502-20261107/build_candidate.py
```

入口沿旧四个 pinned base images，用精确 `git archive 7c12` 构建 `linux/amd64`，实际 save→gzip→load，再以 create 的未启动容器核 `.Image`＋OCI revision。产物为 `.tmp/argon502-release-20261107/sub2api-argon502-7c12de738-linux-amd64.tar.gz` 及 `candidate.json`。读回后绑定 `release.py:IMAGE` 与 receipt 并提交，将实际包、字节数、传输 SHA-256 和唯一 source/runtime-image pair 交父/244；当前 IMAGE 未绑定会阻止 prepare。

244 的新备份准入身份只能是 `source=7c12de7380be0142e18bcdc6397b6537f2af9968` 加上述 create 实测 runtime image ID。未来实际 app 的 `{id,name,started_at}` 由载体切换后读回、CAS 更新并 source-check。正式发布继续由父另安排，真实客户端切换完成后才消费本载体；完整操作及同 run 恢复见新目录 `RUNBOOK.md`。
