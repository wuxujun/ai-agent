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

- `status`、`final_answer`、`trace`；
- `team`、`team_selection_source`、`team_config_digest`；
- `step_count`、`tool_budget`、`token_budget`；
- `llm_calls`、`llm_estimated_cost_usd`；
- `answer_audit`（启用答案流水线时）。

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
  "status": "completed",
  "final_answer": "...",
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
