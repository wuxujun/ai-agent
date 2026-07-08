# Multi-Agent 协调器分析与测试补全

> 会话日期:2026-07-05
> 范围:项目整体分析 → multiagent 协调器流程梳理 → 测试覆盖评估 → 补测并发路径与响应解析器

---

## 一、项目整体分析

**一句话概括**:用 Go 1.25 构建的 AI Agent 执行运行时引擎,核心是 Planner(规划)→ Executor(执行)循环,让 LLM 在受沙箱保护的工作空间里自主规划并调用工具达成用户目标。约 2 万行 Go,111 个 `.go` 文件。

### 技术栈

| 层面 | 选型 |
|---|---|
| 语言/框架 | Go 1.25 + Gin(HTTP) |
| 编排引擎 | CloudWeGo Eino Chain + Google ADK for Go |
| LLM | OpenAI(Responses/Chat)、Gemini(genai)、Ollama,可插拔 |
| 存储 | SQLite(默认)/ Postgres / Redis / Memory,四选一 |
| 可观测性 | OpenTelemetry(OTLP → 127.0.0.1:4318) |
| 配置 | Viper + fsnotify 热重载 |

### 五种编排模式(`orchestrator.Engine.Mode`)

| 模式 | 文件 | 说明 |
|---|---|---|
| `eino` | `orchestrator/eino.go` | 默认。编译并缓存 Eino Chain,每步跑一次 Plan→Execute |
| `legacy` | `orchestrator/engine.go` | 直接 Planner→Executor,无 Eino 包装,便于调试 |
| `step` | `orchestrator/step.go` | 离散步骤状态机变体 |
| `adk` | `orchestrator/adk.go` | Google ADK-for-Go runner(`sync.Once` 编译一次) |
| `multiagent` | `orchestrator/multiagent.go` + `internal/multiagent/coordinator.go` | Plan → Research(×N)→ Write 流水线,**一次 Next 完成整个任务** |

> 注:当前 `config.yaml`(未提交改动)将 `mode` 设为 `adk`。

### 关键设计约束
1. **三方不变式**:`tools.DefaultRegistry` ↔ 两份 Planner Schema ↔ `ValidateDecision` 必须同步;Schema 由工具注册表自动生成。
2. **技能即一等能力**:`skills/<name>/SKILL.md` 启动加载,经 `use_skill` 工具暴露;`RegisterUseSkill` 必须在 planner 首次编译 schema 之前运行。
3. **高风险动作需审批**:工具声明 `RiskLevel()`,`RiskLevelHigh` 触发 `SuspendForApproval`,支持 Redis Pub/Sub 分布式审批总线。
4. **工具失败非致命**:错误记入 trace 作为 observation,不中断循环。
5. **配置永不缓存指针**:始终 `config.Get()`。

---

## 二、Multi-Agent 协调器流程梳理

### 架构:三角色 + 一协调器

```
                    Coordinator (协调器)
                          │
   ┌──────────────────────┼──────────────────────┐
   ▼                      ▼                      ▼
PlannerAgent        ResearcherAgent           WriterAgent
(LLM 规划)          (纯工具执行,无 LLM)        (LLM 综合)
分解目标为步骤       执行每个步骤调工具          汇总证据写最终答案
```

三角色均为 interface(`Planner`/`Researcher`/`Writer`),可替换。协调器**就地修改** `task.Trace`、`task.Status`、`task.StepCount`、`task.ToolBudget`。与其他模式不同,multiagent **一次 `Engine.Next` 就跑完整个任务**。

### 入口衔接(`orchestrator/multiagent.go`)
`runMultiAgentNext`:校验 `Coordinator` 已注入 → 若任务已终态则跳过 → 调 `Coordinator.Run` → 写 OTel span 属性。

### 三阶段主循环(`Coordinator.Run`)

**Phase 1 — Plan(`runPlanPhase`)**
- `PlannerAgent.Plan()` 用 LLM 把目标分解为 2~8 个有序 `ResearchStep`(JSON Schema 强制结构化输出)。
- action 枚举从 `tools.Names()` 动态生成;记录 planner trace(含 token),`Hypothesis = thought_summary`。

**Phase 2 — Research(`runResearchPhase`)—— 核心**
按队列消费 `currentSteps`,每轮:
1. **预算闸门**:`ToolBudget<=0` / token 耗尽 / `StepCount>=MaxSteps` → 停止。
2. **分批 `partitionBatch`**:队首连续只读步骤成并行批;写/执行类单独串行。
   - 只读判定:`find_files/search_text/read_file/git_diff/http_fetch/web_search` 且 `RiskLevel != High`。
3. **Token 前瞻防御**:并行前按"剩余预算 / 每步平均 token"钳制批大小。
4. **执行**:
   - 并行 `runBatchParallel`:`sync.WaitGroup` 并发,结果按序合并。**不做审批**——高风险工具被强制排除在只读之外。
   - 串行 `runBatchSerial`:逐个执行;`RiskLevelHigh` 先 `SuspendForApproval` 阻塞等审批。
5. **失败即重规划**:任一步失败 → `Planner.Replan()` 生成 1~5 个修订步骤。最多 3 次(`maxReplans`)。

**Phase 3 — Write(`runWritePhase`)**
- `WriterAgent.Write()` 综合证据成 `final_answer` + `evidence_summary` + `confidence`(high/medium/low)。
- 证据每行超 2000 字符截断。
- 成功 → `StatusCompleted`;失败 → `StatusFailed`(不是 Completed!),保留证据作兜底。

### 自适应深度扩展(Adaptive Step Depth)
Write 后若 `confidence == "low"` 且 `depthIterations < 2` 且预算/步数/token 有余 → 调 `Replan` 深挖一轮再重写。最多 2 次(`maxDepthIterations`)。

### 两类"重规划"

| 触发点 | 位置 | 上限 | 目的 |
|---|---|---|---|
| 错误纠正重规划 | Phase 2 内,某步失败 | 3 次 | 修复失败步骤 |
| 自适应深度重规划 | Phase 3 后,置信度低 | 2 次 | 证据不足时深挖 |

### 动态团队(teams.yaml)
`GetTeamsConfig()` 支持切换 `active_team`(`software` / `data`),为每个角色覆盖 system_prompt / provider / model / name。当前激活 `software`。

### 可靠性要点
- 工具错误非致命:`ResearcherAgent` 用 `Failed` 布尔字段而非解析 "error" 字符串。
- 并行审批绕过防护:高风险工具经 `RiskLevel` 强制走串行审批。
- 每阶段/批次有 `<-ctx.Done()` 检查点。
- 全程 token 计量并作为预算闸门。

---

## 三、测试覆盖评估(补测前)

**`go test -race -cover ./internal/multiagent/...` → PASS,覆盖率 39.9%**

### 覆盖分布两极分化
- ✅ 编排核心逻辑(mock agent 驱动)覆盖良好:`Run` 79.6%、`runWritePhase` 87.5%、`Research` 92.9%、`partitionBatch` 80% 等。
- ❌ 完全没覆盖(0%):
  - 真实 LLM 调用层:`callLLMJSON`/`callGeminiJSON`/`callHTTPJSON`/`extractResponsesText`/`extractChatText`/`extractOllamaText`/`parseJSONInto`
  - Agent LLM 门面:`PlannerAgent.Plan`/`Replan`、`WriterAgent.Write`
  - **`runBatchParallel` = 0%**(并发路径完全未测,race 风险最高处)
  - `paramsToStep` = 0%

### 识别的真实盲区(非 LLM 层,可低成本补)
1. `runBatchParallel` —— 并发执行路径完全未测。
2. `extractText*` 系列 —— 纯 JSON 解析,不需真实 LLM 即可测。

---

## 四、测试补全成果

### 结果:覆盖率 39.9% → **55.3%**(+15.4pp),`-race` 全绿无竞争

| 函数 | 补测前 | 补测后 |
|---|---|---|
| `runBatchParallel`(并发路径) | 0% | **94.4%** |
| `parseJSONInto` | 0% | **100%** |
| `extractResponsesText` | 0% | **89.3%** |
| `extractChatText` | 0% | **90.0%** |
| `extractOllamaText` | 0% | **100%** |

### 新增文件 1:`internal/multiagent/parallel_batch_test.go`
覆盖之前完全空白的并发批次执行:
- 用 `atomic.Int32` 计数的并发安全 mock researcher,在 `-race` 下验证无竞争。
- 验证证据按批次顺序合并、`StepCount`/`ToolBudget` 按批大小推进、每步一条 trace。
- 覆盖三条路径:失败步骤跳过证据但置 `anyFailed`、fatal error(nil 证据)不 panic、全成功。

### 新增文件 2:`internal/multiagent/llm_parse_test.go`
覆盖三个 provider 响应解析器(纯 JSON,不打网络):
- OpenAI Responses / Chat / Ollama 正常解析 + token 用量提取。
- 各自错误分支(缺 output、空 choices、非法 JSON)。
- `parseJSONInto` 直接解析、markdown 围栏兜底提取、无 JSON 报错。

### 剩余未覆盖
主要是真实 LLM 调用层(`callHTTPJSON`/`callGeminiJSON`/`Plan`/`Write`/`Replan`),需 `httptest.Server` 或真实 API,属集成测试范畴,未纳入当前纯单测。后续可用 `httptest.Server` 给 `callHTTPJSON` 补一层不依赖真实 provider 的端到端解析测试。

---

## 附:相关文件索引
- 协调器主流程:`internal/multiagent/coordinator.go`
- 三个 Agent:`planner_agent.go` / `researcher_agent.go` / `writer_agent.go`
- 类型定义:`internal/multiagent/types.go`
- LLM 调用与解析:`internal/multiagent/llm.go`
- 团队配置:`teams.yaml` + `internal/multiagent/teams.go`
- 编排入口:`internal/orchestrator/multiagent.go`
- 新增测试:`internal/multiagent/parallel_batch_test.go`、`internal/multiagent/llm_parse_test.go`
