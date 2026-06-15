# AI Agent Runtime — 可扩展功能与待优化方向深度分析报告

> **生成日期**：2026/06/13  
> **分析对象**：`github.com/wuxujun/ai-agent`  
> **分析维度**：功能扩展性（Extensibility）、系统性能与稳定性优化（Optimization）

---

## 1. 可扩展的功能（Extensible Features）

基于项目目前清晰的抽象边界（如 `Store`、`LLMProvider`、`Tool` 接口），系统为后续横向和纵向的功能扩展预留了良好的扩展点。

### 1.1 编排引擎扩展：DAG/图化工作流引擎 (LangGraph 模式)
- **现状**：现有的默认 `Eino` 链和 `Legacy` 模式本质上是线性的 **Planner $\rightarrow$ Executor** 循环，且每个 Next 执行单个 Step 的 Plan-Execute。
- **扩展方向**：
  - **基于图的编排（Graph-based Orchestration）**：可以增加一个新编排 Mode（如 `ModeGraph`），允许 Planner 生成含有依赖关系的 Action 树/有向无环图（DAG），并以拓扑序并发执行无依赖的工具链，而非现在的线性串行或 Multi-Agent Research 阶段的纯并发。
  - **条件分支控制**：支持分支图结构路由，根据 Executor 执行的阶段状态或数据动态分流，无需每步都请求 LLM Planner 进行重新规划，节约 Token 消耗。

### 1.2 工具生态动态化与新型工具包扩展
借助已有的 `tools.DefaultRegistry` 工具注册表，扩展新能力只需实现 `Tool` 接口并注入：
- **浏览器交互工具 (Web Browser Tool)**：
  - **实现**：引入整合了 Playwright 或 Headless Chrome 的 `web_browser` 工具，允许 Agent 渲染 SPA 单页应用、定位 DOM 元素、模拟点击并截取网页图片提供给多模态 Planner。
- **结构化查询隔离工具 (SQL Query Tool)**：
  - **实现**：增加 `sql_query` 工具。为了防止 SQL 注入或删库风险，可以配置只读数据库连接，且前置通过 SQL Parser（例如 `github.com/pingcap/tidb/pkg/parser`）强校验只允许 `SELECT` 语句，阻止 DDL/DML。
- **语义代码修补工具 (Code Patch Tool)**：
  - **实现**：增加 `apply_patch` 或 LLM 增量 diff 替换工具，替代目前 `write_file` 暴力覆盖整个文件的机制，提高代码修改的安全性和局部完整性。

### 1.3 多智能体（Multi-Agent）动态网络扩展 (Completed)
- **现状**：现有的 `internal/multiagent` 已重构为支持基于 `teams.yaml` 配置文件动态组建子 Agent 团队（如软件团队、数据团队）代替原本的硬编码三人组。
- **功能特性**：
  - **动态团队组建**：通过读取工作区根目录的 `teams.yaml` 动态覆盖 Planner, Researcher, Writer 的系统提示词（`SystemPrompt`）和名称。
  - **大模型配置热插拔**：支持为团队中各个子 Agent 动态指定不同的 LLM Provider (如 Gemini, OpenAI, Ollama) 和特定 Model 模型，实现了完全异构的 Agent 协作网络。
  - **环境变覆盖**：支持通过环境变量 `AI_AGENT_MULTIAGENT_TEAM` 运行时指定当前激活的智能体团队。
- **进一步扩展方向**：
  - **群组对话与层级治理**：支持组播路由（Group Chat）与主从层级决策，Planner Agent 作为主控，下发指令给子 Agent Swarm 执行具体任务。

### 1.4 人类介入（Human-in-the-loop）深度交互
- **现状**：目前审批挂起（`SuspendForApproval`）只有同意/拒绝（Approve/Reject）的二元判定。
- **扩展方向**：
  - **交互式变量注入**：允许用户在审批时回传修正参数（例如：LLM 试图删除某个文件，用户拒绝但输入“允许重命名为 xxx”）。
  - **反馈文本反馈环**：用户输入的拒绝理由（Reject Reason）作为 `Evidence` 直接灌回 Trace，使 Planner 在下一轮能够明确了解“为什么动作被拒绝”，进行自纠规划。

---

## 2. 待优化的功能与痛点（Optimizations & Bottlenecks）

随着并发量和长期记忆（Memories）规模的增长，以下领域存在性能瓶颈和稳定性隐患，需要进行优化：

### 2.1 长期记忆检索性能升级 (Vector DB ANN)
- **痛点**：目前 `store.QueryMemories` 在 SQLite 和 Postgres 中仍需在 Go 进程内反序列化 200 条候选记忆并利用 `CosineSimilarity` 算向量相似度。随着任务量的累积（上万条历史记录），这种内存算余弦的做法将成为 CPU 和内存瓶颈。
- **优化方案**：
  - **Postgres 驱动**：使用 `pgvector` 扩展，执行原生 SQL：`ORDER BY embedding <=> $1`，并在 embedding 字段上建立 HNSW 索引实现近似最近邻（ANN）检索。
  - **SQLite 驱动**：使用 `sqlite-vss` 或 `sqlite-vec` 插件，在本地进行向量化检索加速。

### 2.2 多实例部署下的分布式审批与取消
- **痛点**：
  - 任务审批挂起通道 `approvals = make(map[string]*approvalEntry)` 存在于进程的全局内存中。
  - 取消任务 `activeTasks` 也保存在进程内存。
  - **隐患**：如果在生产环境下进行多实例水平扩展部署，客户端的审批请求（`/api/tasks/:id/approve`）或取消请求（`/api/tasks/:id/cancel`）如果路由到了与执行任务不同的实例上，将无法获取对应的 Channel 或 Context 实例，造成系统逻辑挂起。
- **优化方案**：
  - 引入分布式消息总线（例如 Redis Pub/Sub 或 RabbitMQ），在接收到 `/approve` 或 `/cancel` 请求时向集群广播消息。
  - 在底层 Store 中增加 `awaiting_approval` 的状态同步，在审批通过时由获取广播的执行实例激活对应的任务。

### 2.3 LLM 客户端池统一治理与参数轮换
- **痛点**：当前项目中仅 Gemini Client 拥有 LRU 连接池（`gemini_pool.go`），而 OpenAI 和 Ollama 的客户端在 Provider 切换时缺少一致的生命周期生命和缓存，长跑服务可能出现内存抖动。
- **优化方案**：
  - 构造统一的 `ClientPool[K, C]` 泛型结构。
  - 支持对所有 LLM Provider（OpenAI/Gemini/Ollama）的客户端实例进行统一的 LRU 缓存、闲置释放和心跳保活检测。

### 2.4 并行批次执行中的 Token 预算“前瞻防御”
- **痛点**：在 Multi-Agent 模式的 Research 阶段，系统会并行运行一整批 read-only action。Token 预算检查是在**批次开始前**进行的。如果这个批次很大，由于并发执行，无法在其中一个步骤超额时及时截断，可能导致整批并发运行结束后远远超出设定的 `TokenBudget`。
- **优化方案**：
  - 引入“前瞻估算”（Look-ahead Estimation）机制：根据 Planner 输入的历史上下文和 Action 动作的复杂度，估算本批次的最大并发 Token 开销。
  - 如果估算值可能超预算，则自动将并行度调低或将批次拆分为较小的串行子批。

### 2.5 优雅停机与断点恢复状态回滚 (Graceful Shutdown Rollback)
- **痛点**：当 Gin 服务器接收到 SIGTERM/Interrupt 信号进行 `Shutdown` 时，系统会取消所有 `activeTasks` 的 Context。这些被强行中断的任务会在 DB 中被标记为 `failed` 状态。
- **隐患**：重启服务器后，这些任务将无法继续，这不符合“断点恢复”的运行时语义。
- **优化方案**：
  - 在优雅停机时，检查当前任务在 Cancel 前的执行进度。
  - 对于未发生崩溃、仅因服务停机导致中断的任务，将其状态回退为 `StatusCreated` 或新增的 `StatusPaused`，并在重启后支持重新加载断点轨迹（Traces）继续向下运行。

---

## 3. 优化与扩展优先级推荐矩阵

为了最大化工程杠杆率，建议采用如下分阶段建设矩阵：

| 阶段 | 核心任务 | 分类 | 杠杆价值与解决痛点 |
| :--- | :--- | :--- | :--- |
| **第一阶段 (P0)** | **分布式审批与取消机制** | 待优化 | 解决服务水平扩展（多副本）部署的核心架构缺陷，实现高可用。 |
| **第二阶段 (P1)** | **优雅停机与状态回滚** | 待优化 | 保证生产环境运维（如滚动更新）期间执行中的任务不坏死、支持自动恢复。 |
| **第三阶段 (P2)** | **引入 `pgvector` / `sqlite-vss`** | 待优化 | 彻底消除大规模历史记忆（Memories）带来的 RAG 检索 CPU/内存瓶颈。 |
| **第四阶段 (P3)** | **工具包扩展与 DAG 图化编排** | 可扩展 | 提升 Agent 应对复杂并行逻辑的解决效率，拓宽应用面。 |
