package orchestrator

import (
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

const (
	limitReasonStepOrToolBudget = "step_or_tool_budget"
	limitReasonTokenBudget      = "token_budget"
)

func finalAnswerForLimit(task *types.Task, reason string) string {
	if existing := strings.TrimSpace(task.FinalAnswer); existing != "" {
		return existing
	}

	var b strings.Builder
	switch reason {
	case limitReasonTokenBudget:
		b.WriteString("Stopped before a final answer could be produced because the token budget was reached.")
	default:
		b.WriteString("Stopped before a final answer could be produced because the step or tool budget limit was reached.")
	}
	if task.Goal != "" {
		b.WriteString("\n\nGoal: ")
		b.WriteString(task.Goal)
	}

	progress := taskProgressSummary(task, 5)
	if len(progress) == 0 {
		b.WriteString("\n\nNo tool observations were recorded before the limit was reached. Increase max_steps, tool_budget, or token_budget and run the task again.")
		return b.String()
	}

	b.WriteString("\n\nProgress so far:")
	for _, item := range progress {
		b.WriteString("\n- ")
		b.WriteString(item)
	}
	b.WriteString("\n\nIncrease max_steps, tool_budget, or token_budget to continue from this partial result.")
	return b.String()
}

func taskProgressSummary(task *types.Task, limit int) []string {
	if limit <= 0 || len(task.Trace) == 0 {
		return nil
	}

	start := len(task.Trace) - limit
	if start < 0 {
		start = 0
	}

	out := make([]string, 0, len(task.Trace)-start)
	for _, tr := range task.Trace[start:] {
		text := strings.TrimSpace(tr.Observation)
		if text == "" {
			text = strings.TrimSpace(tr.Error)
		}
		if text == "" && len(tr.Evidence) > 0 {
			text = evidenceSummary(tr.Evidence)
		}
		if text == "" {
			continue
		}

		prefix := strings.TrimSpace(tr.Action)
		if prefix == "" {
			prefix = fmt.Sprintf("step %d", tr.Step)
		}
		if q := strings.TrimSpace(tr.Query); q != "" {
			prefix += " " + q
		}
		out = append(out, prefix+": "+truncateSummary(text, 500))
	}
	return out
}

func evidenceSummary(evidence []types.Evidence) string {
	items := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		if ev.Path != "" {
			items = append(items, ev.Path)
		}
	}
	if len(items) == 0 {
		return fmt.Sprintf("%d evidence item(s) collected", len(evidence))
	}
	return "evidence: " + strings.Join(items, ", ")
}

func truncateSummary(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
