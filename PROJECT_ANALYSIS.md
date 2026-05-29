# AI Agent 项目分析总结

生成日期：2026-05-29

## 1. 项目定位

本项目是一个使用 Go 编写的 AI Agent 运行时服务。它以任务为中心，接收用户目标后在指定工作空间内执行文件查找、文本搜索、文件读取等动作，并把每一步执行结果持久化为 Trace。

当前项目已经从单一 Planner-Executor 环路演进为“多编排入口”的 Agent Runtime：

- 默认使用 CloudWeGo Eino Chain 编排任务步骤。
- 保留 Legacy Engine 直接编排逻辑，便于回退和对比。
- 新增 Google ADK for Go 编排模式，用 ADK Agent 和工具回调驱动执行。
- HTTP API、SQLite Store、工具层、Policy、Metrics/Telemetry 仍作为公共基础设施复用。

## 2. 技术栈

| 类别 | 当前选择 | 说明 |
| --- | --- | --- |
| 语言 | Go 1.25 | `go.mod` 指定版本 |
| Web 框架 | Gin | 提供 REST API |
| 默认编排 | CloudWeGo Eino v0.9.2 | 用 `compose.Chain` 串联任务节点 |
| 可选编排 | Google ADK for Go v1.3.0 | 用 ADK Agent、Tool、Runner 执行任务 |
| LLM 接入 | OpenAI Responses API / Gemini | Legacy/Eino 使用 OpenAI Planner；ADK 使用 Gemini Model |
| 数据库 | SQLite | 使用 `modernc.org/sqlite`，本地 `data/agent.db` |
| 可观测性 | OpenTelemetry | OTLP HTTP 默认发往 `127.0.0.1:4318` |
| 本地工具 | `find`、`rg`、文件读取 | 用于工作空间检索和读取 |

## 3. 当前目录结构

```text
.
├── cmd/server/main.go                 # 服务启动入口
├── internal/api                       # Gin 路由、Handler、中间件
├── internal/executor                  # Planner 决策到工具调用的执行层
├── internal/metrics                   # 内存指标与 OTel 指标封装
├── internal/orchestrator
│   ├── engine.go                      # Engine 主入口、Legacy 编排、RunAll
│   ├── eino.go                        # Eino Chain 编排实现
│   ├── adk.go                         # Google ADK 编排实现
│   ├── state.go                       # 任务状态迁移和管理
│   ├── eino_test.go                   # Eino/Legacy 模式测试
│   ├── adk_test.go                    # ADK 模式测试
│   └── state_test.go                  # 状态迁移单元测试
├── internal/planner                   # LLM、Mock、Fallback Planner
├── internal/policy                    # 工作空间和命令安全策略
├── internal/store                     # SQLite 持久化
├── internal/telemetry                 # OpenTelemetry 初始化
├── internal/tools                     # find、rg、read_file 等工具封装
├── internal/workspace                 # 早期 Agent 原型式抽象
├── internal/types/task.go                # Task、StepTrace、Evidence 类型
├── workspace/demo                     # 示例工作空间
├── PROJECT_ANALYSIS.md                # 当前分析文档
├── README.md
├── go.mod
└── go.sum
```

## 4. 启动与入口配置

服务入口是 `cmd/server/main.go`，启动过程为：

1. 初始化 OpenTelemetry。
2. 打开 SQLite 数据库 `data/agent.db`。
3. 检查 `OPENAI_API_KEY`，为空会直接退出。
4. 创建 OpenAI `LLMPlanner`、`MockPlanner` 和 `FallbackPlanner`。
5. 读取 `AI_AGENT_ORCHESTRATOR` 决定编排模式。
6. 创建 `orchestrator.Engine`。
7. 注册 Gin 路由并监听 `:8080`。

当前支持的编排模式：

| 环境变量 | 行为 |
| --- | --- |
| 未设置或 `eino` | 默认，使用 Eino Chain 编排 |
| `legacy` | 使用旧版 `Engine.Next` 直接编排 |
| `adk` | 使用 Google ADK Agent 编排 |

示例：

```bash
export OPENAI_API_KEY="your-openai-key"
export AI_AGENT_ORCHESTRATOR=eino
go run ./cmd/server
```

ADK 模式额外会尝试读取：

```bash
export GEMINI_API_KEY="your-gemini-key"
export GEMINI_MODEL="gemini-2.5-flash"
export AI_AGENT_ORCHESTRATOR=adk
```

如果 `GEMINI_API_KEY` 和 `GOOGLE_API_KEY` 都为空，ADK 当前代码会回退尝试 `OPENAI_API_KEY` 作为 Gemini client APIKey。

## 5. 核心运行链路

### 5.1 API 层

主要接口：

| 方法 | 路径 | 功能 |
| --- | --- | --- |
| `POST` | `/api/tasks` | 创建任务 |
| `POST` | `/api/tasks/:id/run` | 执行单步 |
| `POST` | `/api/tasks/:id/run-all` | 执行直到完成 |
| `GET` | `/api/tasks/:id` | 查询任务 |
| `GET` | `/api/metrics` | 查询内存指标快照 |
| `GET` | `/ping` | 健康检查 |

创建任务时会设置默认值：

- `max_steps <= 0` 时设为 5。
- `tool_budget <= 0` 时设为 5。

### 5.2 Engine 分发

`Engine.Next` 是编排入口，根据 `Engine.Mode` 分发：

```go
switch e.Mode {
case "", ModeEino:
    return e.runEinoNext(ctx, task)
case ModeLegacy:
    return e.runLegacyNext(ctx, task)
case ModeAdk:
    return e.runAdkNext(ctx, task)
}
```

`RunAll` 不区分具体模式，它循环调用 `Next`，直到任务状态变为 `completed`。

### 5.3 Eino 模式

文件：`internal/orchestrator/eino.go`

Eino 模式用 `compose.NewChain[*einoStepState, *types.Task]()` 编排四个 Lambda 节点：

1. `budget_guard`：检查最大步数和工具预算。
2. `planner`：调用 `Planner.PlanNext`。
3. `executor`：调用 `Executor.Execute`。
4. `task_output`：返回最终更新后的 `Task`。

它复用现有 Planner、Executor、Metrics、Trace 更新逻辑，行为上与 Legacy 模式保持接近。

### 5.4 Legacy 模式

文件：`internal/orchestrator/engine.go`

Legacy 模式保留原始直接编排逻辑：

1. 检查预算和最大步数。
2. 调用 Planner。
3. 如果 Planner 停止，则写入 FinalAnswer。
4. 否则调用 Executor。
5. 追加 StepTrace。
6. 更新 StepCount、ToolBudget 和 Status。

该模式适合作为 Eino 改造期间的回退入口。

### 5.5 ADK 模式

文件：`internal/orchestrator/adk.go`

ADK 模式创建 ADK `llmagent.Agent`，注册三个 function tool：

- `find_files`
- `search_text`
- `read_file`

工具回调会在执行后把结果转换为项目自身的 `types.StepTrace`，并更新：

- `Trace`
- `StepCount`
- `ToolBudget`
- `Status`

ADK 模式的最终答案来自 ADK Runner 的 final response。

## 6. Planner、Executor 与 Tools

### Planner

目录：`internal/planner`

当前包括：

- `LLMPlanner`：调用 OpenAI Responses API，使用 JSON Schema 约束输出。
- `MockPlanner`：按步骤返回固定动作。
- `FallbackPlanner`：Primary 失败后降级到 Secondary。

允许的 Planner 动作：

- `find_files`
- `search_text`
- `read_file`
- `none`

### Executor

目录：`internal/executor`

`DefaultExecutor` 将 Planner 决策分发到工具层：

| Action | 工具 |
| --- | --- |
| `find_files` | `tools.FindFiles` |
| `search_text` | `tools.SearchWithRG` |
| `read_file` | `tools.ReadFile` |

### Tools

目录：`internal/tools`

工具层通过 `RunCommand` 执行外部命令，当前超时为 3 秒。命令白名单由 `internal/policy` 控制。

## 7. 数据模型与存储

核心类型在 `internal/types/task.go`：

- `Task`：任务状态、预算、工作空间、Trace 和最终答案。
- `StepTrace`：单步动作、查询、观察和证据。
- `Evidence`：搜索命中的文件路径、行内容和查询词。

SQLite Store 包含两张表：

- `tasks`
- `traces`

`SaveFullTask` 当前会 upsert 任务主体，然后删除并重写该任务的全部 Trace。

## 8. 可观测性

项目包含两套观测能力：

- `internal/metrics`：内存计数器和 OTel Meter，包括 planner、executor、run_all、completed、fallback 等指标。
- `internal/telemetry`：初始化 OTel trace 和 metric exporter。

主要链路包括：

- API 请求链路。
- Engine `next` 和 `run_all`。
- Planner 调用。
- Executor 调用。
- Store 保存和读取。

Eino、Legacy、ADK 模式会在 span attribute 中标记不同编排模式。

## 9. 测试现状

已执行：

```bash
go test ./...
```

结果：通过。

当前有测试覆盖：

- Eino 模式正常 Planner -> Executor 执行。
- Legacy 模式正常 Planner -> Executor 执行。
- Planner stop 时不调用 Executor。
- 预算耗尽时不调用 Planner/Executor。
- 非法编排模式返回错误。
- ADK 模式使用 mock LLM 返回最终答案。

缺口：

- API Handler 测试缺失。
- Store 持久化测试缺失。
- Policy 路径安全测试缺失。
- Executor 工具分发测试缺失。
- ADK 工具调用 Trace 转换逻辑测试较弱。

## 10. 当前优势

- 编排入口可切换，Eino/Legacy/ADK 可以并存验证。
- Eino 模式没有破坏原有 Planner 和 Executor 抽象。
- Legacy 模式可作为稳定回退路径。
- ADK 模式引入了 Agent 工具调用范式，为后续多工具、多轮推理扩展留了入口。
- SQLite 和 Trace 让任务过程可追踪、可复盘。
- OTel 和内存指标已覆盖核心路径。

## 11. 风险与改进建议

### 11.1 ADK 模式仍偏实验

ADK 模式每次 `Next` 都创建 Tool、Agent、Runner 和 session service，开销较大。建议后续把 ADK Agent 初始化挪到 Engine 构造阶段或工厂层，并复用 Runner。

### 11.2 ADK API Key 语义不清

ADK 使用 Gemini Model，但当前在 Gemini/Google key 不存在时会回退使用 `OPENAI_API_KEY`。这容易造成配置误导。建议拆分为明确的 `GEMINI_API_KEY` 或 `GOOGLE_API_KEY` 要求。

### 11.3 main.go 仍强依赖 OPENAI_API_KEY

即使只运行 ADK 模式，`main.go` 也会先要求 `OPENAI_API_KEY`。这会限制 ADK 独立运行。建议按模式初始化不同 Planner/Model，或允许 ADK 模式跳过 OpenAI Planner 初始化。

### 11.4 Eino Chain 每步动态编译

`runEinoNext` 当前每次调用都会 `compileEinoStepChain`。功能正确，但有额外开销。建议把编译后的 Runnable 缓存到 Engine，或在启动时构建。

### 11.5 Workspace 安全策略仍需增强

`ValidateWorkspace` 当前只拒绝 `.`、`/` 和包含 `..` 的路径。建议：

- 转为绝对路径后校验。
- 限制到明确允许的根目录。
- 检查目录存在性。
- 处理 symlink，避免逃逸。

### 11.6 Trace 数据写入方式可优化

`SaveFullTask` 每次删除并重写全部 Trace，简单但有写放大。建议改成事务内增量追加 Trace。

### 11.7 工具结果结构化程度不一致

Legacy/Eino 的 `find_files` 只记录数量，没有把文件列表保存为 Evidence。ADK 的工具结果转换也依赖 map/json 转换。建议统一工具结果模型，让 Planner 和用户都能看到更稳定的结构化上下文。

### 11.8 仓库存在本地文件和构建产物

当前工作区可以看到：

- `server`
- `data/agent.db`
- `.DS_Store`
- `internal/.DS_Store`
- `workspace/.DS_Store`

建议确认 `.gitignore` 并清理不应入库的构建产物、本地数据库和系统文件。

### 11.9 internal/workspace 职责不清

`internal/workspace` 下仍有早期 `package agent` 风格抽象，与当前主链路不直接衔接。建议明确是删除、迁移还是接入主架构。

## 12. 推荐下一步

优先级建议：

1. 修正启动配置：按 `AI_AGENT_ORCHESTRATOR` 分模式初始化依赖，避免 ADK 模式强依赖 OpenAI key。
2. 缓存 Eino compiled Runnable，减少单步执行开销。
3. 为 Policy、Store、Executor、API 增加基础测试。
4. 清理 `internal/workspace` 和本地构建/数据库文件。
5. 统一工具输出和 Trace 结构。
6. 将 `run-all` 改成异步任务，避免 HTTP 10 秒超时限制。
7. 增强 workspace sandbox，尤其是 symlink 和绝对路径边界。

## 13. 总结

当前项目已经具备一个可扩展的 AI Agent Runtime 雏形：HTTP API 负责任务生命周期，SQLite 保存状态和 Trace，Planner/Executor/Tools 执行工作空间检索任务，Eino/Legacy/ADK 三种编排方式支持并行演进。

短期最值得处理的是配置初始化、Eino Chain 缓存、安全测试和 ADK 模式稳定性。完成这些后，项目会更适合继续扩展更多工具、更复杂的任务流，以及生产化的异步任务执行模型。
