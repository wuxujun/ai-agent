# 当前项目代码审查报告 (2026-06-15)

- **审查日期**：2026-06-15
- **审查基线**：Go 1.25 / commit `5fc29b2` 及当前工作区修复
- **项目模块**：`github.com/wuxujun/ai-agent`
- **报告输出路径**：`records/CODE_REVIEW_20260615.md`

---

## 一、 执行摘要 (Executive Summary)

本报告对 `ai-agent` 服务的当前代码库进行了深度的技术审查。该项目为基于 Go 1.25 构建的模块化、高可扩展的 AI Agent 编排系统，支持单智能体线性/Fallback 规划及基于 Swarm 架构的多智能体协同模式（Planner, Researcher, Writer 三人组）。

在最新的迭代中，系统引入了**分布式审批总线 (ApprovalBus)** 与**优雅停机断点恢复 (Graceful Shutdown Rollback)** 等 P0/P1 级核心功能。

### 综合评估
*   **代码质量**：**优秀**。遵循 Go 语言的惯用写法，包职责划分清晰，几乎没有无保护的全局状态，日志记录和 telemetry 接入规范。
*   **安全防范**：**良**。路径穿越由 `evalExistingPath` 递归解析防护；SSRF 客户端现已固定拨号至校验通过的 IP，并校验重定向目标。
*   **高并发与高可用**：**优秀**。优雅停机阶段能将运行中和队列中的任务自动安全回滚至 `StatusPaused`，且 `/run-all` 具备完整幂等性保障。分布式审批信号基于 Redis Pub/Sub 广播，具备良好的跨实例路由能力。
*   **存储与性能瓶颈**：**良**。SQL Trace 已支持事务保存完整字段；Redis 列表和记忆候选查询使用 ZSET 索引与批量 `MGET`，不再执行常规路径全量 SCAN。

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

## 三、 核心审查发现 (Core Findings)

### P0：服务监听所有网卡且 API 无认证 【已修复】
*   **位置**：`cmd/server/main.go:269-273`、`internal/api/handler.go:65-91`
*   **问题**：服务默认绑定 `Addr: ":8080"`，会监听系统所有接口（包含外网网卡），而任务创建、审批、取消和配置重载接口均无认证机制。
*   **风险**：若端口意外暴露至公网，远程未授权用户可任意操纵 Agent 任务、执行系统命令或消耗 LLM 配额。
*   **建议**：默认绑定 `127.0.0.1:8080`，通过显式配置开放外网，并为管理及任务接口增加令牌或 HMAC 签名认证。

### P1：HTTP 工具可通过重定向或 DNS 重绑定绕过 SSRF 防护 【已修复】
*   **位置**：`internal/policy/policy.go:130-158`、`internal/tools/http_fetch.go:59-72`、`internal/tools/web_browser.go:58-73`
*   **问题**：系统的 SSRF 校验是在发起 HTTP 请求前，先对域名进行 `LookupIP` 检查。然而，底层的 HTTP 客户端在真正建立 TCP 连接时会再次发起 DNS 请求。
*   **风险**：攻击者可以通过控制恶意 DNS 服务器，在校验域名时返回合法公网 IP，在实际 HTTP 请求时返回本地环回（`127.0.0.1`）或云厂商元数据地址，实现绕过 SSRF 探测内部服务。
*   **建议**：重写 HTTP 客户端的 `DialContext`，在建立 TCP 连接的拨号阶段对目标解析 IP 重新进行二次过滤，并禁止跟随未校验的重定向。
*   **修复验证**：安全客户端现在直接拨号到本次校验通过的 IP，不再二次解析域名；每次重定向重新执行 URL/IP 校验，并有私网拨号和私网重定向回归测试。

### P1：Redis 存储后端 $O(N)$ 性能瓶颈 (技术债) 【已修复】
*   **位置**：`internal/store/redis.go:142-194`
*   **问题**：`ListTasks` 和 `QueryMemories` 使用了 Redis SCAN 遍历所有 key，并使用 for 循环对每个 key 发起独立的 `Get` 操作。
*   **风险**：在高并发或百万任务级场景下，该操作将导致严重的 **N+1 次网络往返**，并极易阻塞 Redis 单线程事件循环，甚至导致内存溢出 (OOM)。
*   **建议**：在 Redis 引入有序集合 `ZSET` （以创建时间戳为 score，任务 ID 为 member），实现数据库侧的无阻塞快速分页。随后使用 `MGET` 或 Pipeline 一并拉取目标页数据。
*   **修复验证**：任务写入和状态跃迁原子维护全局/状态 ZSET，列表按 `offset/limit` 直接读取并 `MGET`；移除 1000 条硬上限，并提供旧索引幂等回填。

### P1：单步执行 `/run` 缺少任务互斥和原子状态迁移 【已修复】
*   **位置**：`internal/api/handler.go:234-269`
*   **问题**：`POST /api/tasks/:id/run` 单步运行仅拥有全局并发信号量限制，未像 `run-all` 那样引入 `activeTasks` 的进程内任务互斥锁和 CAS 状态原子跃迁。
*   **风险**：同一任务的多个 `/run` 请求可并发发起，从而导致同一工具被重复执行，造成 Trace 数据错乱。
*   **建议**：单步与全量运行共用每任务租约；在存储层增加版本号/CAS，冲突返回 `409 Conflict`。
*   **修复验证**：`/run` 与 `/run-all` 现在同时获取进程内 reservation 和带 owner/TTL 的 Store 租约；Memory、SQLite、Postgres、Redis 均实现原子租约，跨 Handler 冲突测试返回 `409`。

### P1：SQL 后端未持久化 Error 和 TokenUsage 【已修复】
*   **位置**：`internal/types/task.go:25-40`、`internal/store/sqlite.go:195-200,305-327`、`internal/store/postgres.go:189-195,293-315`
*   **问题**：`StepTrace.Error` 和 `StepTrace.TokenUsage` 均没有在 SQLite/Postgres 的 SQL schema、写入或读取语句中体现。
*   **风险**：服务重启或灾备切换后，累积的 Token 消费数据丢失，导致 `TokenBudget` 控制失效；在 UI 层面工具执行失败会被错误标记为成功。
*   **建议**：更新数据库 schema 并修改对应 `SQLStore` 实现以持久化该数据。
*   **修复验证**：SQLite/Postgres 已增加 `error_text` 和三项 Token 字段的幂等迁移，事务及独立 Trace 写入/读取路径均已覆盖，并加入跨存储往返断言。

### P2：SaveFullTask 的数据库事务原子性未完全实现 【已修复】
*   **位置**：`internal/store/sqlite.go:208-225`
*   **问题**：接口声明任务和 Trace 应进行原子保存，但在 SQL 实现中是先提交了任务更新，随后再在独立的事务中保存 Trace。
*   **风险**：若系统在两个操作中间崩溃，可能导致新状态搭配旧 Trace 的不一致态。
*   **建议**：重构代码以在同一个数据库事务（DB Transaction）中同时完成 task upsert 和 trace 更新。

---

## 四、 改进方案建议修复顺序

1.  **修复网络接入安全**：默认绑定 `127.0.0.1` 地址，并增加管理 API 认证。
2.  **已完成 HTTP 安全 Dial**：统一 HTTP Client 并固定拨号至已校验 IP。
3.  **已完成 SQL 持久化字段**：添加 `Error` 及 `TokenUsage` 的表迁移字段并补齐测试。
4.  **已完成互斥租约锁定**：`/run` 与 `/run-all` 共享跨实例任务租约。
5.  **已完成 Redis 分页重构**：使用 ZSET + MGET 和旧索引回填。
