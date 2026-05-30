# write_file & execute_code 可执行 Agent 升级变更记录 (Executable Agent Upgrade Changelog)

为了将 AI Agent 从只读检索型升级为可读写、可执行的交互型 Agent，我们新增并优化了 `write_file` 与 `execute_code` 模块。这使得系统有能力直接在工作区目录编写代码和运行程序。

---

## 📂 新增与修改的文件 (Files Modified)

| 文件路径 | 变更类型 | 描述 |
| :--- | :--- | :--- |
| [policy.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/policy/policy.go) | 修改 | 拓展 `allowedCommands` 映射，增加 `python3`, `python`, `go`, `node`, `bash`, `sh` 脚本执行解释器的白名单权限；增加 `ValidateWritePath` 方法防止路径穿越越权。 |
| [write.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/write.go) | **新增** | 新增 `WriteFile` 核心工具方法，支持在写入文件前自动创建多级父目录，并调用 `policy` 实施安全写盘。 |
| [execute.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/execute.go) | **新增** | 新增 `ExecuteCode` 核心工具方法，支持从 LLM 输入的单字符串参数中提取 space-separated arguments slice 并拉起白名单子进程。 |
| [schema.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/schema.go) | 修改 | 优化 `PlannerDecisionSchema()`，在 action enum 中注册 `write_file` 与 `execute_code`，在 parameters properties 中拓展 `content`, `command`, `args` 强类型定义。 |
| [llm.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/llm.go) | 修改 | 在 Gemini 后端专用的 `PlannerDecisionGenAISchema()` 结构化声明中注册新动作与参数格式。 |
| [validate.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/validate.go) | 修改 | 新增对 `write_file` (检查 path 安全和 content 参数) 和 `execute_code` (检查 command 非空及 args 参数) 决策解析的合法性强校验。 |
| [prompt.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/prompt.go) | 修改 | 在 `BuildUserPrompt` 模版中增加 `write_file` 和 `execute_code` 描述，指导模型合适时选用。 |
| [executor.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/executor/executor.go) | 修改 | 在 `DefaultExecutor.Execute` 的 switch 分支中适配 `"write_file"` 和 `"execute_code"`，执行并采集子进程 stdout/stderr，裁剪过长的 observation 以免撑爆 Trace。 |
| [executor_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/executor/executor_test.go) | **新增** | 单元测试，在临时沙箱中验证写出 Python 脚本并执行输出的单 Agent 工具闭环。 |
| [types.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/types.go) | 修改 | 扩展 `ResearchStep` 结构体，增加 `Content`, `Command`, `Args` 参数。 |
| [planner_agent.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/planner_agent.go) | 修改 | 调整多角色 Multi-Agent 规划器的 JSON Schema 及系统提示词，使 Planner 能够智能编排文件写入与代码运行的复合流程。 |
| [researcher_agent.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/researcher_agent.go) | 修改 | 在 `ResearcherAgent.Research` 的 switch 中实现 write_file / execute_code 解析，保证多角色协作模式下也能成功写盘和执行脚本。 |
| [multiagent_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/multiagent_test.go) | 修改 | 增加多 Agent 的 Researcher 环节文件写入与命令行解释器拉起的单元测试。 |

---

## ⚙️ 运行时安全性机制说明

为确保系统的防御等级，本项目采用了两层安全检查：
1. **防越界写入**：`write_file` 会检测目标路径与当前 Workspace root 是否一致，禁止任何以 `..` 或是绝对路径写盘的操作。
2. **防任意代码执行漏洞 (RCE)**：`execute_code` 命令在真正运行前会对其使用的可执行程序进行过滤，当前严格限定只准许使用系统已安设的解释器环境 (`python3`, `python`, `go`, `node`, `bash`, `sh`)。

---

## 🧪 自动化测试验证记录

所有的写入与可执行重构驱动均已通过验证：
```bash
# 运行 Executor 的动作运行测试 (单 Agent 写入/执行验证)
go test -v ./internal/executor -run TestExecutorWriteFileAndExecuteCode
# 运行 Multi-Agent 框架下的 Researcher 工具链测试 (多角色写入/执行验证)
go test -v ./internal/multiagent -run TestResearcherAgent_WriteFileAndExecuteCode
# 验证整个项目编译与全部测试通过
go test ./...
```
单元及集成测试执行结果均显示 **PASS**。
