# Task API 请求文档

本文档对应当前 `ai-agent` HTTP 服务的 Task 接口。默认示例地址为：

```text
http://127.0.0.1:8088
```

## 1. 鉴权与通用约定

除 `/ping`、`/ready` 外，Task 接口均位于 `/api` 鉴权组内。API Key 模式示例：

```http
X-API-Key: {{api_key}}
```

JSON 请求需要：

```http
Content-Type: application/json
```

本文使用以下变量：

```bash
export API_BASE_URL=http://127.0.0.1:8088
export API_KEY='replace-with-api-key'
export TASK_ID='task-demo-001'
```

常见 Task 状态：

| 状态 | 含义 |
|---|---|
| `created` | 已创建，尚未执行 |
| `running` | 正在执行 |
| `awaiting_approval` | 等待高风险操作审批 |
| `paused` | 服务关闭等原因暂停，可恢复 |
| `completed` | 已完整完成 |
| `partial` | 已产生部分结果，但未完全满足要求 |
| `failed` | 执行失败或被取消 |

`completed`、`partial`、`failed` 是终态。

## 2. 创建 Task

```http
POST /api/tasks
```

完整请求示例：

```bash
curl -sS \
  -H "X-API-Key: ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "task-demo-001",
    "session_id": "session-demo-001",
    "mode": "multiagent",
    "team": "software",
    "goal": "分析当前项目并给出测试结果",
    "workspace": "./workspace/demo",
    "max_steps": 6,
    "tool_budget": 10,
    "token_budget": 20000,
    "llm_call_budget": 12,
    "llm_cost_budget_usd": 0
  }' \
  "${API_BASE_URL}/api/tasks"
```

请求字段：

| 字段 | 必填 | 说明 |
|---|---:|---|
| `id` | 否 | Task ID；省略时由服务生成，重复 ID 返回 `409` |
| `session_id` | 否 | 关联的 Session；必须存在且处于 `active` 状态 |
| `goal` | 是 | Task 目标 |
| `workspace` | 是 | 工作目录，必须通过工作区安全策略 |
| `mode` | 否 | `eino`、`legacy`、`adk`、`step` 或 `multiagent` |
| `team` | 否 | 仅用于 `multiagent`；受租户 Team allowlist 约束 |
| `max_steps` | 否 | 最大执行步数；小于等于 0 时默认为 5 |
| `tool_budget` | 否 | 工具调用预算；小于等于 0 时默认为 5 |
| `token_budget` | 否 | Task Token 上限；0 表示不设置，不能为负数 |
| `llm_call_budget` | 否 | LLM 调用上限；0 使用服务默认值，不能为负数 |
| `llm_cost_budget_usd` | 否 | 预估美元成本上限；0 使用默认值，不能为负数 |

成功返回 `201 Created` 和完整 Task。若省略 `team`，Multi-Agent Team 的选择顺序为租户默认、进程默认；选择结果会持久化在 `team`、`team_selection_source` 和 `team_config_digest` 中。

最小请求：

```bash
curl -sS \
  -H "X-API-Key: ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"goal":"总结项目结构","workspace":"./workspace/demo"}' \
  "${API_BASE_URL}/api/tasks"
```

## 3. 单步执行

```http
POST /api/tasks/:id/run
```

```bash
curl -sS \
  -X POST \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/run"
```

非流式模式在当前步骤执行并持久化后返回 `200` 和最新 Task。单步请求最长约 60 秒。Task 正在本实例或其他实例执行时返回 `409`；并发槽不足时返回 `503`。

单步 SSE 模式：

```bash
curl -N \
  -X POST \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/run?stream=true"
```

## 4. 运行至终态

```http
POST /api/tasks/:id/run-all
```

后台执行：

```bash
curl -sS \
  -X POST \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/run-all"
```

正常启动时返回 `202 Accepted`：

```json
{
  "message": "task is running in background",
  "task_id": "task-demo-001",
  "status": "running"
}
```

随后轮询 `GET /api/tasks/:id`。对不可恢复的终态 Task 重复调用会直接返回 `200` 和当前 Task。重复执行、租约冲突或状态竞争通常返回 `409`。

运行至终态并接收 SSE：

```bash
curl -N \
  -X POST \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/run-all?stream=true"
```

客户端断开该流时，服务会取消对应的后台执行上下文。

## 5. 查询 Task

### 5.1 查询单个 Task

```bash
curl -sS \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks/${TASK_ID}"
```

成功返回 `200`；不存在或不属于当前租户的 Task 返回 `404`。

关键响应字段包括：

- `status`、`final_answer`、`error_code`、`error_message`、`trace`；
- `execution_trace_id`（可选）：最近一次执行请求的 OTel Trace ID，仅在请求携带有效 Span 上下文时存在；
- `team`、`team_selection_source`、`team_config_digest`；
- `step_count`、`tool_budget`、`token_budget`；
- `llm_calls`、`llm_estimated_cost_usd`；
- `answer_audit`（启用答案流水线时）。

任务因客户端断开、主动取消或执行超时而失败时，`final_answer` 保持为空，
对外错误分别通过 `error_code` 和 `error_message` 返回。上游地址、内部场景名及
原始 LLM 错误只记录在服务日志中，不写入 Task 响应。其他业务失败继续保留原有
`final_answer` 兼容行为。

### 5.2 查询 Task 列表

```http
GET /api/tasks?status=completed&session_id=session-demo-001&limit=20&offset=0
```

```bash
curl -sS \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks?status=completed&limit=20&offset=0"
```

支持的查询参数：

| 参数 | 说明 |
|---|---|
| `status` | 按 Task 状态过滤 |
| `session_id` | 按 Session 过滤 |
| `limit` | 分页数量 |
| `offset` | 分页偏移 |

普通租户只能看到自己的 Task；管理员查询遵循服务端管理员策略。

控制台可使用摘要视图和稳定游标分页，原有 `GET /api/tasks` 的结构与排序保持不变：

```http
GET /api/tasks?view=summary&status=running&session_id=session-demo-001&limit=20
GET /api/tasks?view=summary&status=running&session_id=session-demo-001&limit=20&cursor=<next_cursor>
```

摘要仅包含任务 ID、租户/Session、目标、状态、模式/Team、步骤数、LLM 调用与预估成本、创建及更新时间，不包含 Trace、Memory、最终答案或审计正文。按 `created_at DESC, id DESC` 排序；`count` 是本页数量，`has_more` 表示还有下一页。`next_cursor` 只适用于同一租户、状态和 Session 筛选，不能与 `offset` 混用。`limit` 默认为 20，范围为 1–100。

任务详情可用 `GET /api/tasks/:id?view=summary` 读取不含 Trace/Memory 的元数据及 `allowed_actions`。动作值为 `run_all`、`cancel`、`re_audit`、`delete` 中后端当前允许显示的操作；`run_all` 会覆盖可恢复的 Multi-Agent 部分完成任务。动作列表是页面提示，执行时仍由服务端再次校验状态、权限及并发条件。

配置 `api.trace_view_url_template`（或环境变量 `AI_AGENT_API_TRACE_VIEW_URL_TEMPLATE`）并提供有效的任务 `execution_trace_id` 后，摘要详情额外返回 `otel_trace_url`。模板需包含且仅包含一个 `{trace_id}`，使用绝对 HTTPS URL；仅本机追踪界面可使用 HTTP。服务端验证模板并在租户授权后的任务详情中生成链接；未配置或任务没有有效 Trace ID 时省略。该设置可热重载，非法候选配置会被拒绝并保留原配置。链接指向部署方指定的追踪系统，其访问权限仍由该系统控制；不要在模板 URL 中放入密钥。

Multi-Agent DAG 任务可读取已持久化的工作流关系图：

```http
GET /api/tasks/:id/workflow
```

有有效运行检查点时返回 `{"available":true,"graph":{"workflow":"planner_researcher_writer","graph_digest":"…","levels":[[{"id":"plan","role":"planner","condition":"always","state":"succeeded"}],[{"id":"research","role":"researcher","depends_on":["plan"],"condition":"always","state":"running"}],[{"id":"write","role":"writer","depends_on":["research"],"condition":"always","state":"pending"}]]}}`；`levels` 是拓扑阶段，节点按真实工作流依赖排列。状态为 `pending`、`running`、`succeeded`、`skipped` 或 `failed`。服务端核验检查点版本、内置图摘要、依赖和状态；不返回检查点的执行结果或错误正文。无检查点、旧版工作流或元数据不匹配时返回 `{"available":false}`，页面继续展示顺序 Trace。此图描述工作流节点依赖，不表示每条 Trace 记录之间的因果关系。接口沿用 Task 租户授权；跨租户读取返回 `404`。

长 Trace 使用独立分页接口：

```http
GET /api/tasks/:id/trace?limit=100
GET /api/tasks/:id/trace?limit=100&cursor=<next_cursor>
```

响应包含 `events`、本页 `count`、`has_more`、`next_cursor`。每条事件包含 `sequence`、`event_id`、可选的 `recorded_at` 和原始 `trace`。例如：

```json
{"sequence":1,"event_id":"task-demo-001:1","recorded_at":"2026-10-02T12:00:00Z","trace":{"step":1,"action":"search"}}
```

`sequence` 是持久化顺序号，`event_id` 由 Task ID 和该顺序号组成；`trace.step` 是可重复的逻辑步骤。`recorded_at` 表示事件首次写入 Store 的时间，并非动作实际发生时间；旧数据没有可靠时间时省略该字段。完整 Trace 快照覆盖同一顺序号时保留原时间。若任务重排或截断 Trace，顺序号和事件 ID 可对应到不同内容，不应将其视为内容哈希。游标绑定 Task ID；`limit` 默认为 100，范围为 1–200。任务正在执行时，新事件会追加，跨页读取不是事务性快照；刷新当前页可对账。普通租户读取别人的 Task 时统一返回 `404`。

SQLite/PostgreSQL 直接按索引读取摘要与 Trace 页。Redis 在保存完整 Task 的同时，原子更新摘要索引、详情元数据及逐条 Trace 列表；摘要按创建时间游标读取，Trace 按事件序号读取，正常分页不再反序列化完整 Task。首次摘要查询会迁移旧 Redis 任务的读取索引，耗时与旧任务数量和 Trace 总量相关；直接访问尚未迁移的旧任务时，详情与 Trace 暂时回退到完整 Task 读取。Redis 每次保存完整 Trace 快照仍要同步重建分页列表，超长且频繁更新的任务会增加写入成本。部署时应先让所有 Redis 写入实例升级到包含读取索引的版本，再启用新摘要查询，避免旧版本写入造成索引过期。

## 6. 独立订阅 SSE

```http
GET /api/tasks/:id/stream
```

```bash
curl -N \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/stream"
```

每条事件的 `data` 是一个 JSON 对象：

```json
{
  "task_id": "task-demo-001",
  "status": "running",
  "step": {},
  "token": "optional streaming token",
  "approval": null
}
```

终态事件可能包含：

```json
{
  "task_id": "task-demo-001",
  "status": "failed",
  "error_code": "client_disconnected",
  "error_message": "Task was canceled because the streaming client disconnected.",
  "token_usage": {
    "prompt_tokens": 100,
    "completion_tokens": 50,
    "total_tokens": 150
  }
}
```

如果订阅时 Task 已经进入终态，服务会立即发送一个终态事件并关闭连接。非终态长连接每约 15 秒发送 keep-alive。

## 7. 审批高风险操作

当 Task 状态为 `awaiting_approval` 时，从 SSE 事件的 `approval.id` 获取审批 ID。
浏览器刷新或跨实例切换后，也可以从下面的持久化查询接口重新获取审批 ID：

```http
GET /api/approvals?status=pending&limit=20
GET /api/approvals/stats
GET /api/approvals/:approval_id
GET /api/tasks/:id/approvals?status=pending
```

列表返回 `approvals`、本页 `count`、`has_more` 和可选 `next_cursor`。下一页将 `next_cursor` 作为 `cursor` 查询参数，游标仅可用于相同租户与状态筛选。默认状态为 `pending`，支持 `approved`、`rejected`、`expired` 和 `consumed`。任务下审批接口不传 `status` 时返回该任务的全部审批记录。

`GET /api/approvals/stats` 返回当前租户**全量持久化审批**的 `pending`、`approved`、`rejected`、`expired`、`consumed` 数量，以及可选的 `oldest_pending_at`。它不受列表分页或状态筛选影响；审批并发变化时，统计和随后读取的列表可能来自不同时间点。响应不包含审批正文或其他租户的数据。例如：

```json
{"pending":2,"approved":1,"rejected":0,"expired":0,"consumed":3,"oldest_pending_at":"2026-10-02T12:00:00Z"}
```

审批读取响应仅包含审批 ID、任务 ID、状态、版本、动作、风险级别、工作区、脱敏参数摘要/预览、时间及可用的决策主体标识 `actor_id`；不会返回持久化的操作密文、决策密文或原始参数。普通租户只能读取自己的审批，不可见与不存在均返回 `404`。`approved/rejected` 表示决策已记录；`consumed` 表示恢复检查点已消费。JWT 的 `actor_id` 来自已验证 `sub` 的短哈希；当前 introspection 模式缺少独立主体声明时仅能标识租户，不能据此区分同租户的不同人员。

批准：

```bash
curl -sS \
  -H "X-API-Key: ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{
    "approval_id": "approval-id-from-sse",
    "message": "approved",
    "parameters": {}
  }' \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/approve"
```

拒绝：

```bash
curl -sS \
  -H "X-API-Key: ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{
    "approval_id": "approval-id-from-sse",
    "message": "rejected by operator"
  }' \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/reject"
```

如果当前 Task 只有一个待审批项，可以省略请求体；存在多个待审批项时必须提供 `approval_id`，否则返回 `409` 和待审批 ID。已过期审批返回 `410`。集群模式可能返回 `202`，表示信号已转发或持久化恢复正在后台进行。

## 8. 取消 Task

```http
DELETE /api/tasks/:id/cancel
```

```bash
curl -sS \
  -X DELETE \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/cancel"
```

本实例内的活动 Task 接受取消信号后返回：

```json
{
  "message": "task cancellation signal sent",
  "task_id": "task-demo-001",
  "error_code": "task_canceled",
  "error_message": "Task was canceled via API."
}
```

本实例运行中的 Task 通常返回 `200`。集群模式下远端取消信号可能返回 `202`。Task 不存在返回 `404`；非运行状态返回 `400`。

## 9. 删除 Task

删除一个非活动 Task：

```bash
curl -sS \
  -X DELETE \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks/${TASK_ID}"
```

运行中或等待审批的 Task 必须先取消，否则返回 `409`。成功返回 `200`。该操作同时清理对应的 SSE 终态缓存。

管理员清空全部 Task：

```bash
curl -sS \
  -X DELETE \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks?confirm=true"
```

这是不可逆的管理操作，需要管理员权限和显式 `confirm=true`；存在活动 Task 时返回 `409`。

## 10. 重新审计最终答案

```http
POST /api/tasks/:id/re-audit
```

复用未变化的阶段结果：

```bash
curl -sS \
  -X POST \
  -H "X-API-Key: ${API_KEY}" \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/re-audit"
```

强制重新执行可用阶段：

```bash
curl -sS \
  -H "X-API-Key: ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"force":true}' \
  "${API_BASE_URL}/api/tasks/${TASK_ID}/re-audit"
```

仅终态且存在 `final_answer` 的 Task 可重新审计。常见失败状态：答案流水线不可用 `503`、Task 非终态 `409`、没有答案 `422`、租约冲突 `409`。

## 11. 推荐调用流程

普通异步流程：

```text
POST /api/tasks
  -> POST /api/tasks/:id/run-all
  -> GET /api/tasks/:id 或 GET /api/tasks/:id/stream
  -> completed / partial / failed
```

需要审批的流程：

```text
POST /api/tasks/:id/run-all?stream=true
  -> awaiting_approval + approval.id
  -> POST /api/tasks/:id/approve 或 /reject
  -> 必要时再次 POST /api/tasks/:id/run-all
  -> 终态
```

## 12. 常见 HTTP 状态码

| 状态码 | 常见含义 |
|---:|---|
| `200` | 查询、单步执行、审批、取消或删除成功 |
| `201` | Task 创建成功 |
| `202` | 后台执行已启动，或集群信号已转发 |
| `400` | 参数、状态、预算或确认参数不合法 |
| `401` | 缺少或无效凭证 |
| `403` | 租户无权访问 Team、Workspace 或资源 |
| `404` | Task、Session 或审批不存在，或租户不可见 |
| `409` | Task 重复、正在运行、租约冲突或审批冲突 |
| `410` | 审批已过期 |
| `422` | Task 不满足重新审计条件 |
| `503` | 并发槽、答案流水线或依赖暂不可用 |

## 13. 安全注意事项

- 不要把 API Key 放在 URL 查询参数中。
- 不要在日志中打印 Authorization、API Key、请求正文或审批参数。
- `workspace` 必须使用租户允许的路径，不能依赖客户端自行保证隔离。
- `team` 仅对 `multiagent` 有效，并受服务端 allowlist 和生命周期约束。
- 高风险操作必须等待服务生成审批请求，客户端不能自行伪造执行结果。
- 删除全部 Task 前应确认已经导出所需结果；该操作不可恢复。

## 14. 浏览器工作台

`GET /console` 提供任务、Trace 和审批工作台。工作台通过 `POST /console/session` 接收已有身份系统签发的 Bearer Token，验证后设置服务端加密的 `HttpOnly`、`SameSite=Strict` 会话 Cookie；生产访问始终设置 `Secure`，仅本机回环地址上的 HTTP 开发请求允许非 `Secure` Cookie。`GET /console/session` 返回租户和 CSRF Token；`DELETE /console/session` 注销。所有由 Cookie 发起的 `/api` 写请求需要 `X-Console-CSRF`，后端仍逐次执行原有 Bearer 鉴权。浏览器不把 API Key 或 Token 写入本地存储。

启动工作台前，服务端必须通过环境变量 `AI_AGENT_CONSOLE_SESSION_KEY` 提供 32 字节密钥的标准 Base64 编码；没有该密钥时会话创建失败。所有实例须使用同一密钥。工作台会话最长 8 小时，底层 Bearer Token 如果更早失效，会立即拒绝后续请求。生产环境须通过 HTTPS 访问工作台。

当前审批操作沿用原有租户级 API 授权；审批人角色与禁止创建者自批仍需绑定可信身份源后单独配置。不要将租户级凭证共享给无审批权限的人员。
