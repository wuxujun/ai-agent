package planner

import (
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

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
- Keep thought_summary short and concrete.`
}

func BuildUserPrompt(task *types.Task) string {
	var memorySection string
	if len(task.Memories) > 0 {
		var ms []string
		for i, mem := range task.Memories {
			ms = append(ms, fmt.Sprintf("- Memory %d:\n  * Goal: %s\n  * Key Findings:\n%s\n  * Final Answer: %s",
				i+1, mem.Goal, indentText(mem.KeyFindings, "    "), mem.FinalAnswer))
		}
		memorySection = "\n\nRelated Historical Memories (RAG - Cross-task Knowledge Sharing):\n" + strings.Join(ms, "\n\n")
	}

	var toolsList []string
	for i, t := range tools.DefaultRegistry.List() {
		toolsList = append(toolsList, fmt.Sprintf("%d. %s: %s", i+1, t.Name(), t.Description()))
	}
	toolsString := strings.Join(toolsList, "\n")

	return fmt.Sprintf(`Task goal:
%s%s

Current status:
- step_count: %d
- max_steps: %d
- tool_budget: %d
- status: %s

Available tools:
%s

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
		formatUnresolved(task.Unresolved),
		summarizeTrace(task.Trace),
	)
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
	if len(traces) == 0 {
		return "No prior steps."
	}

	start := 0
	if len(traces) > 4 {
		start = len(traces) - 4
	}

	var lines []string
	for _, tr := range traces[start:] {
		lines = append(lines,
			fmt.Sprintf("Step %d: action=%s, query=%s, observation=%s",
				tr.Step, tr.Action, tr.Query, tr.Observation,
			),
		)
		for _, ev := range tr.Evidence {
			for _, line := range ev.Lines {
				lines = append(lines, fmt.Sprintf("Evidence: %s :: %s", ev.Path, line))
			}
		}
	}

	return strings.Join(lines, "\n")
}
