# Docs 归档目录 (Archive Index)

本目录归档已完成研发、测试验收与阶段性交付的历史报告、计划方案及测试记录。

## 目录结构与归档内容

### 1. `reviews/`（历史审查与批次修复记录）
- **`项目审查报告_2026-09-08.md`**：2026-09-08 开展的全栈架构与安全性审查基线报告，包含 B01–B13 修复记录、SQL 只读安全强化（2026-09-13）、O02 第一批审批恢复持久化契约（2026-09-14）以及 O03 第一批外部 RAG 接收限制（2026-09-14）的执行与验证证据。
- **`会话续接摘要_2026-09-11.md`**：记录 2026-09-11 至 2026-09-15 期间，围绕高优先级 Bug 修复（B01–B13）、PostgreSQL 真实契约验证、CI 自动化集成及跨会话工作移交上下文。
- **`BUG_REPORT_2026-08-18.md`**：早期 18 项安全与稳定性缺陷的审计追踪报告，其中 17 项已彻底修复或有效缓解，4 项残余项已结转至最新分析报告。

### 2. `superpowers/`（Brain 专属能力设计与执行计划）
- **`plans/2026-08-29-brain-eval-p0.md`**：Brain Eval P0 确定性与 Live 双臂对比评测工具的实施计划，已在 `internal/braineval` 与 `cmd/brain-eval` 完成实现，验证记录见 `records/brain-eval-p0.md`。
- **`plans/2026-09-02-brain-readonly-mvp.md`**：Brain 只读 Wiki MVP P1 的实现计划，已在 `internal/brain` 与 `cmd/brain-compile` 中完成快照、校验、CAS 发布与回滚、撤回账本实现，验证记录见 `records/brain-readonly-mvp-p1.md`。
- **`specs/2026-08-29-brain-eval-design.md`** 与 **`*-zh.md`**：Brain 评测框架的中英文系统设计规范。
- **`specs/2026-09-02-brain-readonly-mvp-design.md`** 与 **`*-zh.md`**：Brain 只读 MVP 的中英文系统设计规范。

### 3. `samples/`（接口调用与测试样例记录）
- **`session_api_req.md`**：2026-08-18 的 Session API 真实请求与响应 Smoke 测试快照，供接口报文参考。

---

## 当前保留在 `docs/` 的活跃文档
- **`task-api.md`**：当前在用的 Task API 核心接口定义、curl 调用示例与参数说明。
- **`项目完成度与待办Bug分析报告_2026-09-16.md`**：当前系统最新、最具权威性的完成度全景盘点、未闭环缺陷（P0 Windows 编译阻塞、P1 路径 TOCTOU、P1 信号量竞争、P1 限流重载等）及待办实施路线。
