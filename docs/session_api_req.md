# Session API 请求与响应记录

## 1. 测试信息

- 测试日期：2026-08-18
- 服务地址：`http://127.0.0.1:8088`
- 测试租户：`default`
- 测试 Session ID：`codex-session-smoke-20260818-a7f3`
- 最终状态：`archived`
- Content-Type：`application/json`

受保护接口需要认证。本文统一使用占位符，不记录真实凭证：

```http
X-API-Key: <API_KEY>
```

如果部署使用 Bearer 认证，应改为：

```http
Authorization: Bearer <ACCESS_TOKEN>
```

## 2. 健康检查

### 2.1 Ping

```http
GET /ping HTTP/1.1
Host: 127.0.0.1:8088
```

返回状态：`200 OK`

```json
{
  "message": "pong"
}
```

### 2.2 Ready

```http
GET /ready HTTP/1.1
Host: 127.0.0.1:8088
```

返回状态：`200 OK`

实际响应包含 `ready`、`llm_scenes`、`teams` 和 `wiki` 等运行状态。本次关键结果为：

```json
{
  "llm_readiness_mode": "gateway",
  "llm_verified": false,
  "ready": true,
  "teams": {
    "configured": true,
    "healthy": true,
    "active_team": "wiki_suggest",
    "team_count": 7,
    "invalid_reference_count": 0,
    "error": ""
  },
  "wiki": {
    "configured": true,
    "error": "",
    "healthy": true,
    "required": false
  }
}
```

## 3. 认证检查

### 3.1 未携带凭证查询 Session

```http
GET /api/sessions HTTP/1.1
Host: 127.0.0.1:8088
```

返回状态：`401 Unauthorized`

```json
{
  "error": "unauthorized: invalid or missing credential"
}
```

## 4. 创建 Session

### 4.1 请求

```http
POST /api/sessions HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
Content-Type: application/json

{
  "id": "codex-session-smoke-20260818-a7f3",
  "title": "Codex session API smoke test"
}
```

请求参数：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `id` | string | 否 | Session ID。省略时由服务生成；自定义值必须符合 request ID 格式。 |
| `title` | string | 否 | Session 标题。空值默认使用 `New session`，最多 200 个字符。 |

返回状态：`201 Created`

```json
{
  "id": "codex-session-smoke-20260818-a7f3",
  "tenant_id": "default",
  "title": "Codex session API smoke test",
  "status": "active",
  "created_at": "2026-08-18T15:06:40.691586Z",
  "updated_at": "2026-08-18T15:06:40.691715Z"
}
```

新 Session 的初始状态固定为 `active`，`tenant_id` 取自认证主体，不能由请求正文指定。

## 5. 查询 Session 详情

### 5.1 请求

```http
GET /api/sessions/codex-session-smoke-20260818-a7f3 HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
```

返回状态：`200 OK`

```json
{
  "id": "codex-session-smoke-20260818-a7f3",
  "tenant_id": "default",
  "title": "Codex session API smoke test",
  "status": "active",
  "created_at": "2026-08-18T15:06:40.691586Z",
  "updated_at": "2026-08-18T15:06:40.691715Z"
}
```

## 6. 查询 Session 列表

### 6.1 查询 active Session

```http
GET /api/sessions?status=active&limit=100&offset=0 HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
```

Query 参数：

| 参数 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `status` | string | 否 | 可选值为 `active` 或 `archived`。 |
| `limit` | integer | 否 | 返回数量上限，必须大于 0。 |
| `offset` | integer | 否 | 分页偏移，必须大于或等于 0。 |

返回状态：`200 OK`

```json
{
  "count": 2,
  "limit": 100,
  "offset": 0,
  "sessions": [
    {
      "id": "codex-session-smoke-20260818-a7f3",
      "tenant_id": "default",
      "title": "Codex session API smoke test",
      "status": "active",
      "created_at": "2026-08-18T15:06:40.691586Z",
      "updated_at": "2026-08-18T15:06:40.691715Z"
    },
    {
      "id": "smoke-session-260810-01",
      "tenant_id": "default",
      "title": "API smoke test updated",
      "status": "active",
      "created_at": "2026-08-10T09:42:24.678308Z",
      "updated_at": "2026-08-10T10:41:47.38389Z"
    }
  ]
}
```

## 7. 更新 Session

### 7.1 更新标题

```http
PATCH /api/sessions/codex-session-smoke-20260818-a7f3 HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
Content-Type: application/json

{
  "title": "Codex session API smoke test updated"
}
```

请求参数：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `title` | string | 否 | 非空标题，最多 200 个字符。 |
| `status` | string | 否 | 可选值为 `active` 或 `archived`。 |

返回状态：`200 OK`

```json
{
  "id": "codex-session-smoke-20260818-a7f3",
  "tenant_id": "default",
  "title": "Codex session API smoke test updated",
  "status": "active",
  "created_at": "2026-08-18T15:06:40.691586Z",
  "updated_at": "2026-08-18T15:06:40.720716Z"
}
```

也可以通过 PATCH 归档或重新激活：

```json
{
  "status": "archived"
}
```

```json
{
  "status": "active"
}
```

## 8. 查询 Session 关联任务

### 8.1 请求

```http
GET /api/sessions/codex-session-smoke-20260818-a7f3/tasks?limit=20&offset=0 HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
```

Query 参数：

| 参数 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `status` | string | 否 | 按 Task 状态过滤。 |
| `limit` | integer | 否 | 返回数量上限。 |
| `offset` | integer | 否 | 分页偏移。 |

返回状态：`200 OK`

本次 Session 没有 Task，实际返回：

```json
{
  "count": 0,
  "limit": 20,
  "offset": 0,
  "tasks": null
}
```

注意：空结果当前返回 `tasks: null`，而不是 `tasks: []`。这与 Session Memory 接口的空数组行为不一致，建议后续统一为空数组。

## 9. 查询 Session 关联 Memory

### 9.1 请求

```http
GET /api/sessions/codex-session-smoke-20260818-a7f3/memories?limit=20&offset=0 HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
```

Query 参数：

| 参数 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `limit` | integer | 否 | 返回数量上限。 |
| `offset` | integer | 否 | 分页偏移。 |

返回状态：`200 OK`

```json
{
  "count": 0,
  "limit": 20,
  "memories": [],
  "offset": 0
}
```

Memory 列表项在有数据时包含以下字段：

| 字段 | 说明 |
|---|---|
| `id` | Memory ID |
| `tenant_id` | 所属租户 |
| `session_id` | 所属 Session |
| `task_id` | 来源 Task |
| `goal` | 来源任务目标 |
| `final_answer` | 最终回答 |
| `key_findings` | 关键发现 |
| `timestamp` | Memory 时间 |
| `embedding_dimensions` | Embedding 维度；不会返回完整向量 |

## 10. 归档 Session

### 10.1 请求

```http
POST /api/sessions/codex-session-smoke-20260818-a7f3/archive HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
```

该接口不需要请求正文。

返回状态：`200 OK`

```json
{
  "id": "codex-session-smoke-20260818-a7f3",
  "tenant_id": "default",
  "title": "Codex session API smoke test updated",
  "status": "archived",
  "created_at": "2026-08-18T15:06:40.691586Z",
  "updated_at": "2026-08-18T15:06:40.752588Z"
}
```

### 10.2 查询 archived Session

```http
GET /api/sessions?status=archived&limit=100&offset=0 HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
```

返回状态：`200 OK`

```json
{
  "count": 1,
  "limit": 100,
  "offset": 0,
  "sessions": [
    {
      "id": "codex-session-smoke-20260818-a7f3",
      "tenant_id": "default",
      "title": "Codex session API smoke test updated",
      "status": "archived",
      "created_at": "2026-08-18T15:06:40.691586Z",
      "updated_at": "2026-08-18T15:06:40.752588Z"
    }
  ]
}
```

## 11. 错误响应验证

### 11.1 重复创建相同 Session

```http
POST /api/sessions HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
Content-Type: application/json

{
  "id": "codex-session-smoke-20260818-a7f3",
  "title": "duplicate"
}
```

返回状态：`409 Conflict`

```json
{
  "error": "session already exists"
}
```

### 11.2 非法 Session 状态

```http
PATCH /api/sessions/codex-session-smoke-20260818-a7f3 HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
Content-Type: application/json

{
  "status": "invalid"
}
```

返回状态：`400 Bad Request`

```json
{
  "error": "invalid session status"
}
```

### 11.3 查询不存在的 Session

```http
GET /api/sessions/codex-session-does-not-exist HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
```

返回状态：`404 Not Found`

```json
{
  "error": "session not found"
}
```

### 11.4 向已归档 Session 创建 Task

```http
POST /api/tasks HTTP/1.1
Host: 127.0.0.1:8088
X-API-Key: <API_KEY>
Content-Type: application/json

{
  "id": "codex-task-archived-session-check-a7f3",
  "session_id": "codex-session-smoke-20260818-a7f3",
  "goal": "session archive validation only",
  "workspace": "./workspace/demo",
  "max_steps": 1,
  "tool_budget": 1
}
```

返回状态：`409 Conflict`

```json
{
  "error": "session is archived"
}
```

请求在创建 Task 前即被拒绝，没有执行 LLM 或工具调用。

## 12. curl 示例

```bash
SESSION_API_KEY='<API_KEY>'
SESSION_ID='codex-session-example'

curl -sS \
  -H "X-API-Key: ${SESSION_API_KEY}" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"${SESSION_ID}\",\"title\":\"Session API example\"}" \
  http://127.0.0.1:8088/api/sessions

curl -sS \
  -H "X-API-Key: ${SESSION_API_KEY}" \
  "http://127.0.0.1:8088/api/sessions/${SESSION_ID}"

curl -sS \
  -X PATCH \
  -H "X-API-Key: ${SESSION_API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Updated session title"}' \
  "http://127.0.0.1:8088/api/sessions/${SESSION_ID}"

curl -sS \
  -H "X-API-Key: ${SESSION_API_KEY}" \
  "http://127.0.0.1:8088/api/sessions/${SESSION_ID}/tasks?limit=20&offset=0"

curl -sS \
  -H "X-API-Key: ${SESSION_API_KEY}" \
  "http://127.0.0.1:8088/api/sessions/${SESSION_ID}/memories?limit=20&offset=0"

curl -sS \
  -X POST \
  -H "X-API-Key: ${SESSION_API_KEY}" \
  "http://127.0.0.1:8088/api/sessions/${SESSION_ID}/archive"
```

## 13. 测试结论

以下功能均按预期工作：

- Session 创建、详情查询、列表和状态筛选；
- 标题及状态更新；
- Session 关联 Task 和 Memory 查询；
- Session 归档；
- 未认证、重复 ID、非法状态、不存在资源等错误处理；
- 已归档 Session 拒绝创建新 Task。

当前确认的接口一致性问题只有一项：空 Task 列表返回 `null`，建议统一为 `[]`。
