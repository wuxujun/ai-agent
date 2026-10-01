# 双实例与 DAG 灰度演练

本文件是阶段 3.3 的执行手册，不是验收报告。当前未完成 Linux 双节点部署、真实 PostgreSQL/Redis 故障演练或连续一小时压测；仓库 `config.yaml` 的灰度比例保持不变。文件拆分、单元测试和单进程双 Handler 契约不能替代这些验证。

## 环境与发布物

在两个专用 Linux 测试节点 A/B 部署同一 Git 提交、同一 Go 构建产物，记录二进制 SHA256、配置版本、`teams.yaml` 摘要。使用现有 [systemd 单元](../systemd/ai-agent.service)。不要把演练请求或故障注入发送到生产实例。

两节点使用同一专用 PostgreSQL 数据库和 Redis 总线。通过节点的 `/etc/ai-agent/ai-agent.env` 或现有秘密管理注入下列配置；不要把真实值写入仓库或验收报告：

- `AI_AGENT_STORE_TYPE=postgres`、`AI_AGENT_STORE_DSN`：共享 PostgreSQL。
- `AI_AGENT_REDIS_BUS_URL`：共享取消/审批总线。
- 现有审批加密 keyring：两节点使用相同当前密钥和必要的历史解密密钥。
- 认证配置、租户 allowlist、Workspace 路径、Wiki/Brain 数据及快照必须一致。文件型任务恢复需要同路径共享文件系统或经过验证的数据同步，不能只共享数据库。
- `AI_AGENT_MULTIAGENT_RUNTIME=legacy`，按节点统一设置 `AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT`；显式 `runtime=dag` 会绕过百分比，禁止用于比例演练。

先使用离线 LLM stub 或明确批准的测试 Provider，并配置租户预算、任务超时和固定的只读测试语料。两节点都由 Prometheus 采集；确认 runtime/event 指标覆盖率完整，监控中保留实例区分。

连接预算按所有实例合计：默认每实例最多 50 条 PostgreSQL 连接，两实例最多 100 条，另为迁移、监控及其他客户端留出容量。部署前确认目标库的配额足够。

## 运行前的契约检查

在拥有专用数据库凭据的测试环境设置 `AI_AGENT_RUN_EXTERNAL_INTEGRATION=true`、`TEST_POSTGRES_DSN`、`TEST_REDIS_URL` 后执行：

```bash
go test -race ./internal/store ./internal/api \
  -run 'Test(ExternalStoresTaskCreation|ExternalStoresTaskLeaseGuard|ExternalStoresPersistPausedTaskAcrossClients|ExternalPostgresDurableApprovalCASAcrossClients|ExternalPostgresDurableApprovalRecoveryContract|PostgresPoolExternal)$' \
  -count=1 -timeout=5m
```

检查输出，要求所有列出的用例实际执行，不能将缺少环境变量导致的 SKIP 当作通过。测试数据必须与业务数据隔离。

启动两个节点，分别验证 `/ping`、`/ready`；使用授权凭据检查 `/api/metrics`。用 [HTTP 样例](../../Sample/agent-api.http) 先完成一条创建、运行、查询链路，记录任务 ID，并确认另一节点能读取相同任务。先验证观测链路，再开始负载。

## 至少一小时的连续任务窗口

使用测试环境已有负载生成器，将同一批固定语料任务轮流提交至 A/B，持续至少 60 分钟。每个请求使用唯一任务 ID，保存提交节点、接收时间、最终状态、运行时选择、延迟与 Trace 摘要。约束并发到环境可承受范围；审批任务须包含批准和拒绝两条路径。不要把仅重复 `go test` 一小时视为在线压测。

| 窗口 | 操作 | 必须观察到的结果 |
|---|---|---|
| 0–15 分钟 | 正常只读任务与审批任务，轮流提交 A/B | 状态跨节点一致，无重复执行，DAG/Legacy 均有样本 |
| 15–25 分钟 | A 创建等待审批的任务，从 B 批准/拒绝；对同一审批并发提交决定 | CAS 只接受一次决定，工具最多执行一次，密钥解密正常 |
| 25–35 分钟 | 对运行中的只读测试任务，从另一节点取消；注入租约失效/存储续约失败 | 旧 owner 停止执行，不覆盖新 owner 的状态；已开始的外部动作单独核验 |
| 35–45 分钟 | 对 A 执行 SIGTERM；重启后再对测试实例执行崩溃恢复演练 | 优雅退出后可恢复任务处于 paused；崩溃后的任务在租约过期及显式恢复后只执行一次 |
| 45–60 分钟 | 恢复双节点负载，检查 SSE 重连、终态和持久化 Trace | 以持久化任务状态核对 SSE 终态；无审批丢失、状态倒退、重复终态处理或 Trace 丢失 |

Redis 中断、数据库连接耗尽或强制租约失效只允许对隔离测试基础设施操作。具体注入命令应由该环境负责人填写，不能猜测数据库表名或对共享业务库删改租约。

审批 CAS、失租保护及 SSE 终态检查需要单独断言；成功率和延迟指标本身不能证明这些行为。SSE 为进程内事件通道，跨节点重连时必须核对持久化状态，不预设它提供跨节点历史事件重放。

## 20% → 50% → 100% 的门禁

前述验收通过后才将两节点统一调到 20%；每一档单独积累完整观察窗口并复核审批、Replan Trace。使用现有只读门禁：

```bash
go run ./cmd/canary-gate --prometheus-url http://127.0.0.1:9090 \
  --window 1h --json
```

首次命令未声明人工复核通过，预期保持 HOLD。完成人工复核后才增加 `--manual-review-passed` 并保存报告。只有 `decision=PROMOTE`、退出码 0 且故障演练全部通过，才能进入下一档；样本不足、计数器重置、缺少事件覆盖或无 Legacy 对照均不得绕过门禁。

进入 100% 之前，在 50% 的完整窗口上完成 DAG/Legacy 比较并保存结果。100% 稳态窗口没有新的 Legacy 对照，现有比较门禁可能 HOLD，应使用事先保存的晋级证据与绝对 SLO 监控，不能将无对照窗口解释为自动通过。

进程环境变量通过重启生效。节点依次重启并验证 readiness，避免同时停机；重启后重新累积观察窗口。回退时将百分比恢复到 0 并保持 `runtime=legacy`。已创建任务保留 Trace 中记录的 runtime，不能通过切换默认配置强制改变已有任务。Legacy 代码的移除是后续独立变更。

## 验收记录

验收报告至少包含：提交与二进制摘要、脱敏配置版本、两个实例标识、数据库/Redis 版本、负载窗口与并发、各运行时样本数、连接数峰值、审批重复执行数、失租旧 owner 写入数、SSE/持久化终态不一致数、错误率/P95、故障注入时间与恢复时间、门禁 JSON、审批和 Replan 人工复核结论。未执行项明确标记待验收。
