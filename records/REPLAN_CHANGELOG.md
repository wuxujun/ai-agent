# Multi-Agent 协作纠错闭环变更记录 (Collaborative Re-planning Changelog)

为了提升 Multi-Agent 模式在面对异常、代码报错和安全拦截时的鲁棒性，我们引入了动态重规划与协作纠错闭环机制。

---

## 📂 新增与修改的文件 (Files Modified)

| 文件路径 | 变更类型 | 描述 |
| :--- | :--- | :--- |
| [coordinator.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/coordinator.go) | 修改 | 重构 `runResearchPhase` 执行控制流。定义 `Planner`, `Researcher`, `Writer` 接口以支持 Mock。拦截 Researcher 返回的 `error` 以及 Observation 中的关键报错短语，动态触发重规划，并重置步骤切片。 |
| [planner_agent.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/planner_agent.go) | 修改 | 新增 `Replan` 核心接口方法，引入 `replannerSystemPrompt` 系统提示词指导大模型分析失败历史、调整参数并重新排列执行步骤。 |
| [replan_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/replan_test.go) | **新增** | 新建多角色纠错协作测试，验证在工具致命错误与观察文本警告两种异常场景下，动态重构规划的正确性。 |

---

## ⚙️ 纠错运行参数

- **最大重试规划次数**：白名单参数 `maxReplans` 硬编码为 `3`。以防大模型在极端情况下反复陷入失败死循环，超过此限制将安全退出并输出当前收集到的事实痕迹。

---

## 🧪 测试执行与通过情况

```bash
# 运行协作重规划测试
go test -v ./internal/multiagent -run "TestCoordinatorReplan"
# 验证整个项目测试通过
go test ./...
```
测试执行结果均显示 **PASS**。
