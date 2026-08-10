# AI Agent — Go 运行时引擎

> 基于 Go 语言构建的生产级、多 LLM AI Agent 执行运行时。它在安全沙箱工作区内，通过双工作流多智能体引擎和完整的答案质量审查流水线，编排自主研究、代码分析与任务执行。

[![Go 1.25](https://img.shields.io/badge/Go-1.25-blue)](https://go.dev/) [![License: MIT](https://img.shields.io/badge/License-MIT-green)](LICENSE)

---

## ✨ 核心亮点

- **双 Multi-Agent 工作流** — `planner_researcher_writer`（开放研究任务）；`planner_critic_executor_verifier`（高风险执行，内置强制计划评审与结果验证）。
- **自适应路由** — 根据任务复杂度、意图、计划规模或高风险工具存在与否，自动路由至 Reviewed 拓扑，无需额外 LLM 调用。
- **多 LLM 提供商** — OpenAI（Responses API & Chat）、Gemini、Ollama、LiteLLM 网关、Google ADK。支持按 Scene 覆盖模型、熔断器、重试预算和成本追踪。
- **答案质量流水线** — 并行审计：事实时效性、数值一致性、不确定性校准、安全防护，均可按租户独立配置。
- **RAG / 长期记忆** — JIT 或预取检索，支持 MCP 或 REST；向量搜索（进程内余弦相似度或 pgvector）；带冲突解决的长期任务记忆。
- **多 MCP 工具** — 从多个 MCP Streamable HTTP 服务发现工具，按服务设置命名空间，并隔离可选服务故障。
- **结构化 Workspace 工具** — 支持只读 SQLite/JSON 查询、需审批的补丁应用和受限 Go 测试执行，统一经过策略与中间件层。
- **完整可观测性** — OpenTelemetry 链路追踪 + 指标（OTLP 或标准输出）、结构化 JSON 日志（每日轮转）、本地指标接口。
- **安全防护** — Workspace 边界强制校验、高风险工具人工审批、Prompt 注入检测、敏感信息脱敏、URL 访问白名单。
- **热重载配置** — 通过 `POST /api/config/reload` 无需重启即可重新加载配置与 API Key。

---

## 🏗️ 架构设计

```
┌─────────────┐   POST /api/tasks   ┌──────────────────────────────────────────────────┐
│   客户端     │ ──────────────────▶ │               API 层（Gin）                       │
└─────────────┘                     │  任务 · 运行 · 流式 · 审批 · SSE · 记忆 · 配置    │
                                    └────────────────────┬─────────────────────────────┘
                                                         │
                                    ┌────────────────────▼─────────────────────────────┐
                                    │              编排引擎（Orchestrator）              │
                                    │   Eino Chain · Legacy · ADK · Multi-Agent 模式   │
                                    └──┬──────────────────────────────────────┬─────────┘
                                       │ 规划                                 │ 执行
                               ┌───────▼──────┐                      ┌───────▼──────────┐
                               │   规划器      │                      │  研究员 /        │
                               │  LLM · Mock  │                      │  执行器 Agent    │
                               │  Fallback    │                      └───────┬──────────┘
                               └───────┬──────┘                             │
                                       │（Reviewed 工作流）         ┌───────▼──────────┐
                               ┌───────▼──────┐                    │  安全策略层       │
                               │   评审师      │                    │  工具注册表       │
                               │  计划评审     │                    │  审批门控         │
                               └───────┬──────┘                    └───────┬──────────┘
                                       │                           ┌───────▼──────────┐
                               ┌───────▼──────┐                   │  答案质量流水线   │
                               │   验证师      │                   │  事实·数值        │
                               │  双调用验证   │                   │  安全·不确定性    │
                               └──────────────┘                   └──────────────────┘
                                                         │
                               ┌─────────────────────────▼────────────────────────────┐
                               │   存储层（SQLite / Postgres / Redis / 内存）           │
                               │   OTel 遥测 · 本地指标 · JSON 结构化日志              │
                               └──────────────────────────────────────────────────────┘
```

### 核心包说明

| 包 | 职责 |
|:---|:---|
| `internal/api` | 基于 Gin 的 REST 路由：任务 CRUD、SSE 流式、审批、记忆、指标、配置热重载 |
| `internal/orchestrator` | 任务运行主循环（Eino Chain / Legacy / ADK / Multi-Agent）；预算、规划、执行、答案流水线 |
| `internal/multiagent` | 协调器 + 双工作流引擎：Planner、Critic、Researcher/Executor、Writer/Verifier |
| `internal/planner` | LLM / Mock / Fallback 规划器；JIT 检索路由；工具参数修复器 |
| `internal/executor` | 单 Agent 动作分发器 |
| `internal/tools` | 工具注册表：`find_files`、`search_text`、`read_file`、`write_file`、`execute_code`、`git_diff`、`http_fetch`、`web_search`、`rag_search`、`analyze_image` 等 |
| `internal/policy` | Workspace 边界、URL 白名单、风险等级、审批门控 |
| `internal/answerpipeline` | 并行答案审计：引用核查、事实时效性、数值一致性、不确定性、安全防护 |
| `internal/plancritic` | 结构化计划评审：完整性、步骤顺序、风险、可行性 |
| `internal/llm` | LLM 提供商抽象（OpenAI · Gemini · Ollama · LiteLLM · ADK），熔断器、重试、成本追踪 |
| `internal/memory` | 基于向量检索的长期记忆存储与冲突解决 |
| `internal/store` | SQLite / Postgres / Redis / 内存后端；pgvector 支持 |
| `internal/telemetry` | OTel SDK 初始化，OTLP 或标准输出导出 |
| `internal/promptmanager` | Langfuse Prompt 获取，三层降级（Langfuse → system_prompt → 代码默认） |
| `internal/evidencefilter` | 基于 LLM 的证据相关性过滤 |
| `internal/evidenceconflict` | 证据冲突检测与解决 |
| `internal/sourcecredibility` | 来源可信度评分 |
| `internal/promptguard` | Prompt 注入检测 |
| `internal/vision` | 多模态图像分析 |
| `internal/skills` | 基于文件的技能发现（`skills/<name>/SKILL.md`） |

---

## 📂 项目结构

```text
├── cmd/server/main.go          # 启动入口 — 初始化数据库、OTel、HTTP 服务
├── internal/
│   ├── api/                    # REST 处理器、SSE、中间件
│   ├── orchestrator/           # 任务运行主循环（Eino / Legacy / ADK / Multiagent）
│   ├── multiagent/             # 双工作流协调器（Research + Reviewed）
│   ├── planner/                # LLM / Mock / Fallback 规划器
│   ├── executor/               # 单 Agent 动作分发器
│   ├── tools/                  # 工具注册表与具体实现
│   ├── policy/                 # 安全策略、审批门控
│   ├── answerpipeline/         # 答案质量并行审计
│   ├── plancritic/             # 计划评审（Critic）
│   ├── llm/                    # LLM 提供商抽象层
│   ├── llmprovider/            # 各提供商专用客户端
│   ├── memory/                 # 带向量搜索的长期记忆
│   ├── store/                  # 持久化（SQLite / Postgres / Redis）
│   ├── telemetry/              # OpenTelemetry 初始化
│   ├── promptmanager/          # Langfuse Prompt 管理
│   ├── evidencefilter/         # 证据相关性过滤
│   ├── evidenceconflict/       # 证据冲突解决
│   ├── sourcecredibility/      # 来源可信度评分
│   ├── promptguard/            # Prompt 注入检测
│   ├── vision/                 # 多模态图像分析
│   ├── skills/                 # 技能发现与加载
│   ├── config/                 # 可热重载配置
│   ├── logger/                 # 结构化 JSON 日志
│   ├── metrics/                # 本地指标采集器
│   ├── types/                  # 共享类型（Task、StepTrace、Evidence 等）
│   └── workspace/              # 工作区管理
├── config.yaml                 # 主配置文件（支持热重载）
├── teams.yaml                  # Multi-Agent 团队与工作流配置
├── teams_zh.yml                # teams.yaml 中文注释版
├── skills/                     # 内置技能（code-review 等）
├── Sample/agent-api.http       # JetBrains HTTP 请求示例
├── records/                    # 设计笔记与变更日志
└── go.mod
```

---

## ⚡ 快速开始

### 前置条件

- Go 1.25 及以上版本
- 可选：`ripgrep`（`rg`）和 `find` 命令，用于本地文件工具
- 可选：本地运行 OTel Collector / Jaeger，监听 `127.0.0.1:4318`

### 配置 API Key

```bash
# 至少配置以下其中一个：
export OPENAI_API_KEY="sk-..."
export GEMINI_API_KEY="..."
export GOOGLE_API_KEY="..."
```

### 启动服务

```bash
go run ./cmd/server
# 服务默认监听 http://127.0.0.1:8088（可通过 AI_AGENT_API_ADDR 修改）
```

### 编译运行

```bash
go build -o server ./cmd/server
./server
```

### 运行测试

```bash
go test ./...
go test -race ./internal/multiagent/... ./internal/orchestrator/...
```

---

## ⚙️ 配置说明

所有配置项位于 [`config.yaml`](config.yaml)，可通过 `AI_AGENT_*` 环境变量覆盖，无需重启。

| 配置节 | 关键项 |
|:---|:---|
| `api` | `addr`、认证、租户 Workspace 根目录、预算与流水线执行模式 |
| `orchestrator` | `mode`（`eino` / `legacy` / `adk` / `step` / `multiagent`）、`max_concurrent_tasks` |
| `llm` | `provider`、`model`、按 Scene 覆盖、熔断器、重试预算、成本上限 |
| `store` | `type`、`dsn`、`vector_search`、`pgvector_dimensions`、ParadeDB 排名/慢查询设置、Memory 候选/衰减设置 |
| `rag` | `search_url`、`search_method`（`MCP` / `POST`）、`context_mode`（`jit` / `prefetch`） |
| `mcp` | `servers[]`，包含 URL、凭据环境变量、工具前缀、风险级别和故障策略（修改后需重启） |
| `embedding` | `model`（用于记忆向量搜索） |
| `answer_pipeline` | `enabled`、`enforcement`、必选审计阶段 |
| `langfuse` | 凭证、运行时获取及可选的启动 Prompt 初始化 |
| `telemetry` | `enabled`、`endpoint`、`exporter`（`otlp` / `stdout`） |
| `log` | `level`、`console`、`file_enabled`、`directory`、`retention_days` |
| `search` | `url`、`api_key`（Firecrawl 或兼容服务） |
| `tool` | `timeout_seconds` |
| `skill` | `root`（技能发现根目录） |

生产多租户部署应设置 `api.auth.require_tenant_workspace_root: true`，并为每个
非管理员租户配置独立的 `api.tenants.<tenant>.workspace_root`。创建任务时若请求的
Workspace 超出该根目录，服务会返回 `403`。兼容默认值为 `false`；即使未开启严格
模式，只要租户配置了 `workspace_root`，该边界也始终生效。

### Multi-Agent 编排模式

```bash
export AI_AGENT_ORCHESTRATOR_MODE=multiagent
```

在 [`teams.yaml`](teams.yaml) 中配置团队与工作流：

```yaml
active_team: "software_reviewed"        # 也可选 "software" / "data"

teams:
  software_reviewed:
    workflow: "planner_critic_executor_verifier"  # Reviewed（高严谨度）
  software:
    workflow: "adaptive"                          # 按风险/复杂度自动路由
  data:
    workflow: "planner_researcher_writer"         # Research（高效率）
```

运行时覆盖：

```bash
AI_AGENT_MULTIAGENT_TEAM=software_reviewed
AI_AGENT_MULTIAGENT_WORKFLOW=adaptive      # planner_researcher_writer | planner_critic_executor_verifier | adaptive
AI_AGENT_MULTIAGENT_RUNTIME=dag            # legacy | dag
```

### Langfuse Prompt 启动同步

`teams.yaml` 中交给 Langfuse 管理的角色需要配置 `prompt_name`、label 或固定版本，并
保留 `system_prompt` 作为本地降级内容和首次创建种子：

```yaml
planner:
  prompt_name: "teams/software/planner"
  prompt_label: "production"
  system_prompt: |
    You are a software architect agent...
```

显式开启启动同步：

```bash
LANGFUSE_ENABLED=true
LANGFUSE_BASE_URL=https://cloud.langfuse.com
LANGFUSE_PUBLIC_KEY=pk-lf-...
LANGFUSE_SECRET_KEY=sk-lf-...
LANGFUSE_BOOTSTRAP_MISSING_PROMPTS=true
LANGFUSE_BOOTSTRAP_FAILURE_POLICY=fail
LANGFUSE_BOOTSTRAP_TIMEOUT_SECONDS=15
```

服务启动时会获取所有显式命名的团队 Prompt。只有目标 label 和 `latest` 都确认该
Prompt 名称不存在时，才使用 `system_prompt` 创建 text Prompt。鉴权失败、超时、
服务端错误、已有 Prompt 缺少目标 label，以及固定版本不存在都不会触发创建。

多实例部署中，普通副本应关闭 bootstrap，只允许一个指定实例或部署 Job 执行，避免
并发创建首个版本。关闭 bootstrap 不影响运行时获取和本地 fallback。

---

## 🔗 API 接口说明

### Session 会话接口

Session 用于在当前租户下组织多个任务，并让这些任务共享会话记忆。

| 方法 | 路径 | 说明 |
|:---|:---|:---|
| `POST` | `/api/sessions` | 创建会话 |
| `GET` | `/api/sessions` | 查询会话列表（`?status=active\|archived&limit=50&offset=0`） |
| `GET` | `/api/sessions/:id` | 获取会话详情 |
| `PATCH` | `/api/sessions/:id` | 更新会话标题或状态 |
| `POST` | `/api/sessions/:id/archive` | 归档会话 |
| `GET` | `/api/sessions/:id/tasks` | 查询会话任务（`?status=&limit=50&offset=0`） |
| `GET` | `/api/sessions/:id/memories` | 查询会话记忆（`?limit=50&offset=0`） |

**创建会话：**

```http
POST /api/sessions
Content-Type: application/json

{
  "id": "session-demo-001",
  "title": "Agent 文件调研"
}
```

`id` 可省略，省略时由服务自动生成。`title` 默认为 `New session`，最多
200 个字符。新建会话的状态为 `active`。

**更新会话：**

```http
PATCH /api/sessions/session-demo-001
Content-Type: application/json

{
  "title": "更新后的调研会话",
  "status": "active"
}
```

创建任务时设置 `session_id`，即可将任务关联到指定会话：

```json
{
  "id": "task-001",
  "session_id": "session-demo-001",
  "mode": "multiagent",
  "goal": "查找代码库中所有 TODO 注释",
  "workspace": "./workspace",
  "max_steps": 8,
  "tool_budget": 10
}
```

`mode` 为可选字段，任务级支持 `eino`、`legacy`、`adk`、`step` 和
`multiagent`；省略时使用服务的全局编排模式。研究及 RAG 任务推荐使用
`multiagent`，简单生成或工具任务推荐使用 `eino` 或 `legacy`。

已归档的会话仍可查询，但不能创建新任务。可通过
`PATCH /api/sessions/:id` 并提交 `{"status":"active"}` 重新激活。

### 任务接口

| 方法 | 路径 | 说明 |
|:---|:---|:---|
| `POST` | `/api/tasks` | 创建任务 |
| `POST` | `/api/tasks/:id/run` | 执行一次 Plan-Execute 闭环（`?stream=true` 开启 SSE） |
| `POST` | `/api/tasks/:id/run-all` | 运行至终态，异步（`?stream=true` 同步 SSE 流式输出） |
| `GET` | `/api/tasks/:id` | 获取任务详情 |
| `GET` | `/api/tasks` | 任务列表（`?status=&limit=50&offset=0`） |
| `GET` | `/api/tasks/:id/stream` | SSE 实时事件流 |
| `POST` | `/api/tasks/:id/approve` | 审批高风险动作 |
| `POST` | `/api/tasks/:id/reject` | 拒绝高风险动作 |
| `DELETE` | `/api/tasks/:id/cancel` | 取消运行中的任务 |
| `DELETE` | `/api/tasks/:id` | 删除任务（运行中须先取消） |
| `DELETE` | `/api/tasks?confirm=true` | 管理员：清空所有任务 |

SSE `token` 事件只包含增量 `final_answer` 文本。Planner 的结构化思考、动作名称和
工具参数会被主动过滤；需要审计时仍通过正常执行 Trace 查看允许公开的内容。
Multi-Agent 会先缓冲答案 chunk，待草稿被接受（Reviewed 工作流还需独立验证通过）后
再发布；被拒绝或低置信度的草稿不会进入 SSE。

**创建任务请求体示例：**

```json
{
  "id": "task-001",
  "session_id": "session-demo-001",
  "mode": "multiagent",
  "goal": "查找代码库中所有 TODO 注释",
  "workspace": "./workspace",
  "max_steps": 8,
  "tool_budget": 10
}
```

### 记忆接口

| 方法 | 路径 | 说明 |
|:---|:---|:---|
| `GET` | `/api/memories` | 查询记忆列表（`?limit=50&offset=0&tenant_id=`） |
| `DELETE` | `/api/memories/:id` | 删除指定记忆 |
| `DELETE` | `/api/memories?confirm=true` | 管理员：清空所有记忆 |

### 系统接口

| 方法 | 路径 | 说明 |
|:---|:---|:---|
| `GET` | `/api/metrics` | 本地性能指标 |
| `POST` | `/api/config/reload` | 热重载配置（返回脱敏 diff 和单调递增的 `config_revision`） |
| `GET` | `/ping` | 健康检查 → `{"message":"pong"}` |

---

## 🔒 安全说明

- **Workspace 边界**：所有文件操作严格限定在声明的 Workspace 路径内，防止目录穿越。
- **高风险审批**：`write_file`、`execute_code` 等高风险工具会挂起任务，等待 `POST /api/tasks/:id/approve` 显式放行。
- **Prompt 注入检测**：外部输入文本在进入 LLM 上下文前，由 `internal/promptguard` 进行扫描。
- **敏感信息脱敏**：`internal/sanitize` 在写入存储前对 Observation 中的敏感信息进行清除。
- **URL 访问白名单**：`http_fetch` 工具屏蔽私有地址和回环地址。
- **API Key 安全**：请勿将 API Key 提交至代码仓库，应通过环境变量或密钥管理服务注入。

---

## 🗺️ 路线图

- [ ] DAG 图引擎正式发布（当前通过 `AI_AGENT_MULTIAGENT_RUNTIME=dag` 灰度开启）
- [ ] 任务管理与 Trace 可视化 Web UI
- [ ] 更多内置技能（测试生成、代码审查、数据分析）
- [ ] 自定义工具注册插件系统
