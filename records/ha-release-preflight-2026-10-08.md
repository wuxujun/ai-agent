# 阶段 3.3 当前提交发布准备与本地存储契约（2026-10-08）

## 结果与范围

固定提交 **`fbec15693a9b34eaf83ea45c7e35e011decec9f0`**（`Web UI: 记录工具耗时并展示 Trace 分析`）的隔离干净 Git 检出完成发布准备：全仓 49 个含测试的包、相关竞态测试 9 个含测试的包、`go vet ./...`、HA 脚本/Console JavaScript 语法和 Compose 配置校验全部通过。Linux/amd64、Linux/arm64 发布包均已重建，各 12 项 SHA256 校验通过。

本轮还在新建的专用本地 PostgreSQL/Redis 上实际执行七项存储契约，含子用例共 **28 项通过，0 SKIP、0 FAIL**，开启竞态检测。双连接池实际同时保持 **50 + 50 + 10 = 110 条连接**。这补充了当前提交的真实存储证据，历史 `e7e1319` 结果不再是本轮存储验证的唯一依据。

开始时原工作区干净，先前未提交的执行器、DAG、Store 和 UI 改动已经包含在当前提交中。验证、构建及契约执行均使用仓库外的固定检出，结束后该检出仍干净；原工作区在本轮记录更新前也保持干净。本轮仓库变更仅为四个验证与续接文档，不修改运行时代码或配置。

**阶段 3.3 仍未完成**：没有目标 Linux 双节点、目标共享存储/workspace、Prometheus 入口或故障注入范围，未部署 Agent、运行在线 smoke/一小时负载、注入故障或提升灰度。仓库保持 `runtime=legacy`、`dag_canary_percent=0`，`full_ha_acceptance=false`。

## 源码、工具链与命令

执行目录：`/private/tmp/ai-agent-stage33-fbec156-ep9ftyh0`；固定检出位于其中的 `source`。沿用先前经模块校验的临时原生 `go1.26.8 darwin/arm64`，设置 `GOTOOLCHAIN=local`、`GOCACHE=/private/tmp/ai-agent-go-cache`，未修改系统工具链。工具链二进制摘要、每项检查时间、退出码和日志摘要见[结构化证据](ha-release-preflight-2026-10-08.json)。

没有加载原工作区 `.env`；检查进程清除外部集成开关及模型凭据。只有专用存储契约进程设置外部集成变量，不调用在线模型。需要临时监听端口或 Docker socket 的命令经沙箱外执行授权后运行。

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

## 双架构发布包

从固定干净检出构建，未使用 `--allow-dirty`：

```bash
bash deploy/ha/build-bundle.sh --output /private/tmp/ai-agent-stage33-fbec156-ep9ftyh0/bundle-linux-amd64 --arch amd64
bash deploy/ha/build-bundle.sh --output /private/tmp/ai-agent-stage33-fbec156-ep9ftyh0/bundle-linux-arm64 --arch arm64
```

| 架构 | SHA256SUMS | Server SHA256 |
|---|---|---|
| Linux/amd64 | 12/12 | `634cbb8b33027d3e15f06e600fbfed6d29dad015c8919b36b7093f68e9743600` |
| Linux/arm64 | 12/12 | `eb1e797bb63438c47bbb7f264efaf6b83e8e33e9b66c81d3bbd7b988853f868f` |

六个二进制的 ELF 架构、静态链接、`GOOS=linux`、对应 `GOARCH`、`CGO_ENABLED=0`、`vcs.revision=fbec15693a9b34eaf83ea45c7e35e011decec9f0`、`vcs.modified=false` 均已核对。两个清单均标记同一提交、`git_dirty=false`。所有文件摘要保存于结构化证据。

发布目录为临时目录，部署前须确认存在、重新核验 SHA256，并选择与目标架构一致的包；旧 `cc7d199` 发布物仅作历史证据。本轮文档更新不会改变这些发布包的源码身份。

## 七项本地真实存储契约

专用 Compose 项目为 `ai-agent-stage33-fbec156-ep9ftyh0`，采用固定检出的 `deploy/ha/compose.yaml`，新建独立数据卷，PostgreSQL/Redis 仅映射回环端口 15432/16379。数据库凭据随机生成，仅保存在仓库外权限 0600 的文件中并通过环境注入，未写入提交或报告。

实际查询 PostgreSQL 为 `17.10 (Debian 17.10-1.pgdg12+1)`、`max_connections=150`、`superuser_reserved_connections=3`、`reserved_connections=0`，非保留容量 147。设置 `AI_AGENT_RUN_EXTERNAL_INTEGRATION=true`、`TEST_POSTGRES_DSN`、`TEST_REDIS_URL` 后执行：

```bash
go test -race -json -p=1 ./internal/store ./internal/api \
  -run '^Test(ExternalStoresTaskCreation|ExternalStoresTaskLeaseGuard|ExternalStoresPersistPausedTaskAcrossClients|ExternalPostgresDurableApprovalCASAcrossClients|ExternalPostgresDurableApprovalRecoveryContract|PostgresPoolExternal|PostgresPoolHAExternal)$' \
  -count=1 -timeout=5m
```

契约执行区间为 `2026-10-08T08:15:29.424365+00:00` 至 `2026-10-08T08:15:37.695330+00:00`。七项顶层测试全部实际执行，28 项 PASS；容量用例日志记录同时持有 110 条真实连接，并检查连接归还。验证脚本首次在创建凭据文件时因 `Path.open` 参数不支持而退出，在启动容器和运行测试前改用内置 `open` 后重跑；该准备失败没有记为契约通过。

结束后仅停止本轮两个夹具容器：PostgreSQL `aaa4fc2459b4`、Redis `b9d4232923b1` 均核验为 `Exited (0)`，数据卷与原始日志保留，没有删除目录、卷或测试数据。控制区间包含授权等待和文档准备，不能解释为连续在线负载。

本地契约验证的是共享存储客户端行为与连接容量，不能替代目标环境配额核定、跨节点审批 API 副作用、失租执行停止、进程恢复或故障中的 SSE 验收。

## 下一步

下一步进入目标双节点环境验收，所需字段及待提供状态已汇总到[环境交接清单](../docs/阶段3.3未完成事项与下次续接.md#下次开始前需要补齐的信息)：A/B 地址、架构、SSH 登录方式和部署路径，共享 PostgreSQL/Redis/workspace、认证及审批密钥配置位置，测试模型与预算、Prometheus 入口，以及故障注入范围。凭据通过环境或秘密管理注入，交接时只提供位置或引用。

环境就绪后依次执行目标存储七项契约 → 发布包核验与双节点部署 → 双节点 smoke → 至少一小时连续负载 → 独立审批与故障恢复验收 → 灰度门禁。目标信息仍待提供，依赖这些信息的操作尚未执行；阶段 3.3 保持未完成、灰度保持 0%。本次文档同步没有新增测试或验收结论。
