# AI Agent 项目可扩展功能分析

下面从 **架构层面**、**核心组件**、**外部集成** 三个维度梳理系统的可扩展点，并给出实现思路与对应代码位置。

---

## 1️⃣ 架构层面的扩展入口

| 扩展方向 | 关键实现文件 / 接口 | 实现要点 |
|----------|-------------------|----------|
| **编排模式**（Orchestrator） | `internal/orchestrator/*.go`，`Engine.Next` 通过环境变量 `AI_AGENT_ORCHESTRATOR_MODE` 选路由 | 在 `engine.go` 中新增实现结构体，实现统一的 `runXNext(ctx, task)` 接口即可；只需在 `init()` 或 `main.go` 注册对应模式名称。 |
| **LLM Provider** | `internal/planner/llm.go`、`planner/provider.go` | 通过 `AI_AGENT_LLM_PROVIDER` 决定走向；要接入新模型（如 Claude、Azure OpenAI）只需实现一个符合 `LLMClient` 接口的适配层并在 `provider.go` 中映射。 |
| **存储后端** | `internal/store/store.go`（接口），具体实现：`sqlite.go / postgres.go / redis.go / memory.go` | 新增 DB（MySQL、DynamoDB）只需实现 `Store` 接口的 `SaveFullTask`, `GetTask`, `ReplaceTraces` 等方法，并在启动时通过环境变量或配置文件注入。 |
| **安全策略** | `internal/policy/policy.go` | 在 `ValidateWorkspace`, `ValidateCommand`, `ValidateReadPath` 中加入自定义校验（例如沙箱网络访问、用户角色限制），或者实现 `PolicyChecker` 接口并在 `Executor.Execute` 前调用。 |
| **工具链**（Tool） | `internal/tools/*`（find, rg, read, runner）以及 `tools/registry.go` | 新增工具只需要：<br>1. 实现统一的 `Tool` 接口 (`Execute(ctx, params) (Result, error)`) <br>2. 在 `registry.go` 注册名称 → 实例；<br>3. 更新 Planner Schema 中添加对应 `action`（如 `write_file`, `exec_command`）。 |
| **Skill / 插件体系** | `skills/` 目录、`internal/tools/use_skill.go` | 可在此层面加入新的业务技能（code‑review、security‑review 等），实现 `Skill` 接口并通过 `use_skill` 调用；可配合 CLI `/skill <name>` 快速触发。 |
| **监控/指标** | `internal/metrics/metrics.go`、`internal/telemetry/otel.go` | 新增自定义 Metric（如 “tool_timeout_total”）只需在对应业务点创建 Counter / Histogram 并记录；OTel Span 需要添加额外属性即可。 |
| **配置中心** | `.claude/settings.json`（通过 `update-config` skill） | 想让系统行为可运行时调节（如默认 `max_steps`, 超时时间），把对应字段抽象为 Settings 项并在代码中使用 `settings.Get(key)` 读取即可。 |

---

## 2️⃣ 功能层面的潜在扩展

| 方向 | 示例实现思路 |
|------|--------------|
| **更多工具**：写文件、删除文件、压缩归档、调用外部 API（REST/GraphQL） | 在 `internal/tools/write.go` 中实现 `WriteFileTool`，注册为 `write_file`；更新 Planner Schema 的 `action` 列表。 |
| **复杂工作流**：多模型协同（Planner 生成子任务 → 子任务分配给不同 LLM） | 在 Orchestrator 的 `runXNext` 中加入“子任务调度器”，把 `PlanDecision.SubTasks []TaskSpec` 发往不同的 Planner 实例。 |
| **长时运行 & 异步**：后台作业、任务超时重试、分布式执行 | 将 `Engine.RunAll` 改为使用消息队列（Kafka/NSQ）或 `goroutine pool`，状态持久化在 Store 中；利用 OTel 记录跨进程 Span。 |
| **多租户 & 隔离**：每个用户独立 Workspace、配额限制 | 在 `policy.ValidateWorkspace` 加入租户 ID 前缀检查，在 Store 增加 `tenant_id` 字段，Engine 根据请求上下文挑选对应 workspace。 |
| **自定义 Prompt/Schema**：业务方可自行编辑 Planner Prompt 或添加新的 Action 参数结构 | 把 Prompt 模板抽取到配置文件（JSON/YAML），在 `planner/prompt.go` 动态读取；提供 `/update-config set planner.prompt_file=…` 命令。 |
| **结果后处理**：自动生成报告、发送邮件/Slack 通知、触发 CI/CD | 在 Executor 完成后调用新插件 `Notifier`（实现 `Notify(ctx, task)`），可在 `internal/tools/notifier.go` 中封装 HTTP/Webhook 调用。 |
| **前端 UI 扩展**：实时任务监控仪表盘、交互式 Prompt 编辑器 | 利用现有 `frontend-design` skill 创建 React/Vue 页面，调用 `/run <skill>` 或直接访问 `GET /api/tasks/:id/trace` 提供数据源。 |
| **安全审计**：审计日志、工具使用记录、合规报告生成 | 在每个 Tool 执行前后写入统一的 AuditLog（SQLite 表或外部日志系统），并提供 `/security-review` skill 报告。 |

---

## 3️⃣ 推荐的第一步扩展路线

| 步骤 | 操作 |
|------|------|
| **1. 增加工具**：实现 `write_file` 并在 Planner Schema 中加入 `action: "write_file"`（最小冲击、直接验证）。 |
| **2. 扩充编排模式**：复制现有 `step` 模式的结构，新建 `batch` 模式支持一次性并行执行多个 Tool（展示多任务调度能力）。 |
| **3. 引入新 LLM Provider**：实现 Anthropic Claude SDK 适配器，修改 `provider.go` 增加 `claude` 条目；配置文件中加入 `CLAUDE_API_KEY`。 |
| **4. 完善安全策略**：在 `policy.ValidateCommand` 中加入白名单机制，支持基于正则的可执行命令过滤，防止潜在注入风险。 |
| **5. 可观测性升级**：为新增工具/模式添加自定义 OTel Span 名称和属性（如 `tool.name`, `mode.type`），并在 `metrics.go` 中暴露对应计数器。 |

---

## 小结
- 项目已经通过 **接口抽象（Store、Policy、Tool）** 与 **环境变量驱动的插件化** 实现了良好的可扩展性。
- 关键扩展点集中在 **工具链、编排模式、LLM Provider、存储后端和安全策略**，只需实现相应接口并在注册表中登记即可完成新增功能。
- 对外暴露的 **Skill / Config / Metrics** 机制让业务方可以在运行时通过 CLI 或 UI 快速开启/关闭新特性，而无需改动代码。

如果您对某个具体方向（如 “实现 write_file 工具”）想要进一步的实现细节或示例，请告诉我，我可以直接生成相应的代码片段并提交。
