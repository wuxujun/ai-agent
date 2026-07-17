# AI Agent - Go Runtime Engine

这是一个基于 Go 语言构建的 AI Agent 执行运行时引擎，核心采用 Eino Chain 承载 Planner-Executor（规划器-执行器）任务编排。它允许 AI Agent 根据用户的目标（Goal），在安全的沙箱工作空间（Workspace）中规划并执行文件查找、内容检索以及文件阅读等操作，以最终达成目标。

项目集成了 Gin HTTP 框架、SQLite 数据库以及 OpenTelemetry 链路/指标监控，具备完整的任务生命周期管理与可观测性。

---

## 🏗️ 架构设计

系统设计遵循模块解耦与高可观测性原则，其核心环路逻辑如下：

```mermaid
graph TD
    Client[客户端] -->|POST /api/tasks| API[API 层]
    API -->|创建/执行任务| Engine[Orchestrator Engine]
    Engine -->|1. 规划 (PlanNext)| Planner[Planner]
    Planner -->|GPT-4.1 或 Mock 回退| Engine
    Engine -->|2. 执行 (Execute)| Executor[Executor]
    Executor -->|安全过滤| Policy[Security Policy]
    Executor -->|执行底层命令| Tools[Tools: rg/find/cat]
    Tools -->|收集证据| Engine
    Engine -->|更新任务状态与 Trace| Store[SQLite Store]
    Engine -->|推送监控指标 & 链路| OTel[OpenTelemetry 127.0.0.1:4318]
```

### 核心模块说明
1. **API 层 (`internal/api`)**：提供 Gin-based RESTful API 接口，负责任务的注册、单步调试、批量运行及指标监控。
2. **编排引擎 (`internal/orchestrator`)**：任务运行的主循环，通过 Eino Chain 将预算检查、`Planner` 决策、`Executor` 执行和任务状态更新串联为可组合节点，并把执行状态、发现的线索（Evidence）追加到 `Trace` 中。
3. **规划器 (`internal/planner`)**：
   - `LLMPlanner`：使用大语言模型（GPT-4.1 兼容 API）通过 JSON Schema 控制输出，为 Agent 规划思考逻辑。
   - `MockPlanner`：提供静态本地测试流程。
   - `FallbackPlanner`：提供双路容错机制，当 primary 报错时自动降级到 secondary 并记录指标。
4. **执行器 (`internal/executor`)**：解析 Planner 输出的 Action，并将其分发给具体的工具集（如 `find_files`, `search_text`, `read_file`）。
5. **安全策略 (`internal/policy`)**：进行目录防穿透（Directory Traversal）校验，确保读取的所有路径及执行命令均在 Workspace 授权边界之内。
6. **工具箱 (`internal/tools`)**：封装本地命令（如 `ripgrep`, `find` 以及标准文件读写）。
7. **数据存储 (`internal/store`)**：采用 SQLite 记录任务上下文和所有的步骤 Trace，方便随时断点恢复或复盘。
8. **可观测性 (`internal/telemetry`)**：通过 OpenTelemetry 规范导出 metrics 和 traces 到 OTLP 接收端 (`127.0.0.1:4318`)，包含全链路的 Span Attributes 自定义跟踪与错误捕获。

---

## 📂 项目结构

```text
├── cmd/
│   └── server/
│       └── main.go         # 服务启动入口，初始化数据库与 OTel 监控
├── internal/
│   ├── api/                # REST 接口路由与 API 处理器
│   ├── executor/           # 动作执行器分发
│   ├── metrics/            # 本地性能指标采集器
│   ├── orchestrator/       # 基于 Eino Chain 的任务编排引擎核心
│   ├── planner/            # LLM / Mock / Fallback 规划器实现
│   ├── policy/             # 权限边界与安全策略校验 (防逃逸)
│   ├── store/              # SQLite 数据库持久化实现
│   ├── telemetry/          # OpenTelemetry 链路追踪初始化
│   ├── tools/              # 底层文件、搜索等工具封装
│   ├── types/              # 任务 (Task)、步骤追踪 (StepTrace) 等实体定义
│   └── workspace/          # 工作空间管理
├── go.mod                  # 依赖管理
└── README.md               # 项目说明文档
```

---

## ⚡ 快速开始

### 1. 环境准备
项目依赖本地安装的 `go 1.25` 以上版本，任务编排使用 CloudWeGo Eino，且运行环境推荐包含 `ripgrep (rg)` 和 `find` 命令。

在运行前，请先设置 OpenAI 的 API 密钥（LLM 规划器所需）：
```bash
export OPENAI_API_KEY="your-api-key"
```

### 2. 启动 Telemetry 收集服务 (可选)
如果需要采集链路和监控数据，请在本地启动 OpenTelemetry Collector（或 Jaeger 等），并暴露 HTTP OTLP 接收端口 `4318`。
如果未启动，应用启动后会在控制台打印 `[OTel Error]` 相关的连接异常日志，但不影响核心业务功能运行。

### 3. 运行服务
在项目根目录下编译并启动：
```bash
go run ./cmd/server
```
服务默认监听在 `http://127.0.0.1:8080`。

任务编排入口可通过环境变量切换：
```bash
export AI_AGENT_ORCHESTRATOR_MODE=eino    # 默认，使用 Eino Chain 编排
export AI_AGENT_ORCHESTRATOR_MODE=legacy  # 使用旧版 Engine.Next 直接编排
export AI_AGENT_ORCHESTRATOR_MODE=adk     # 使用 Google ADK for Go 编排
```

---

## 🔗 接口 API 指南

### 1. 创建任务
* **请求方式**：`POST /api/tasks`
* **Payload**：
```json
{
  "id": "task-001",
  "goal": "Find the key secret in config files",
  "workspace": "./workspace",
  "max_steps": 5,
  "tool_budget": 5
}
```

### 2. 单步执行任务
* **请求方式**：`POST /api/tasks/:id/run`
* **Query 参数**：`stream=true`（可选，设置为 `true` 时以 Server-Sent Events 格式流式输出执行过程中的 Token 及 StepTrace）
* **说明**：执行一次 `Plan-Execute` 闭环，默认返回该步执行后的任务最新状态和 Trace 详情。

### 3. 运行全部任务（异步/流式）
* **请求方式**：`POST /api/tasks/:id/run-all`
* **Query 参数**：`stream=true`（可选，设置为 `true` 时以 Server-Sent Events 格式同步流式输出执行过程，直至任务到达终态）
* **返回**：默认返回 `202 Accepted`（任务在后台异步执行）；若指定 `stream=true`，则返回 `200 OK` 并以 `text/event-stream` 格式同步推送事件流。
* **说明**：服务端循环执行，直至任务到达终态 `completed` 或 `failed`（达到最大步数、工具预算耗尽，或 Planner 判定已达成目标）。**权威状态以轮询 `GET /api/tasks/:id` 为准。**
* **并发保护**：同一任务重复触发返回 `409 Conflict`；通过 DB 原子状态转换防止多实例并发执行。

### 4. 获取任务详情
* **请求方式**：`GET /api/tasks/:id`

### 5. 任务列表
* **请求方式**：`GET /api/tasks`
* **Query 参数**：`status`（可选，按状态过滤）、`limit`（默认 50）、`offset`（默认 0）。

### 6. 实时事件流 (SSE)
* **请求方式**：`GET /api/tasks/:id/stream`
* **Content-Type**：`text/event-stream`
* **说明**：每完成一步推送一条 `StepEvent` JSON（`data: {...}\n\n`），任务到达终态时推送终态事件后关闭连接。已处于终态的任务订阅时立即返回一条快照。
* **可靠性**：实时事件为**尽力推送**（慢消费者可能丢事件）；服务端每 15s 轮询存储作为兜底，确保终态事件最终一定送达，并兼作 keep-alive。客户端如需精确状态，应以 `GET /api/tasks/:id` 为权威来源。

### 7. 审批 / 拒绝高风险动作
* **请求方式**：`POST /api/tasks/:id/approve` ｜ `POST /api/tasks/:id/reject`
* **说明**：当任务因高风险工具调用进入 `awaiting_approval` 时，用于放行或拒绝该动作。

### 8. 取消任务
* **请求方式**：`DELETE /api/tasks/:id/cancel`
* **说明**：取消运行中的任务。本进程内运行则触发 context 取消；否则在 DB 中将 `running` 任务标记为 `failed`。

### 9. 删除任务
* **单个任务**：`DELETE /api/tasks/:id`，同时删除关联的执行 Trace、租约和该任务生成的长期记忆。运行中或等待审批的任务必须先取消，否则返回 `409 Conflict`。
* **清空任务**：`DELETE /api/tasks?confirm=true`，仅管理员可调用；清空所有任务及关联数据并返回 `deleted` 数量。存在本实例正在执行的任务时返回 `409 Conflict`。

### 10. Memory 管理
* **查询列表**：`GET /api/memories?limit=50&offset=0`。普通租户只返回自己的数据；管理员可用 `tenant_id` 筛选。响应包含 `embedding_dimensions`，不返回完整向量。
* **删除单条**：`DELETE /api/memories/:id`。普通租户只能删除自己的 Memory。
* **清空全部**：`DELETE /api/memories?confirm=true`，仅管理员可调用；可增加 `tenant_id` 只清空指定租户。

### 11. 获取本地监控指标
* **请求方式**：`GET /api/metrics`

### 12. 热重载配置
* **请求方式**：`POST /api/config/reload`
* **说明**：不重启进程即可重新读取配置文件与环境变量（用于 API Key 轮换、模型/超时调优）。返回脱敏后的变更 diff，API Key 以 `***` 显示。

### 健康检查
* **请求方式**：`GET /ping` → `{"message":"pong"}`
