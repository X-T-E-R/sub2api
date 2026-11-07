# Sub2 7c12 本地候选载体

精确产品源码 `7c12de7380be0142e18bcdc6397b6537f2af9968`（直接父 `b0e0fb9e671c0180d8e39c9ac5dffebdbc943bd9`）的单平台 `linux/amd64` 镜像已构建，并绑定到 `.agent/release/argon502-20261107/`。镜像包含原两窄修及 Argon 空流分类修复，构建上下文不含 `78acffc48` 的 Task88 五路径。

| 对象 | 本地实测 |
|---|---|
| tag | `sub2api-argon502:7c12de738` |
| 运行镜像 ID | `sha256:a09c7ecad5c7c722dddd3c925c0f8510a2cbafe57666f2a1f9875c2b47439f58` |
| 归档 | `E:/kiki-wt/sub2-stream-preheader-20261106/.tmp/argon502-release-20261107/sub2api-argon502-7c12de738-linux-amd64.tar.gz` |
| 字节数 | 44,404,242 |
| 传输 SHA-256 | `4b77e74788365e615df32676a62b4260007056e3a70044b74358027bcac0c3ea` |

`bash-26maixkb` exit 0：`desktop-linux` 29.8.1 / Linux x86_64 从 `git archive 7c12` 使用固定 base images 构建，实际 save→gzip→load。临时容器 `815fea588bb3df0e3ebe6be57b0b3a923a0078823c11481ac17c658f22a61cde` 保持 `created`、`Running=false`，读回 `.Image` 如上、OCI revision 精确 `7c12` 后删除。父独立核了本地 image ID、revision 和平台。早期 `bash-jymxmgt7` 的 engine 超时由父恢复；本轮候选 owner 未重启引擎。实际构建与验证原件在归档同目录 `candidate.json`，载体 [receipt.json](../release/argon502-20261107/receipt.json) 保留可移交的身份与校验。

## 单活与旧恢复隔离

新远端 root `/opt/sub2api-argon502-20261107`、unit 前缀 `sub2-argon502-`、候选 label `sub2.argon502.run`，共用原 `/run/lock/sub2-gcp-app-switch.lock`。父在 `2026-11-07T00:15:23Z` 只读核对：旧 b7df/06ee/f183 与 `2026-11-06T17:43:56.662935341Z` 的绑定仍一致、healthy，备份 timer 等待 18:30Z、service inactive。正式窗口再按 [RUNBOOK.md](../release/argon502-20261107/RUNBOOK.md) 核现场基线。

旧容器同时有 `sub2.stream_preheader.run=run-20261106T174243Z` 和 `argon.keepalive.run=run-20261105T211000Z`。新 payload 不复制这两个旧发布 owner 标签，避免旧恢复入口匹配到新容器。旧容器、旧恢复入口、`task46.run`、其它运行标签及配置保持；候选 Env 与旧 Env 同序完全相等，要求原来恰有一个启用的保活项。切换和回退仍用现库，父按现有管理 API/运行快照读回 A=true/B=false/C=false；当前账号 73 可调度，载体不改调度。

- 历史 owner 标签泄漏先在新 payload fixture 复现，去除这两个标签后，缓存 Python Linux 容器中 11 项定向测试通过（0.047s / exit 0）；补现场精确标签值及其它标签全等断言后，受影响的单例再验通过。
- `ingress_gate.py` 与旧载体逐字节相等；除 Env、prepare、payload 和 unit 这五个直接差异函数外，生命周期函数和类 AST 相等。复用旧 Linux 16 项、Docker 三场景/九 TCP 样本及 native bridge 证据。原产品四协议、计量和真实双 HTTP wire 见 `argon-502-20261107/REPORT.md`；客户端 `e19fd64e9` 联测已由父接受。

父已将实际 `7c12/a09c` pair 交 244 作唯一新增双端备份准入。真实 Kiki 客户端切换完成并获得正式窗口后，父校验传输 SHA-256、在目标机一次 load 并用未启动容器核对同一 pair，再用独立 run 操作。候选 owner 未在远端 stage、启停、写入或发模型请求。
