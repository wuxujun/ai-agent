# 工具注册表化与 Schema 同步修复变更记录 (Tool Registry Changelog)

为了让 Agent 的工具集从"硬编码闭集"演进为"可插拔开集"，本次迭代统一了 `Tool` 接口与注册中心，并修复了一个会让新增工具**完全失效**的关键 bug：工具已注册进 Registry，但 Planner 的 JSON Schema（`action` 枚举）未同步，导致 LLM 在 strict 模式下永远无法选中新工具。

本次同时打通了**单 Agent（OpenAI + Gemini 两条路径）**与 **multi-agent** 三处规划链路，使其 action 枚举全部从注册表派生——新增工具只需写一个工具文件 + `init()` 注册，三处自动生效。

---

## 📂 新增与修改的文件 (Files Modified)

| 文件路径 | 变更类型 | 描述 |
| :--- | :--- | :--- |
| [registry.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/registry.go) | 修改 | `Tool` 接口新增 `Parameters()`；`List()` 改为按名排序；新增 `Names()` 辅助方法。 |
| [find.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/find.go) / [rg.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/rg.go) / [read.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/read.go) / [write.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/write.go) / [execute.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/execute.go) / [git_diff.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/git_diff.go) / [http_fetch.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/http_fetch.go) / [web_search.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/web_search.go) | 修改 | 每个工具补 `Parameters()` 方法，导出各自的参数 JSON-Schema 片段。 |
| [schema.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/schema.go) | **修复** | `PlannerDecisionSchema()`（OpenAI 路径）的 action 枚举与 parameters 改为从注册表动态生成。 |
| [llm.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/llm.go) | **修复** | `PlannerDecisionGenAISchema()`（Gemini 路径）同样改为动态生成，补齐遗漏。 |
| [policy.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/policy/policy.go) | **新增** | 新增 `ValidateURL()` SSRF 防护，拦截 loopback/私网/link-local/云元数据等地址。 |
| [http_fetch.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/http_fetch.go) | 修改 | 执行前调用 `policy.ValidateURL` 做出站 URL 校验。 |
| [researcher_agent.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/researcher_agent.go) | 修改 | 新增 `default` 分支经注册表分发任意工具；新增 `stepToParams()` 字段映射。 |
| [planner_agent.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/planner_agent.go) | 修改 | step schema 的 action 枚举改为 `tools.Names()`，新增 `url` 字段；更新系统提示词。 |
| [types.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/types.go) | 修改 | `ResearchStep` 新增 `URL` 字段（供 http_fetch 使用）。 |
| [coordinator.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/coordinator.go) | 修改 | `isReadOnlyAction` 纳入新只读工具以支持并行；`buildStepQuery` 增加新 action 的可读格式。 |
| [registry_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/tools/registry_test.go) / [schema_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/schema_test.go) / [url_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/policy/url_test.go) / [researcher_registry_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/multiagent/researcher_registry_test.go) | **新增** | 注册表完整性、schema 回归、SSRF 防护、researcher 注册表分发测试。 |

---

## 🛠️ 设计详情 (Design Details)

### 1. 统一 Tool 接口与注册中心
`Tool` 接口新增 `Parameters()`，让每个工具自描述其入参 Schema：
```go
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // JSON-Schema 片段，例如 {"path": {"type": "string"}}
	Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error)
}
```
工具通过 `init()` 自注册进 `DefaultRegistry`；`List()` 按名排序返回，保证生成的 schema 与 prompt 稳定可复现。

### 2. Planner Schema 动态化（核心修复）
此前 `action` 枚举写死为旧六项，与 `strict: true` 的结构化输出叠加后，模型物理上无法输出新 action——新工具沦为死代码。现改为：
```go
for _, t := range tools.DefaultRegistry.List() {
	actions = append(actions, t.Name())
	for name, spec := range t.Parameters() { paramProps[name] = spec }
}
actions = append(actions, "none")
```
> **strict 模式约束**：OpenAI / Gemini 结构化输出要求 properties 全部出现在 `required` 中，因此合并后的参数集全列入 required，未使用的参数以空字符串提交。

### 3. multi-agent 混合接入
保留原 5 个 action 的富证据映射不变（避免回归逐文件 Evidence），新增 `default` 分支将任意其他已注册工具经 `tools.Get` 路由执行，并用 `stepToParams()` 把语义字段映射为通用参数（超集映射，各工具只取所需键）。

### 4. SSRF 防护
`http_fetch` 的 URL 来自 LLM 输出，`ValidateURL()` 仅放行 http/https，并拦截 loopback、私网（10/172.16/192.168）、link-local、云元数据 `169.254.169.254`、CGNAT `100.64/10` 等地址。

---

## 🔌 新增工具与调用示例 (Usage)

新增三个工具：`git_diff`、`http_fetch`、`web_search`。

### 各工具 parameters 形状（Planner 决策 / Executor 入参）
```jsonc
{ "action": "git_diff",   "parameters": { "path": "internal/tools/registry.go" } }
{ "action": "http_fetch", "parameters": { "url": "https://example.com/api" } }
{ "action": "web_search", "parameters": { "query": "golang context timeout best practice" } }
```

### 注册一个自定义工具（扩展演示）
```go
package tools

import "context"

type EchoTool struct{}

func (t *EchoTool) Name() string             { return "echo" }
func (t *EchoTool) Description() string       { return "Echo back the given text" }
func (t *EchoTool) Parameters() map[string]any {
	return map[string]any{"text": map[string]any{"type": "string"}}
}
func (t *EchoTool) Execute(ctx context.Context, ws string, p map[string]interface{}) (*ToolResult, error) {
	msg, _ := p["text"].(string)
	return &ToolResult{Query: msg, Observation: "echo: " + msg}, nil
}

func init() { Register(&EchoTool{}) } // 放在 internal/tools 下，包初始化自动注册，三处规划链路自动可见
```

---

## 🧪 测试 (Testing)

```bash
go build ./... && go test ./internal/...
```
重点回归用例：
- `internal/planner/schema_test.go`：断言 action 枚举包含 git_diff/http_fetch/web_search、parameters 含 `url`，且满足 strict 模式不变式。
- `internal/policy/url_test.go`：SSRF 放行/拦截用例。
- `internal/multiagent/researcher_registry_test.go`：验证 researcher 经注册表分发并正确映射参数与证据。

---

## ⚠️ 已知限制 (Known Limitations)
- `web_search` 抓固定域名（DuckDuckGo HTML），SSRF 风险低，未加 URL 校验。
- multi-agent 的原 5 个 action 仍走专用分支（保留富证据），未来若需完全统一可进一步收敛。
