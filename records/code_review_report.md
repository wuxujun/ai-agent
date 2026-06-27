# AI-Agent 代码审查报告 (Code Review Report)

- **审查日期**：2026-06-15
- **当前版本基线**：Go 1.25 / 最新 commit `eda3111`
- **项目模块**：`github.com/wuxujun/ai-agent`
- **报告输出路径**：`records/code_review_report.md`

---

## 一、 执行摘要 (Executive Summary)

本报告对 `ai-agent` 服务的当前代码库进行了深度的技术审查。该项目为基于 Go 1.25 构建的模块化、高可扩展的 AI Agent 编排系统，支持单智能体线性/Fallback 规划及基于 Swarm 架构的多智能体协同模式（Planner, Researcher, Writer 三人组）。

在最新的迭代中，系统引入了**分布式审批总线 (ApprovalBus)** 与**优雅停机断点恢复 (Graceful Shutdown Rollback)** 等 P0/P1 级核心功能。

### 综合评估
*   **代码质量**：**优秀**。遵循 Go 语言的惯用写法，包职责划分清晰，几乎没有无保护的全局状态，日志记录和 telemetry 接入规范。
*   **安全防范**：**良**。针对路径穿越防御构建了业界高标准的 `evalExistingPath` 软链接递归解析机制。但在网络层，针对 SSRF 的 URL 校验仍然存在 DNS Rebinding / TOCTOU（时间差攻击）的隐患。
*   **高并发与高可用**：**优秀**。优雅停机阶段能将运行中和队列中的任务自动安全回滚至 `StatusPaused`，且 `/run-all` 具备完整幂等性保障。分布式审批信号基于 Redis Pub/Sub 广播，具备良好的跨实例路由能力。
*   **存储与性能瓶颈**：**中等，存在明显的架构技术债**。SQLite 在 Traces 保存上做了增量追加写优化，性能良好。但 Redis 存储后端在 `ListTasks` 与 `QueryMemories` 中依赖了 `SCAN` + 循环单点 `GET` 模式，当数据规模增大时会引发 $O(N)$ 的网络往返延时，并阻塞 Redis 事件循环。

---

## 二、 架构设计与三方不变量

项目架构遵循严格的单向依赖原则：

```
cmd/server (主入口)
   └── internal/api (HTTP 路由与控制层)
          └── internal/orchestrator (编排引擎)
                 ├── internal/multiagent (多智能体协同)
                 ├── internal/planner (LLM 规划层)
                 ├── internal/executor (执行器)
                 ├── internal/store (多存储后端)
                 └── internal/policy (安全沙箱)
```

### 1. 三方锁步不变量的设计
系统建立了 `tools.DefaultRegistry`（工具注册表）、`planner.PlannerDecisionSchema`（LLM 决策 JSON Schema）与 `planner.ValidateDecision`（决策合法性校验）的三方锁步不变量。新增任何工具均从 Registry 自动推导 Schema，规避了由于手动维护校验逻辑导致的 Schema 漂移漏洞，设计非常优雅。

### 2. 多智能体协作流 (Swarm)
在 `ModeMultiAgent` 下，编排控制权交给 [coordinator.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/coordinator.go)。系统依据 `teams.yaml` 动态构建子 Agent 团队，并由 `PlannerAgent` 分解目标为 `ResearchSteps`。通过 `partitionBatch` 区分只读并发与写串行，且在只读并发阶段前引入了 **Look-ahead Token 预算前瞻防御机制**，动态调整并发批次大小，防止大批次并发击穿 LLM 配额。

---

## 三、 安全沙箱策略审计

### 1. 路径穿越防护 (Path Traversal)
位于 [policy.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/policy/policy.go#L44-L123) 的 `ValidateWorkspace` 和 `ValidateReadPath` 表现极佳：
*   **渐进式软链接评估**：利用 `evalExistingPath` 自底向上逐级评估路径中已存在的真实目录并解析其软链接。这解决了“文件或路径目前不存在时导致 `filepath.EvalSymlinks` 失效，从而在文件创建时绕过沙箱检查”的经典漏洞。
*   **显式软链接拒绝**：如果最终评估出的真实路径与输入路径不符，直接拒绝该 workspace，将安全隐患消灭在初始化阶段。

### 2. SSRF 校验与 DNS Rebinding 隐患
`ValidateURL` 过滤了私有 IP 段、回环地址、云元数据地址（`169.254.169.254`）等敏感网段。

> [!WARNING]
> **DNS 重绑定 (DNS Rebinding) / TOCTOU 缺陷**
> 系统的 SSRF 校验是在发起 HTTP 请求前，先对域名进行 `LookupIP` 检查。然而，底层的 HTTP 客户端（如 `http_fetch` / `web_browser`）在真正建立 TCP 连接时会再次发起 DNS 请求。
> 攻击者可以通过控制 DNS 服务器，使得第一次解析返回合法的公共 IP，而第二次解析（实际连接时）返回 `127.0.0.1`，从而绕过 SSRF 防护，探测或操纵内网服务。
>
> **修复方案**：应当重写 HTTP 客户端的 `DialContext`，在 TCP 三次握手建立连接的那一刻，对目标 IP 重新进行二次过滤。

---

## 四、 高并发与高可用评估

### 1. 分布式审批总线 (ApprovalBus)
*   **机制**：[approval_bus.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/orchestrator/approval_bus.go) 封装了基于 Redis Pub/Sub 的消息订阅与发布。
*   **安全性**：当跨实例部署时，某个实例收到了 `/api/tasks/:id/approve` 请求，若发现当前任务不运行在本地，它会通过 Redis 广播信号。正在运行该任务的实例通过 background goroutine 监听到信号后，安全地往本地的 `approvalEntry.ch` 写入结果。
*   **锁释放设计**：在 [approval.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/orchestrator/approval.go) 中，`ResolveApproval` 和 `ResolveApprovalByID` 将 channel 写入操作置于锁外部执行（利用 `buffer=1` 的 channel 不阻塞特性），有效防止了持有 Mutex 进行阻塞式 I/O 引发死锁的问题。

### 2. 优雅停机与租约机制
*   在 [handler.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/api/handler.go#L116-L183) 的 `Shutdown` 过程中，系统优雅地调用所有 activeTasks 的 `cancel()` 并等待 `sync.WaitGroup` 任务落库。
*   利用回滚机制，强制将被 context 终端的运行中任务的状态从 `StatusFailed` 重写为 `StatusPaused`。在重新启动时，`/run-all` 接口可以通过判定 `StatusPaused` 承接状态，实现优雅断点续传。

---

## 五、 存储层与性能债务

### 1. SQLite: 增量 Trace 写入
`SQLiteStore` 中的 `ReplaceTraces` 包含以下优化：
*   通过查询 `COALESCE(MAX(step), 0)` 确认已落库的最大步号，仅执行 `step > maxPersistedStep` 的增量 `INSERT OR IGNORE` 写入。
*   该优化将原先每次调用全量重写带来的 $O(N^2)$ 写放大问题降至 $O(N)$，极大减轻了磁盘 I/O 开销。

### 2. Redis: $O(N)$ 性能瓶颈 (技术债)
在 [store/redis.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/redis.go#L142-L194) 的 `ListTasks` 接口和 `QueryMemories` 中存在反模式设计：
*   **问题**：直接调用 `Scan(..., "task:*", 100)` 遍历所有 task key，然后利用 `for _, key := range keys` 对每一个 key 独立发起 `client.Get`。这引发了严重的 **N+1 查询瓶颈**，会带来海量的网络 RTT 损耗，且容易阻塞 Redis 的单线程处理能力。
*   **问题**：无条件的进程内排序与分页。如果数据库积压了数万条任务，一次 `/api/tasks` 会导致全量数据反序列化，极易引发内存溢出 (OOM)。

> [!CAUTION]
> **Redis 存储重构建议**
> 1. 应使用 Redis 排序集合（ZSET）维护任务索引。例如，任务创建时执行：`ZADD tasks:index <timestamp> <taskID>`。
> 2. `ListTasks` 时，直接通过 `ZREVRANGE tasks:index <offset> <offset + limit - 1>` 检索当前页 of TaskID。
> 3. 利用 Redis `MGET` 或 Pipeline 批量获取任务详情，网络交互复杂度降为常数级 $O(1)$。

---

## 六、 改进方案路线图 (Actionable Roadmap)

### 1. P0 级核心修复 (安全与并发)
*   **DNS Rebinding 免疫** 【已修复】：重构 [policy.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/policy/policy.go)，提供统一的安全 HTTP Client。在其自定义 Dialer 的 `DialContext` 函数中进行 IP 范围过滤。
*   **API 认证门禁** 【已修复】：在 `/api` 路由组中加入基础令牌认证（Bear Token）或 HMAC 签名拦截器，默认禁止网卡全监听。

### 2. P1 级系统优化 (存储性能)
*   **Redis ZSET 重构** 【已修复】：消除 `redis.go` 中的 `SCAN` 全表扫描和无序分页逻辑，引入 ZSET 索引完成数据库侧分页。
*   **SQLite 并发写优化**：在 sqlite dsn 初始化时附加 `_pragma=journal_mode=WAL&_pragma=busy_timeout=5000`，缓解多实例并发写时 SQLite 偶发的锁死报错。

### 3. P2 级日常维护
*   **统一 LLM 客户端连接池**：参照 `gemini_pool.go` 的 LRU 连接复用模式，将连接池规范应用到 OpenAI 和 Ollama 客户端。
*   **外部化向量检索**：将 in-process 内存余弦相似度计算解耦为独立的 `VectorStore` 接口，以便后续在 Postgres 下无缝迁移至 `pgvector`。
