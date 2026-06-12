# Skill 接入 —— 接线补丁(prompt.go / main.go / config.go)

> 这些是需要改动**现有文件**的部分。为避免让仓库处于半编译状态,这里以补丁片段形式给出,你 review 后手动套用。新增文件(`internal/skills/*.go`、`internal/tools/use_skill.go`)已经是加法式的,可直接编译。

---

## 1. `internal/config/config.go` —— 新增 skill 根目录配置

在 `Config` 结构体里加一段:

```go
Skill struct {
    Root string `mapstructure:"root"`
} `mapstructure:"skill"`
```

在 `LoadConfig()` 的默认值区加:

```go
viper.SetDefault("skill.root", "skills") // 相对 workspace 或运行目录
```

---

## 2. `internal/planner/prompt.go` —— 注入 skill 摘要

`BuildUserPrompt` 目前签名是 `BuildUserPrompt(task *types.Task) string`,内部直接读 `tools.DefaultRegistry`。最小改动:让它也能拿到 skill registry。两种做法,二选一:

**方案 A(推荐,显式传参)** 改签名为 `BuildUserPrompt(task *types.Task, skillReg *skills.Registry) string`,调用方(planner 的 LLM/Mock 实现)把 registry 传进来。

**方案 B(零签名改动,包级变量)** 在 planner 包加一个 `var SkillRegistry *skills.Registry`,main 启动时赋值,prompt.go 读它。简单但引入全局可变状态。

无论哪种,拼接逻辑复用 `skills.PromptSection`:

```go
// 在 toolsString 之后:
skillsString := skills.PromptSection(skillReg) // 方案A;方案B用 SkillRegistry

// 在 fmt.Sprintf 的 "Available tools:\n%s" 段后追加 "%s"(skillsString)
```

并在 `BuildSystemPrompt()` 的 Rules 列表里加一条:

```
- For specialized tasks, first call use_skill to load the matching skill's instructions, then follow them step by step.
```

> multiagent 模式:`internal/multiagent/planner_agent.go` 若自建 prompt,同样调用 `skills.PromptSection(reg)` 注入,保证两条路径一致。

---

## 3. `cmd/server/main.go` —— 启动时构建并注册

在 LLM/orchestrator 初始化**之前**(planner 首次编译 schema 之前)插入:

```go
import (
    "github.com/wuxujun/ai-agent/internal/skills"
    "github.com/wuxujun/ai-agent/internal/tools"
)

cfg := config.Get()
skillRoot := cfg.Skill.Root
if !filepath.IsAbs(skillRoot) {
    // 解析为相对 workspace 或运行目录,按你的约定
    skillRoot = filepath.Join(workspaceRoot, skillRoot)
}
skillReg := skills.NewRegistry(skillRoot)
if err := skillReg.Load(); err != nil {
    log.Printf("[Skills] load failed (continuing without skills): %v", err)
}
tools.RegisterUseSkill(skillReg) // 进 DefaultRegistry,三同步自动覆盖

// 若用 prompt.go 方案 B:planner.SkillRegistry = skillReg
// 若用方案 A:把 skillReg 透传给 planner 构造函数
```

顺序要点:`RegisterUseSkill` 必须在任何 `PlannerDecisionSchema()` / `PlannerDecisionGenAISchema()` 首次被调用之前完成,否则 `use_skill` 不会出现在 action enum 里。eino 路径是 lazy compile(首个请求才编译 schema),所以 main 早期注册一定来得及。

---

## 4.(第二阶段)`internal/planner/validate.go` —— 校验下沉

把写死的 switch 换成接口断言,消除"validate 与 registry 脱节"这类历史坑:

```go
// 新增接口(放 tools 包或 planner 包均可,建议 tools 包随工具走)
type Validator interface {
    Validate(params map[string]any) error
}

// ValidateDecision 循环内,替换 switch:
if v, ok := tool.(Validator); ok {
    if err := v.Validate(ac.Parameters); err != nil {
        return fmt.Errorf("validation failed for action %s: %w", ac.Action, err)
    }
}
// "none" 的 final_answer 校验保留在外层单独处理。
```

迁移时把 `validateFindFiles` / `validateSearchText` / `validateReadFile` / `validateWriteFile` / `validateExecuteCode` 分别挪进各工具的 `Validate` 方法。`UseSkillTool.Validate` 已在草稿里给好。
