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

## 手动构建与发布脚本

以下脚本只生成发布物或在操作员指定的 Linux 测试节点执行；它们不连接第三方服务器，也不自动改变灰度比例。需要 Go 工具链、Git；节点脚本使用 systemd、GNU coreutils、curl 和 sha256sum。**从干净的提交构建正式演练发布物**；`--allow-dirty` 仅用于隔离环境试跑，清单会标出 `git_dirty=true`，节点安装时也必须显式传入 `--allow-dirty`。

在开发机的仓库外生成一个新目录，`--arch` 按节点选择 `amd64` 或 `arm64`：

```bash
bash deploy/ha/build-bundle.sh --output /tmp/ai-agent-ha-bundle --arch amd64
```

发布物包含同一构建的 `server`、`ha-soak`、`canary-gate`、配置、Team/Skill 文件、systemd 单元、操作脚本、清单和 SHA256 校验文件。将**同一个目录**复制到节点 A、B；记录两节点打印的 Server SHA256，并核对两者一致。示例：

```bash
scp -r /tmp/ai-agent-ha-bundle operator@node-a:/tmp/
scp -r /tmp/ai-agent-ha-bundle operator@node-b:/tmp/
ssh operator@node-a 'sudo bash /tmp/ai-agent-ha-bundle/deploy-node.sh --bundle /tmp/ai-agent-ha-bundle'
ssh operator@node-b 'sudo bash /tmp/ai-agent-ha-bundle/deploy-node.sh --bundle /tmp/ai-agent-ha-bundle'
```

预检完成后，在两个节点分别给相同命令加 `--apply`。部署脚本只管理全新或由该脚本创建的 `/opt/ai-agent/{server,config.yaml,teams.yaml,skills,current}` 链接；遇到原有普通文件、不同的 systemd 单元或校验失败会停止，不覆盖。发布物按版本保存在 `/opt/ai-agent/releases/`；切换 `current` 后重启服务，检查本机 `/ping` 和 `/ready`，更新失败时尝试切回旧版本。首次部署没有旧版本可回退。不会删除旧发布物或数据。

操作员须先在两节点创建 `ai-agent` 用户/组、root 所有且权限为 0600 的 `/etc/ai-agent/ai-agent.env`，以及由该用户可写的 `/opt/ai-agent/workspace`。环境文件设置共享 PostgreSQL、Redis、相同的审批 keyring、认证/租户配置，明确设置 `AI_AGENT_MULTIAGENT_RUNTIME=legacy` 和初始 `AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT=0`；不要将它复制进发布物。两节点的 workspace 必须挂载到同一受控共享存储或完成可靠同步验证，发布脚本不会创建或证明共享挂载。还需核对节点身份、相同的配置与 Team 摘要、监控覆盖和测试租户策略。脚本预检不替代这些人工核对。

如果服务地址不是默认本机 `127.0.0.1:8088`，发布时传入 `--health-url http://127.0.0.1:<port>`。测试流量应经受控网络或 SSH 隧道进入，不要把管理 API 直接暴露在公网。

## 运行前的契约检查

在拥有专用数据库凭据的测试环境设置 `AI_AGENT_RUN_EXTERNAL_INTEGRATION=true`、`TEST_POSTGRES_DSN`、`TEST_REDIS_URL` 后执行：

```bash
go test -race ./internal/store ./internal/api \
  -run 'Test(ExternalStoresTaskCreation|ExternalStoresTaskLeaseGuard|ExternalStoresPersistPausedTaskAcrossClients|ExternalPostgresDurableApprovalCASAcrossClients|ExternalPostgresDurableApprovalRecoveryContract|PostgresPoolExternal)$' \
  -count=1 -timeout=5m
```

检查输出，要求所有列出的用例实际执行，不能将缺少环境变量导致的 SKIP 当作通过。测试数据必须与业务数据隔离。

启动两个节点，分别验证 `/ping`、`/ready`；使用授权凭据检查 `/api/metrics`。用 [HTTP 样例](../../Sample/agent-api.http) 先完成一条创建、运行、查询链路，记录任务 ID，并确认另一节点能读取相同任务。先验证观测链路，再开始负载。

## 可执行的持续测试工具

仓库提供 `cmd/ha-soak`，只向操作员指定的两个实例提交新建测试任务，不改运行时配置、不自动批准工具、不注入故障、不删除任务。请求经过已有出站 URL 策略；私网端点需要显式 `--allow-private-network`，所有重定向均被拒绝，避免凭据转发。

通过秘密管理向进程提供 `AI_AGENT_HA_API_KEY`（测试租户凭据），两实例必须接受相同租户身份。输出路径必须不存在；文件以 0600 创建。报告只记录任务 ID、状态、实例序号、延迟、运行时和终态/Trace 摘要，不包含 API key、原始响应、任务回答或 Trace 正文。

先构建并做短时 smoke（以下本地地址示例需要两个已部署实例）：

```bash
go build -o /tmp/ai-agent-ha-soak ./cmd/ha-soak
/tmp/ai-agent-ha-soak \
  --node-a http://127.0.0.1:18088 --node-b http://127.0.0.1:18089 \
  --allow-private-network --team software --workspace /opt/ai-agent/workspace/ha-fixture \
  --goal 'Read README.md and summarize it; do not modify files or execute commands.' \
  --duration 2m --interval 10s --max-tasks 20 --concurrency 2 \
  --results /tmp/ha-smoke-results.jsonl --report /tmp/ha-smoke-report.json
```

短于一小时的 smoke 即使用例全成功也返回 1，`window_complete=false`。检查逐任务结果，不把这个预期 HOLD 当成真实测试失败。

正式负载使用相同参数，将 `--duration` 改为 `1h`、`--interval` 改为 `30s`、`--max-tasks` 改为 `150`，并指定新的输出文件名。默认每任务最多 12 次 LLM 调用、0.05 USD 预算；服务端必须配置对应 Provider 价格，否则创建会被拒绝。使用真实模型前应核实整体预算（默认最多 150 个任务，即配置上限合计 7.50 USD；服务端计费估算与实际结算可能不同）。测试目标和 workspace 必须是已授权夹具，仅靠只读提示词不能限制所有工具权限，应在测试 Team/租户策略中限制允许的工具。

工具交替向 A/B 提交 `multiagent` 任务；每个任务完成后对比另一实例读取到的状态、回答、终止类型和完整 Trace，并在另一实例重连 SSE 核对终态。运行时以持久化的 `multiagent_runtime_selection` Trace 为准，没有该 Trace 时记为 `unknown`，不猜测 DAG/Legacy。两 URL 不同不等于两个独立节点，实例身份仍须部署证据证明。

`duration` 限制新任务接纳窗口，已接纳任务最多再等待 `task-timeout`；达到 `max-tasks` 会提前停止并保持未完成状态。未完成、遇到审批或暂停的任务不会被自动恢复或批准，工具只对自己生成的任务 ID 发起取消；取消失败写入逐任务报告，需要人工按 ID 处理。跨节点取消返回 202 仅表示信号已接纳，仍需按任务 ID 核对最终状态。报告不删除测试数据。

退出码：0 表示一小时负载窗口和本工具的跨实例/终态 SSE 检查通过；1 表示 smoke、接纳上限提前耗尽或任务检查失败；2 表示预检、输出或执行中断。**所有报告的 `full_ha_acceptance` 都是 false**，并列出未覆盖的审批 CAS、重复副作用、失租、崩溃恢复、故障中的实时 SSE 和 Prometheus/人工门禁。这些项目仍需按下表独立演练。

在独立的 Linux 控制节点上，也可使用发布物内的包装脚本保存结果并核对工具退出码；它需要 Python 3 和通过秘密管理提供的 `AI_AGENT_HA_API_KEY`。控制节点不要是即将被故障注入的 A/B 实例。下例假设 A/B 的 HTTP 入口只在受控网络可达，输出目录必须不存在：

```bash
bash /tmp/ai-agent-ha-bundle/test-load.sh \
  --bundle /tmp/ai-agent-ha-bundle --mode smoke \
  --node-a http://10.0.0.11:8088 --node-b http://10.0.0.12:8088 \
  --allow-private-network --workspace /opt/ai-agent/workspace/ha-fixture \
  --team software --goal 'Read README.md and summarize it; do not modify files or execute commands.' \
  --output /tmp/ha-smoke-001
```

检查 `results.jsonl`、`report.json`。短时 smoke 的底层工具预期返回 1；包装脚本只有在任务全部通过、两个端点均有样本且报告仍标记窗口未完成时才返回 0。通过后把 `--mode smoke` 改成 `--mode soak`，使用新的输出目录执行一小时负载。包装脚本不会把负载通过解释为完整 HA 验收。故障注入、审批 CAS、崩溃恢复和实时 SSE 仍按下表人工执行并记录。

每档灰度的完整观察窗口结束后，在控制节点保存只读门禁结果。先不带人工复核标志运行，检查 `decision=HOLD` 的原因；审批与 Replan Trace 人工复核完成后，再指定新文件名并加入 `--manual-review-passed`：

```bash
bash /tmp/ai-agent-ha-bundle/test-gate.sh \
  --bundle /tmp/ai-agent-ha-bundle --prometheus-url http://127.0.0.1:9090 \
  --output /tmp/ha-gate-20pct.json
```

门禁脚本保留 `canary-gate` 退出码：0 为 PROMOTE、1 为 HOLD、2 为查询/参数错误；它仅生成报告，不会更改节点配置。按下方门禁要求在 20%、50% 的窗口留存 DAG/Legacy 对照，并在进入 100% 前完成晋级判断。

### 本地专用存储依赖

[compose.yaml](compose.yaml) 复用外部集成 CI 的 PostgreSQL/Redis 镜像，仅提供专用存储依赖，不代替两个服务实例。Docker 服务可用后，通过环境注入独立的 `HA_POSTGRES_PASSWORD`，然后执行：

```bash
docker compose -f deploy/ha/compose.yaml config --quiet
docker compose -f deploy/ha/compose.yaml up -d --wait
```

数据库为 `ai_agent_ha`、用户为 `ai_agent_ha`，仅监听本机 15432；Redis 仅监听本机 16379。两服务使用专用持久卷。单机容器不证明双物理/虚拟节点 HA。演练结束可用 `docker compose -f deploy/ha/compose.yaml stop` 停止服务，不自动删除数据卷。

## 至少一小时的连续任务窗口

使用上述工具或测试环境已有负载生成器，将同一批固定语料任务轮流提交至 A/B，持续至少 60 分钟。每个请求使用唯一任务 ID，保存提交节点、接收时间、最终状态、运行时选择、延迟与 Trace 摘要。约束并发到环境可承受范围；审批任务须包含批准和拒绝两条路径。不要把仅重复 `go test` 一小时视为在线压测。

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
