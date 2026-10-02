# 阶段 3.3 发布前验证记录（2026-10-02）

验证提交：`e7e1319d04303a3a83551c63f3a273d440a93b0d`。构建时工作区干净。

## 已执行并通过

- `go test -p=4 ./... -count=1 -timeout=5m`
- `go test -race ./internal/hasoak ./cmd/ha-soak -count=20`
- `go vet ./...`
- `bash -n deploy/ha/build-bundle.sh deploy/ha/deploy-node.sh deploy/ha/test-load.sh deploy/ha/test-gate.sh`
- 使用非秘密占位变量执行 `docker compose -f deploy/ha/compose.yaml config --quiet`。
- `build-bundle.sh --arch amd64` 生成 Linux 发布目录；`shasum -a 256 -c SHA256SUMS` 全部通过。
- `file` 确认 `server`、`ha-soak`、`canary-gate` 均为静态链接 Linux x86-64 ELF。

此前全仓测试中的一小时窗口边界竞争已修复：到达截止时刻时显式检查时钟，避免仅依赖 Context 取消信号而额外接纳任务。上述重复竞态验证包含此回归。

## 构建信息

- 工具链：Go 1.26.8，构建主机 darwin/amd64。
- 目标：linux/amd64，CGO disabled。
- 清单时间：`2026-10-02T07:57:09Z`。
- Server SHA256：`4b46f23a91130c02d0c5e27c7bebcda430f13c3d61999bd8733525a1338adabc`。
- 本地临时发布目录：`/private/tmp/ai-agent-ha-e7e1319-linux-amd64`。该目录不进入版本控制，长期保存或传输时需要连同 `manifest.txt` 和 `SHA256SUMS` 一起归档。

## 验证边界

首次发布前验证时主机为 macOS，Docker daemon socket 不存在，未提供专用 Linux 双节点入口；该次验证未启动真实 PostgreSQL/Redis 服务，也未部署服务到目标节点。

一小时用例使用 Go 虚拟时钟验证负载调度逻辑，不是在线持续压测。Shell 语法与 Compose 校验不是部署验收；交叉编译成功也不证明 Linux 运行正常。

阶段 3.3 仍待：双节点运行、至少一小时真实负载、审批 CAS、失租/崩溃恢复、故障中的 SSE 终态检查，以及 20% → 50% → 100% 灰度门禁。未修改灰度比例，未执行故障注入，未形成完整 HA 验收结论。

## 后续本地存储验证（同日续接）

重新检查时 Docker daemon 已可用。对上述提交的干净快照，在本地独立 PostgreSQL/Redis 夹具执行六项外部契约竞态测试，含子用例共 27 项全部通过，没有 SKIP；旧发布包清单全部 12 项再次校验通过。详见[真实存储验证记录](ha-external-contract-2026-10-02.md)。该进展仍不代表目标双节点环境或完整 HA 验收。
