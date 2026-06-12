# 稳健性与契约一致性审查变更记录 (Robustness & Contract Review Changelog)

本轮针对三方同步不变量、Multi-Agent 协作逻辑、API/SSE 接口契约做了一次静态审查，并修复了其中的正确性、可靠性与安全加固问题。

> ⚠️ 注：改动均经静态审查（导入、签名、类型、控制流自洽），但当前环境无 Go 工具链、未编译验证。合并前请在本地执行 `go build ./... && go test ./...`。

---

## 📂 新增与修改的文件 (Files Modified)

| 文件路径 | 变更类型 | 描述 |
| :--- | :--- | :--- |
| [coordinator.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/coordinator.go) | 修改 | ① Writer 失败时任务状态由 `Completed` 改为 `Failed`，错误写入结构化 `StepTrace.Error`，保留 best-effort 答案。④ `isReadOnlyAction` 增加注册表 RiskLevel 权威 gate：高危工具一律不进并行批，强制走串行审批路径。⑥ 抽出 `tokenBudgetExhausted` 共享辅助，自适应深度循环也纳入 token 预算 gate。 |
| [sse.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/api/sse.go) | 修改 | ② `streamTask` 新增 15s 存储轮询兜底：即使 `Publish` 对慢消费者丢弃了实时事件，终态事件最终也一定送达；非终态轮询兼作 keep-alive，替换原与 ticker 冲突的 25s 计时器。 |
| [llm.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/llm.go) | 修改 | ⑤ 新增 `genaiSchemaFromSpec`，从工具 `Parameters()` 的 JSON-Schema 推导 Gemini 参数类型，不再一律 `TypeString`，使 OpenAI / Gemini 两条 planner 路径对非字符串参数自动同步。 |
| [README.md](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/README.md) | 修改 | ③ 修正 `run-all` 为 202 异步语义；补全缺失的 6 个端点（list/stream/approve/reject/cancel/config-reload）+ `/ping`；明确"权威状态以 GET 为准"及 SSE 尽力推送契约。 |
| [writer_failure_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/writer_failure_test.go) | **新增** | 验证 Writer 失败 → 任务标记 `Failed`、保留 fallback 答案、writer trace 记录错误。 |
| [coordinator_internal_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/coordinator_internal_test.go) | **新增** | 白盒测试：高危工具不被判只读、被强制切为串行批，确保不绕过审批。 |

---

## 🔍 审查结论摘要

- **三方同步不变量**（schema/llm/validate 均由 `tools.DefaultRegistry` 派生）：当前一致，机制健壮。修复了 Gemini 侧参数类型退化（⑤）这一中期隐患。
- **Multi-Agent 协作**：架构清晰、容错丰富。修复了 Writer 失败被吞为成功（①）和并行批审批盲区（④），并堵住 token 预算只在研究阶段生效的缺口（⑥）。
- **API / SSE 契约**：实现比文档完善。补齐了 SSE 终态可靠投递（②）与 README 接口同步（③）。
- **误报更正**：`listTasks` 的 limit 在 store 层 `resolveLimit(_, 50, 500)` 已有 500 硬上限截断，handler 不截断不构成漏洞，无需修改。

---

## 🧪 建议的验证命令

```bash
go build ./...
go test ./internal/planner/ ./internal/multiagent/ ./internal/api/
# 或全量
go test ./...
```
