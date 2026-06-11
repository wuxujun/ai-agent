# AI Agent 项目概览与关键分析

**生成日期**：2026-06-09

---

## 1. 项目定位
本仓库实现了基于 **Go 1.25** 的 *AI Agent 执行运行时*，核心功能是接收用户的任务目标（Goal），在受控工作空间（Workspace）中通过 **Plan‑Execute‑Observe** 循环完成文件查找、搜索与读取等操作。系统支持四种编排模式：Eino、Google ADK、Legacy 与 Step。

## 2. 技术栈要点
| 类别 | 组件 | 用途 |
|---|---|---|
| 编程语言 | Go 1.25 | 项目主实现语言 |
| HTTP 框架 | Gin v1.12.0 | REST API 服务 |
| 编排框架 | CloudWeGo Eino、Google ADK | 多步决策与工具调用 |
| 大模型客户端 | Google GenAI SDK (Gemini) / OpenAI SDK | LLM 交互 |
| 持久化 | SQLite（默认）/ PostgreSQL / Redis | Task 与 Trace 存储 |
| 可观测性 | OpenTelemetry (OTel) | 链路追踪 & 指标上报 |
| 工具层 | `find`、`ripgrep (rg)`、自研 Runner | 文件检索与命令执行 |

## 3. 目录结构概览
```
.
├─ cmd/server/main.go                # 程序入口，初始化 OTel、Router、Engine
├─ internal/
│   ├─ api/        # HTTP 控制器 (任务创建、运行、查询)
│   ├─ executor/   # DefaultExecutor 将 Planner 决策路由到工具
│   ├─ orchestrator/ # 四种编排实现与状态机
│   ├─ planner/    # LLM Prompt 构建、双路容错 (Primary+Fallback)
│   ├─ policy/     # 工作空间安全校验、命令白名单
│   ├─ store/      # SQLite/Postgres/Redis 持久化实现
│   ├─ telemetry/  # OTel 初始化
│   └─ tools/      # Find, RG, Read, Runner (带超时 & 白名单)
├─ workspace/demo/ # 示例工作空间，含 sample.txt 与若干 markdown 文件
└─ records/        # 项目分析、路标文档（中文）
```

## 4. 核心运行流程（简化版）
1️⃣ **任务创建**：`POST /api/tasks` → 校验 Workspace → 写入 Store (status=created)。
2️⃣ **触发执行**：`POST /api/tasks/:id/run` → Engine.Next 或 RunAll。
3️⃣ **预算检查**：StepCount 与 ToolBudget 上限控制，超出即结束任务。
4️⃣ **Planner 决策**：构造 SystemPrompt + UserPrompt → 调用 LLM (Gemini/OpenAI) → JSON Schema 输出 → 验证合法性。
5️⃣ **Executor 执行**：依据 `Action`（find_files、search_text、read_file）调用对应工具；前置 Policy 检查路径安全与命令白名单。
6️⃣ **状态持久化 & 观测**：更新 Task.StepCount/ToolBudget → 写入 Store → 在每个关键点记录 OTel Span 与 Metrics（task run, fallback, durations 等）。

## 5. 数据模型要点
```go
type Task struct {
    ID, Goal, Workspace string
    Status               TaskStatus // created/running/completed/failed
    MaxSteps, StepCount  int
    ToolBudget           int
    Trace []StepTrace
    FinalAnswer          string
}

type StepTrace struct {
    Step        int
    Action      string // find_files / search_text / read_file
    Query       string
    Observation string
    Evidence    []Evidence
}
```
> 详细定义见 `internal/types/task.go`。

## 6. 可观测性设计
- **Tracing**：Gin 中间件 → Engine → Planner/Executor → Store，所有 Span 标记关键属性 (`agent.task.id`, `action`, `step_count_after`).
- **Metrics**：任务总数、完成数、Planner/Fallback 次数、执行时长等在 `internal/metrics/metrics.go`。默认推送到本地 OTLP 接收端 (`127.0.0.1:4318`)。

## 7. 已知改进方向（精选）
| 编号 | 建议 | 价值 | 实施难度 |
|---|---|---|---|
| ✅1 | **缓存 Eino 编译链**：避免每次 `runEinoNext` 重复编译，提高并发性能。 | 高 | 中 |
| ✅2 | **ADK 会话持久化**：在服务启动时创建一次 Agent，重复使用以降低握手开销。 | 高 | 中 |
| ✅3 | **增量 Trace 写入**：改用 `INSERT OR IGNORE` + 唯一索引，避免全表删除/写放大。 | 中 | 低 |
| ✅4 | **配置精简**：让 ADK 模式仅依赖 Gemini 配置，去掉对 OpenAI KEY 的硬性检查。 | 中 | 低 |
| ✅5 | **强化 Workspace 防逃逸**：在 `ValidateWorkspace` 中解析软链接、使用绝对路径前缀校验，以防高级目录穿透。 | 高 | 中 |

> 完整的技术细节与实现代码分析请参阅 `PROJECT_ANALYSIS.md`（已在仓库根目录）。

---

*本文件为项目快速概览，供新成员入门或管理层评审使用。*
