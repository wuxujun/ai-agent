# LLM 模块多后端支持与 Gemini、Ollama 接入变更记录 (LLM Multi-Backend & Gemini, Ollama Support Changelog)

为了提升 AI Agent 规划器（Planner）的扩展性、灵活性与自主性，我们对原有的 `LLMPlanner` 进行了重构，并增加了对 **Gemini**、**Ollama** 以及**标准 OpenAI Chat Completions** 的原生支持，彻底消除了仅支持单一 OpenAI Responses 闭源 API 以及硬编码依赖 `OPENAI_API_KEY` 的限制。

---

## 📂 新增与修改的文件 (Files Modified)

| 文件路径 | 变更类型 | 描述 |
| :--- | :--- | :--- |
| [llm.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/llm.go) | 修改 | 重构 `LLMPlanner`，定义统一的 `ProviderType` 接口模式；实现 Gemini (通过官方 `genai` 客户端与结构化 JSON Schema)、Ollama 以及标准 OpenAI API 的请求和响应解析逻辑。 |
| [main.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/cmd/server/main.go) | 修改 | 修改主服务入口，支持通过环境变量灵活配置 LLM Provider，移除未配置 `OPENAI_API_KEY` 时程序直接退出的硬编码。 |
| [llm_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/llm_test.go) | **新增** | 新增测试用例，通过本地 mock 服务对 `openai-responses`、`openai`、`ollama` 和 `gemini` 四大 LLM 后端的请求载荷和响应解析逻辑进行多维度校验。 |

---

## 🛠️ 重构设计详情 (Refactoring Details)

### 1. `LLMPlanner` 结构与 Provider 支持
在 `internal/planner/llm.go` 中，新增了 `ProviderType` 枚举并扩展了结构体：
```go
type ProviderType string

const (
	ProviderOpenAIResponses ProviderType = "openai-responses" // 原有的 OpenAI Responses API
	ProviderOpenAI          ProviderType = "openai"           // 标准 OpenAI Chat Completions API
	ProviderGemini          ProviderType = "gemini"           // 谷歌 Gemini SDK
	ProviderOllama          ProviderType = "ollama"           // Ollama 本地化服务 API
)

type LLMPlanner struct {
	Provider ProviderType
	APIKey   string
	Model    string
	BaseURL  string
	Client   *http.Client
}
```

### 2. 结构化 JSON 输出（Structured Output）实现方案
各家大模型在输出结构化 JSON schema 规划决策（PlanDecision）时的配置机制：
1. **Gemini**: 引入 `google.golang.org/genai` 官方 SDK。在请求参数中直接设置 `ResponseMIMEType: "application/json"`，并基于 `genai.Schema` 动态生成标准的 JSON Schema 类型（`PlannerDecisionGenAISchema()`）。
2. **OpenAI (标准)**: 向 `/v1/chat/completions` 发送请求，通过 `"response_format"` 参数强制指定 `json_schema` 类型以及 `PlannerDecisionSchema()`。
3. **Ollama**: 向 `/api/chat` 发送请求，将 `"format"` 直接配置为标准的 `PlannerDecisionSchema()`。
4. **OpenAI Responses (保留原样)**: 保留原先对 `https://api.openai.com/v1/responses` 的请求格式与返回数据处理，以确保 100% 向后兼容。

---

## ⚙️ 运行时切换配置指南 (Runtime Configuration Guide)

运行时可以通过配置 `AI_AGENT_LLM_PROVIDER` 及相关环境变量，切换并运行不同的规划器：

### 1. 谷歌 Gemini (默认模型为 `gemini-2.5-flash`)
```bash
export AI_AGENT_LLM_PROVIDER="gemini"
export GEMINI_API_KEY="your-gemini-api-key"
# 可选参数：
export AI_AGENT_LLM_MODEL="gemini-2.5-flash"
go run ./cmd/server
```

### 2. Ollama 本地大模型 (默认模型为 `llama3`)
```bash
export AI_AGENT_LLM_PROVIDER="ollama"
export AI_AGENT_LLM_MODEL="llama3"
export AI_AGENT_LLM_BASE_URL="http://localhost:11434/api/chat"
go run ./cmd/server
```

### 3. 标准 OpenAI 兼容端 (默认模型为 `gpt-4o`)
```bash
export AI_AGENT_LLM_PROVIDER="openai"
export OPENAI_API_KEY="your-openai-api-key"
export AI_AGENT_LLM_MODEL="gpt-4o"
export AI_AGENT_LLM_BASE_URL="https://api.openai.com/v1/chat/completions"
go run ./cmd/server
```

### 4. 原有 OpenAI Responses 兼容端 (向后兼容默认方案)
如果不显式指定 `AI_AGENT_LLM_PROVIDER`，系统默认回退至本方案：
```bash
export OPENAI_API_KEY="your-openai-api-key"
go run ./cmd/server
```

---

## 🧪 单元与集成测试校验 (Testing)

所有的 LLM 接入驱动都可以通过统一的单元测试进行回归验证（通过 mock 本地服务器模拟各模型的 API response）：
```bash
go test -v ./internal/planner/...
```

#### 单元测试执行结果：
```text
=== RUN   TestLLMPlannerProviders
=== RUN   TestLLMPlannerProviders/openai-responses
2026/05/29 14:57:33 [LLM Planner] Starting planning for task test-task-1, step_count: 0, provider: openai-responses, model: gpt-4.1
2026/05/29 14:57:33 [LLM Planner] Sending request to API (openai-responses): http://127.0.0.1:52030
2026/05/29 14:57:33 [LLM Planner] API response received with status 200
2026/05/29 14:57:33 [LLM Planner] Task test-task-1 decision - Thought: "Finding files" | Action: "find_files" | Stop: false | FinalAnswer: "" | Parameters: map[pattern:*]
=== RUN   TestLLMPlannerProviders/openai
2026/05/29 14:57:33 [LLM Planner] Starting planning for task test-task-1, step_count: 0, provider: openai, model: gpt-4o
2026/05/29 14:57:33 [LLM Planner] Sending request to API (openai): http://127.0.0.1:52032
2026/05/29 14:57:33 [LLM Planner] API response received with status 200
2026/05/29 14:57:33 [LLM Planner] Task test-task-1 decision - Thought: "Finding files" | Action: "find_files" | Stop: false | FinalAnswer: "" | Parameters: map[pattern:*]
=== RUN   TestLLMPlannerProviders/ollama
2026/05/29 14:57:33 [LLM Planner] Starting planning for task test-task-1, step_count: 0, provider: ollama, model: llama3
2026/05/29 14:57:33 [LLM Planner] Sending request to API (ollama): http://127.0.0.1:52034
2026/05/29 14:57:33 [LLM Planner] API response received with status 200
2026/05/29 14:57:33 [LLM Planner] Task test-task-1 decision - Thought: "Finding files" | Action: "find_files" | Stop: false | FinalAnswer: "" | Parameters: map[pattern:*]
=== RUN   TestLLMPlannerProviders/gemini
2026/05/29 14:57:33 [LLM Planner] Starting planning for task test-task-1, step_count: 0, provider: gemini, model: gemini-2.5-flash
2026/05/29 14:57:33 [LLM Planner] Sending request to Gemini: model=gemini-2.5-flash
2026/05/29 14:57:33 [LLM Planner] Task test-task-1 decision - Thought: "Finding files" | Action: "find_files" | Stop: false | FinalAnswer: "" | Parameters: map[pattern:*]
--- PASS: TestLLMPlannerProviders (0.03s)
    --- PASS: TestLLMPlannerProviders/openai-responses (0.02s)
    --- PASS: TestLLMPlannerProviders/openai (0.00s)
    --- PASS: TestLLMPlannerProviders/ollama (0.00s)
    --- PASS: TestLLMPlannerProviders/gemini (0.01s)
PASS
ok  	github.com/wuxujun/ai-agent/internal/planner	1.865s
```
