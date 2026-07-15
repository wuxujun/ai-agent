package planner

import (
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/skills"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

// SkillRegistry is the optional skill capability layer injected into the
// planner prompt. It is a package-level variable (set once at startup in main)
// rather than a constructor parameter so the existing planner constructors and
// BuildUserPrompt callers stay source-compatible. A nil registry simply means
// no skills are advertised — skills.PromptSection handles nil gracefully.
var SkillRegistry *skills.Registry

func BuildSystemPrompt() string {
	return `You are the planner for a multi-step search agent.

Your job is to choose exactly one next action at a time.
Do not execute tools.
Do not invent tools.
Do not answer with free-form prose.
Return only a decision object that matches the required schema.

Rules:
- Prefer the smallest useful next step.
- First narrow the search space, then search, then inspect context.
- If there is enough evidence to answer, stop.
- If no useful next step exists, stop.
- Never use an action not listed in the tool list.
- Use read_file only after you have a likely target file.
- For a specialized task, first call use_skill to load the matching skill's instructions, then follow them step by step.
- Keep thought_summary short and concrete.`
}

func BuildUserPrompt(task *types.Task) string {
	prompt, _ := buildUserPromptWithStats(task)
	return prompt
}

func buildUserPromptWithStats(task *types.Task) (string, promptBuildStats) {
	memorySection, stats := buildMemoryPromptSection(task)

	var toolsList []string
	for i, t := range tools.DefaultRegistry.List() {
		toolsList = append(toolsList, fmt.Sprintf("%d. %s: %s", i+1, t.Name(), t.Description()))
	}
	toolsString := strings.Join(toolsList, "\n")

	// Progressive disclosure: advertise only the skill name+description here;
	// the full SKILL.md body is loaded on demand via the use_skill tool.
	skillsString := skills.PromptSection(SkillRegistry)

	traceSection, traceOriginal, traceIncluded, traceTruncated := summarizeTraceWithStats(task.Trace)
	stats.TraceOriginalBytes = traceOriginal
	stats.TraceIncludedBytes = traceIncluded
	stats.TraceTruncated = traceTruncated

	prompt := fmt.Sprintf(`Task goal:
%s%s

Current status:
- step_count: %d
- max_steps: %d
- tool_budget: %d
- status: %s

Available tools:
%s%s

Unresolved questions:
%s

Recent trace:
%s

Decision requirements:
- Choose exactly one next action.
- If enough evidence exists, stop and provide final_answer.
- If stopping, set action to "none".`,
		task.Goal,
		memorySection,
		task.StepCount,
		task.MaxSteps,
		task.ToolBudget,
		task.Status,
		toolsString,
		skillsString,
		formatUnresolved(task.Unresolved),
		traceSection,
	)
	stats.UserPromptBytes = len(prompt)
	return prompt, stats
}

func indentText(text, indent string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}

func formatUnresolved(items []string) string {
	if len(items) == 0 {
		return "- none"
	}
	var b strings.Builder
	for _, item := range items {
		b.WriteString("- ")
		b.WriteString(item)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func summarizeTrace(traces []types.StepTrace) string {
	result, _, _, _ := summarizeTraceWithStats(traces)
	return result
}
