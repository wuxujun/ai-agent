# 阶段 3.3 新提交发布前验证（2026-10-07）

## 结果与范围

固定提交 **`cc7d199920086b8f96b5e776bcb27ddbffa44de4`**（`Web UI: 完善审批统计与 Trace 分析`）的隔离干净 Git 检出已通过发布前验证：全仓 49 个含测试的包通过，相关竞态测试 9 个含测试的包通过，`go vet ./...`、HA 脚本语法、Console JavaScript 语法和 Compose 配置校验通过。两个 Linux 架构的发布包已重新构建并验证。

当前原工作区的 22 个已有修改文件未纳入验证或构建；完成构建时逐文件 SHA256 与开始时一致。本轮仅新增验证记录并更新任务文档。固定检出在测试和构建后仍为干净状态。

这次是**发布前准备**：没有启动存储夹具或目标 Agent、没有重跑真实外部存储契约、没有执行双节点 smoke、一小时在线负载、故障或审批副作用验收。`full_ha_acceptance=false`，仓库继续使用 `runtime=legacy`、`dag_canary_percent=0`。

## 工具链与首次失败

主机为 macOS/arm64。原有 Go 工具链为 `go1.26.8 darwin/amd64`：首次全仓测试因沙箱禁止 `httptest` 监听本地端口而失败；沙箱外重跑时，7 个包的测试进程最终被 Go 外层超时终止。对其中 `canarygate.test` 的一秒采样仅见主线程停留在 Rosetta Runtime Routines，未见 Go 调用栈；结合相同版本原生工具链上的成功重跑，判断为启动/模拟层问题。不能将这次失败记录为通过。

后续从 Go 官方模块代理下载相同版本的原生 `go1.26.8 darwin/arm64`，经 Go 模块校验后用于全仓测试、竞态测试和两种架构构建。工具链位于临时目录，系统 Go 安装未修改；模块校验值为 `h1:dFjGhY6f8DTsY8/rLchUTDRRfjJP4pCrGCwxvHFT2Fg=`。原生全仓测试约 45 秒通过，竞态测试约 37 秒通过。`go vet ./...` 在切换前已使用原有 amd64 工具链通过。

首次失败日志、进程采样和成功日志均保留在 `/private/tmp/ai-agent-stage33-release-_p4df5aj`；各日志 SHA256 及结果见[结构化证据](ha-release-preflight-2026-10-07.json)。控制区间为 `2026-10-08T05:25:05.640023+00:00` 至 `2026-10-08T06:39:17.221444+00:00`，包含环境诊断和等待时间，不是连续负载窗口。记录日期按本机时区，UTC 时间跨至 2026-10-08。

## 验证命令

全仓、竞态和构建通过 PATH 使用上述原生工具链，设置 `GOTOOLCHAIN=local` 和 `GOCACHE=/private/tmp/ai-agent-go-cache`；未加载原工作区 `.env`，清除了外部集成开关及模型凭据。全仓与竞态命令没有启用外部集成，不能用其 PASS 代替七项真实存储契约的执行。

```bash
go test -p=4 ./... -count=1 -timeout=5m
go test -race -p=4 ./internal/api ./internal/store ./internal/orchestrator \
  ./internal/multiagent ./internal/config ./internal/executor ./internal/types \
  ./internal/hasoak ./cmd/server ./cmd/ha-soak -count=1 -timeout=5m
go vet ./...
bash -n deploy/ha/build-bundle.sh deploy/ha/deploy-node.sh deploy/ha/test-load.sh deploy/ha/test-gate.sh
node --check internal/api/console_assets/app.js
HA_POSTGRES_PASSWORD=non-secret-validation-placeholder docker compose -f deploy/ha/compose.yaml config --quiet
```

## 新发布包

在上述干净检出中，不使用 `--allow-dirty`，分别执行：

```bash
bash deploy/ha/build-bundle.sh --output /private/tmp/ai-agent-stage33-release-_p4df5aj/bundle-linux-amd64 --arch amd64
bash deploy/ha/build-bundle.sh --output /private/tmp/ai-agent-stage33-release-_p4df5aj/bundle-linux-arm64 --arch arm64
```

| Linux 架构 | SHA256SUMS 核验 | Server SHA256 |
|---|---|---|
| amd64 | 12/12 | `1a51cdce4899b9997fee89f20ee88bf4a7be538f5b91358321c1c40f9751e528` |
| arm64 | 12/12 | `e5c0f4e31e54e40c27b0c7da2ea5ba11f0acbc53ba4f2b6bfae4897e45394fd0` |

每包均包含 `server`、`ha-soak`、`canary-gate`。六个二进制分别通过 ELF 架构和静态链接检查，Go build info 的 `GOOS=linux`、对应 `GOARCH`、`CGO_ENABLED=0`、`vcs.revision=cc7d199920086b8f96b5e776bcb27ddbffa44de4`、`vcs.modified=false` 均一致。清单均记录 `git_dirty=false` 和原生构建工具链。所有文件摘要、清单和二进制身份已保存到结构化证据。

发布包和源码检出位于仓库外临时目录，可能被清理；部署前确认存在并重新校验。目标节点架构尚未提供，两个包仅作为准备；应选取匹配的同一架构包分发到两节点，并核对配置、租户、审批密钥、共享 workspace 和节点身份。旧 `e7e1319` 包保留为历史证据，不作为本提交的发布物。

## 下一步

按[续接清单](../docs/阶段3.3未完成事项与下次续接.md)补齐目标节点、共享 PostgreSQL/Redis/workspace、认证及审批密钥配置位置、Prometheus 和故障注入范围。随后在目标专用存储重新执行七项契约，完成双节点部署/smoke、一小时负载、审批与故障恢复、灰度门禁。未获得这些环境信息前不开展依赖它们的操作，灰度保持 0%。
