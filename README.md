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
export AI_AGENT_ORCHESTRATOR=eino    # 默认，使用 Eino Chain 编排
export AI_AGENT_ORCHESTRATOR=legacy  # 使用旧版 Engine.Next 直接编排
export AI_AGENT_ORCHESTRATOR=adk     # 使用 Google ADK for Go 编排
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
* **说明**：执行一次 `Plan-Execute` 闭环，并返回该步执行后的任务最新状态和 Trace 详情。

### 3. 运行全部任务
* **请求方式**：`POST /api/tasks/:id/run-all`
* **说明**：自动循环执行，直至任务标记为 `completed`（达到最大步数、工具预算耗尽，或 Planner 判定已达成目标）。

### 4. 获取任务详情
* **请求方式**：`GET /api/tasks/:id`

### 5. 获取本地监控指标
* **请求方式**：`GET /api/metrics`
