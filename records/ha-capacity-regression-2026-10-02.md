# 阶段 3.3 双连接池容量回归（2026-10-02）

## 结果

专用 PostgreSQL 夹具已从 `max_connections=100` 调整为 **150**。新增 `TestPostgresPoolHAExternal`：先检查非保留连接配额，再同时填满两个实际 Store 连接池，并保持 10 个额外客户端连接，最后检查连接归还。默认每池 50 条时，实际同时持有 **110 条连接**。

旧配置的失败已真实复现：`max_connections=100`、保留连接 3 条，只剩 97 条可用，低于测试所需 110 条；用例按预期返回失败。新配置下七项 HA 存储契约（含子用例 28 项）全部通过，没有 SKIP 或竞态报告。该修复不修改 Agent 默认连接池、运行时或灰度比例。

## 源码与环境

- 基础提交：`e7e1319d04303a3a83551c63f3a273d440a93b0d`。
- 验证源为该提交的独立快照，仅叠加本次 `deploy/ha/compose.yaml` 和 `internal/store/ha_pool_external_test.go`。**这是带两项未提交改动的验证快照，不是干净的正式发布提交**；各文件 SHA256 记录于[结构化证据](ha-capacity-regression-2026-10-02.json)。其他工作区改动未纳入本轮验证。
- 专用 Compose 项目：`ai-agent-stage33-capacity-r8_qzce9`，本机回环端口 15432/16379，独立数据卷。
- PostgreSQL：`17.10 (Debian 17.10-1.pgdg12+1)`；新配置实际查询结果为 `max_connections=150`、`superuser_reserved_connections=3`、`reserved_connections=0`，非保留容量 147。
- 执行区间（含夹具初始化和停止）：`2026-10-02T08:22:45.885982Z` 至 `2026-10-02T08:24:09.485851Z`。
- 凭据运行时生成并通过环境注入，未写入仓库。结束后两个夹具容器均已停止，数据卷保留。

## 验证

在专用数据库环境设置 `AI_AGENT_RUN_EXTERNAL_INTEGRATION=true`、`TEST_POSTGRES_DSN` 和 `TEST_REDIS_URL` 后执行：

```bash
# 对旧配置复现不足，对新配置检查通过。
go test -race -json ./internal/store \
  -run '^TestPostgresPoolHAExternal$' -count=1 -timeout=5m

# 新配置下的七项契约，包串行初始化同一数据库。
go test -race -json -p=1 ./internal/store ./internal/api \
  -run '^Test(ExternalStoresTaskCreation|ExternalStoresTaskLeaseGuard|ExternalStoresPersistPausedTaskAcrossClients|ExternalPostgresDurableApprovalCASAcrossClients|ExternalPostgresDurableApprovalRecoveryContract|PostgresPoolExternal|PostgresPoolHAExternal)$' \
  -count=1 -timeout=5m

# 不设置外部集成变量的全仓验证，以及变更所在包的静态检查。
go test -p=4 ./... -count=1 -timeout=5m
go vet ./internal/store
```

新配置契约退出码为 0；全仓测试退出码为 0，49 个含测试的包通过；vet、Compose 配置校验通过。新容量用例耗时 1.78 秒。测试的 JSONL、全仓日志和原始汇总保存在 `/private/tmp/ai-agent-stage33-capacity-r8_qzce9`，日志 SHA256 及脱敏结果已保存到结构化证据。

## 剩余验收

本轮验证了单台开发机上两个 Store 客户端的数据库连接容量，没有部署两个独立 Linux Agent 实例，也没有执行一小时在线负载、进程故障注入或 DAG 放量。目标数据库仍须按实际客户端数量核定余量；10 个额外客户端是本契约验证范围，不是目标环境容量的完整估算。后续按[续接清单](../docs/阶段3.3未完成事项与下次续接.md)提供目标节点、共享存储、监控与故障注入范围后继续。
