package planner

import (
	"fmt"
	"strings"

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
	return fmt.Sprintf(`Task goal:
%s

Current status:
- step_count: %d
- max_steps: %d
- tool_budget: %d
- status: %s

Available tools:
1. find_files(pattern): find candidate files by glob inside the workspace
2. search_text(query, glob?): search file contents for a term
3. read_file(path): read a small text file for local context

Unresolved questions:
%s

Recent trace:
%s

Decision requirements:
- Choose exactly one next action.
- If enough evidence exists, stop and provide final_answer.
- If stopping, set action to "none".`,
		task.Goal,
		task.StepCount,
		task.MaxSteps,
		task.ToolBudget,
		task.Status,
		formatUnresolved(task.Unresolved),
		summarizeTrace(task.Trace),
	)
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
