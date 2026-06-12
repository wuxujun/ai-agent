# Skill 机制接入设计

> 目标:在现有 `ai-agent` 运行时中引入一套类似 Claude Agent Skills 的"能力包"机制——以 `SKILL.md`(YAML frontmatter + Markdown 正文)+ 资源文件夹的形式定义能力,由 planner **渐进式披露 / 按需加载**,作为现有 tool registry 之外的扩展层。
>
> 适用版本:`github.com/wuxujun/ai-agent`(go 1.25)。本文档基于 2026-06-04 的代码静态分析。

---

## 1. 设计原则与核心判断

**不要把每个 skill 注册成一个独立 tool。** 项目里有一条强不变量(三同步):`PlannerDecisionSchema()`(OpenAI)、`PlannerDecisionGenAISchema()`(Gemini)、`ValidateDecision()` 全部从 `tools.DefaultRegistry` 派生。其中两套 schema 都把**每个工具的全部 `Parameters()` 合并进一个扁平的 `parameters` 对象**,且 OpenAI strict 模式要求所有参数都进 `required`。这意味着:

- 工具越多,合并后的参数表越大;planner 每一轮都要为不相关参数填空字符串。
- 若把 N 个 skill 各做成一个工具,schema 的 action enum 和参数表会随 skill 数量线性膨胀,context 预算和 strict 校验都会受不了。

**正确做法:Skill = prompt 层的能力包 + 单一 `use_skill` 入口工具。**

- 平时只把每个 skill 的 `name + description`(一行摘要)注入 prompt —— 对应 Claude Skills 的"渐进式披露":元数据常驻,正文按需读。
- planner 决定用某个 skill 时,调用唯一的新工具 `use_skill{ name }`,它返回该 skill 的 `SKILL.md` 正文 + 资源清单,作为 observation 喂回 planner。
- skill 正文里引导的后续具体动作,仍走现有的 `read_file` / `execute_code` / `write_file` / `search_text` 等工具。

这样 registry 只增加**一个**工具、**一个** `name` 参数,完全规避 schema 膨胀,同时拿到 skill 的全部表达力。

---

## 2. 架构总览

```
                       ┌──────────────────────────────────────────┐
                       │  skills/ 目录 (workspace 内或可配置根)     │
                       │    pdf/SKILL.md  + 资源                    │
                       │    code-review/SKILL.md + scripts/        │
                       └──────────────────────────────────────────┘
                                        │ 启动时扫描 (只解析 frontmatter)
                                        ▼
        internal/skills.Registry ── List() 摘要 ──┐
                  │                                │
                  │ Body(name) 读正文              ▼
                  │                       internal/planner/prompt.go
                  │                       BuildUserPrompt 追加 "Available skills" 段
                  │                                │
                  ▼                                ▼
        internal/tools/use_skill.go  ◄──── planner 选择 use_skill{name}
        Execute → 返回 SKILL.md 正文 + 资源清单 作为 observation
                  │
                  ▼
        planner 下一轮据正文指引调用 read_file / execute_code / ...
```

关键点:skill 层**不进入** `executor` 的并行执行模型之外的任何新路径。`use_skill` 就是一个普通 `Tool`,沿用现有 `Registry.Register` → `schema/validate/prompt` 自动派生 → `executor.Execute` 并行调度 → `StepTrace` 反馈的全链路。

---

## 3. 组件设计

### 3.1 `internal/skills` 包(新增)

职责:扫描 skill 目录、解析 frontmatter、提供摘要列表与按需读取正文。

```go
type Skill struct {
    Name        string   // frontmatter.name,用作 use_skill 的入参与唯一键
    Description string   // frontmatter.description,一行摘要,注入 prompt
    AllowedTools []string // 可选:该 skill 允许引导的工具白名单(用于审批/约束)
    Dir         string   // skill 目录绝对路径
    bodyPath    string   // SKILL.md 路径(正文按需读取,不常驻内存)
}

type Registry struct { ... }           // 线程安全,镜像 tools.Registry 的风格
func NewRegistry(root string) *Registry // root = skill 根目录
func (r *Registry) Load() error         // 扫描 root/*/SKILL.md,解析 frontmatter
func (r *Registry) List() []Skill       // 按 name 排序(确定性,和 tools 一致)
func (r *Registry) Get(name string) (Skill, bool)
func (r *Registry) Body(name string) (string, []string, error) // 正文 + 资源文件相对路径清单
```

设计要点:

- **只解析 frontmatter 入内存**,正文与资源清单在 `Body()` 调用时才读盘——这是"渐进式披露"的关键,避免把所有 skill 正文塞进每轮 prompt。
- 排序用 `name`,和 `tools.Registry.List()` 保持确定性,保证 prompt 稳定、可缓存。
- frontmatter 解析:复用 `SKILL.md` 顶部 `---` 包裹的 YAML。可用 `gopkg.in/yaml.v3`(已在依赖图中作为间接依赖,可提为直接依赖),或为零新依赖手写一个极小的 frontmatter 切分器(见代码草稿)。
- `root` 默认取 `workspace/skills`,可通过 `config.Skill.Root` 覆盖。

### 3.2 `internal/tools/use_skill.go`(新增工具)

```go
type UseSkillTool struct{ Skills *skills.Registry }

func (t *UseSkillTool) Name() string        { return "use_skill" }
func (t *UseSkillTool) RiskLevel() types.RiskLevel { return types.RiskLevelLow } // 只读正文
func (t *UseSkillTool) Description() string { return "Load a skill's full instructions by name before performing a specialized task" }
func (t *UseSkillTool) Parameters() map[string]any {
    return map[string]any{
        "name": map[string]any{"type": "string", "description": "Skill name to load, must match one of the listed available skills"},
    }
}
func (t *UseSkillTool) Execute(ctx, workspace, params) (*ToolResult, error) {
    // 查 registry → 读 Body → 返回正文 + 资源清单作为 Observation
}
```

注册方式有别于其它工具:其它工具用包级 `func init() { Register(...) }`,但 `use_skill` 需要持有 `*skills.Registry`,**不能在 init 里 Register**(此时 registry 尚未构建)。改为提供一个显式注册函数 `RegisterUseSkill(reg *skills.Registry)`,在 `cmd/server/main.go` 启动时、构建好 skill registry 后调用。

> 注意:`tools.Register` 写的是 `DefaultRegistry`,而 `DefaultRegistry` 在所有 `init()` 跑完后就定型并被 schema 读取。`use_skill` 在 `main()` 早期注册(planner 首次构建 schema 之前)即可,不影响三同步。

### 3.3 `internal/planner/prompt.go`(改动)

在 `BuildUserPrompt` 的 "Available tools" 段之后,追加一段 "Available skills"。改动是**纯增量**,不动现有工具清单逻辑:

```go
// 现有:Available tools 列表
// 新增:
var skillsString string
if skillRegistry != nil {
    var lines []string
    for i, s := range skillRegistry.List() {
        lines = append(lines, fmt.Sprintf("%d. %s: %s", i+1, s.Name, s.Description))
    }
    if len(lines) > 0 {
        skillsString = "\n\nAvailable skills (call use_skill{name} to load full instructions before a specialized task):\n" + strings.Join(lines, "\n")
    }
}
```

并在 `BuildSystemPrompt` 增加一条规则:`- For specialized tasks, first call use_skill to load the matching skill's instructions, then follow them step by step.`

> multiagent 模式有自己的 PlannerAgent(`internal/multiagent/planner_agent.go`),若希望该模式也享受 skill,需要在其 prompt 构建处做同样的注入。建议把 skill 摘要的拼接逻辑抽成一个 `skills.PromptSection(reg)` 帮助函数,两处共用。

### 3.4 `internal/planner/validate.go`(建议重构,非阻塞)

当前 `ValidateDecision` 用一段**按 action 名写死的 switch** 做参数校验(`validateFindFiles` / `validateExecuteCode` …)。`use_skill` 命中 `default` 分支(不校验),所以**接入 skill 本身不要求改这里**——`use_skill` 可以安全落地。

但这段写死的 switch 正是 2026-06-03 踩过的同类坑(validate 与 registry 脱节)。**建议顺带把校验下沉到工具自身**,彻底消除这条隐患:

```go
// Tool 接口新增可选方法(用类型断言,保持向后兼容,无需改所有工具)
type Validator interface {
    Validate(params map[string]any) error
}

// ValidateDecision 中:
if v, ok := tool.(Validator); ok {
    if err := v.Validate(ac.Parameters); err != nil { ... }
}
```

迁移路径:把 `validateFindFiles` 等逻辑挪进各自工具的 `Validate` 方法,删掉 `validate.go` 的 switch。这样"新增工具/skill 无需手改三处"的不变量才真正闭合。**此项可作为第二阶段**,不阻塞 skill 上线。

---

## 4. 安全与边界(必须处理)

1. **审批链覆盖。** `policy` 包目前只有 workspace 守卫、路径穿越、URL/命令 allowlist。skill 正文若引导 `execute_code`(高危),其审批必须仍走现有 `orchestrator/approval.go` 的 `RiskLevelHigh` 暂停-审批流程——因为审批是在**执行 `execute_code` 工具**时触发的,而非在 `use_skill` 时,所以**天然被覆盖**:`use_skill` 本身只读正文(Low risk),真正的高危动作仍由对应工具拦截。设计上要守住"`use_skill` 不直接执行任何副作用"这条线。
2. **skill 来源可信。** skill 目录内容等同于注入 prompt 的指令。若 skill 目录可被不可信来源写入,等于打开了 prompt 注入面。建议:skill 根目录与 workspace 分离(或只读挂载),并在文档中明确"skill 是受信任的一等配置,不接受运行时下载"。
3. **AllowedTools 白名单(可选强化)。** 在 frontmatter 里声明 skill 允许引导的工具集,执行 `use_skill` 时把白名单写进返回的 observation,供 planner 自律;若要硬约束,可在 executor 侧按"当前激活 skill 的 allowed-tools"过滤——但这会引入跨步状态,属于进阶项,首版不建议。
4. **context 预算。** 摘要常驻、正文按需,已经把膨胀控制住;但 `use_skill` 返回的正文仍受 `middleware.go` 的 4000 字符截断影响。长 skill 应拆分,或在 middleware 对 `use_skill` 放宽阈值(需评估)。

---

## 5. 落地步骤(建议顺序)

1. 新增 `internal/skills` 包(types + Registry + frontmatter 解析)。
2. 新增 `internal/tools/use_skill.go` + `RegisterUseSkill`。
3. `cmd/server/main.go`:启动时 `skills.NewRegistry(root)` → `Load()` → `tools.RegisterUseSkill(reg)`,并把 reg 传给 planner 用于 prompt 注入。
4. `internal/planner/prompt.go`:追加 "Available skills" 段 + system 规则。
5. 放一个示例 skill(`skills/code-review/SKILL.md`)做端到端验证。
6.(第二阶段)`validate.go` 下沉校验到 `Validator` 接口。

## 6. 验证清单(本地,沙箱无法编译)

- `go build ./...` 通过(新文件为加法式,不应破坏现有编译)。
- `go test ./internal/planner/...` —— 确认三同步回归测试(`schema_test.go` / `validate_test.go`)仍绿:`use_skill` 进 registry 后,schema 多一个 action+`name` 参数,validate `default` 放行。
- `go test ./internal/tools/...` —— 为 `UseSkillTool` 补一个单测(命中/未命中 skill、资源清单)。
- 手动:跑一个会触发 skill 的 goal,确认 trace 里出现 `use_skill` → 正文 observation → 后续工具调用的链路。
- 安全:确认 skill 引导的 `execute_code` 仍触发审批暂停(`StatusAwaitingApproval`)。
