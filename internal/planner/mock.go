package planner

import (
	"context"
	"strings"

	"github.com/wuxujun/ai-agent/pkg/types"
)

type MockPlanner struct{}

func (m *MockPlanner) PlanNext(ctx context.Context, task *types.Task) (*PlanDecision, error) {
	if task.StepCount == 0 {
		return &PlanDecision{
			ThoughtSummary: "Find candidate text files first",
			Action:         "find_files",
			Parameters: map[string]any{
				"pattern": "*.txt",
			},
		}, nil
	}

	if task.StepCount == 1 {
		parts := strings.Fields(task.Goal)
		q := "TODO"
		if len(parts) > 0 {
			q = parts[len(parts)-1]
		}
		return &PlanDecision{
			ThoughtSummary: "Search likely keyword in text files",
			Action:         "search_text",
			Parameters: map[string]any{
				"query": q,
				"glob":  "*.txt",
			},
		}, nil
	}

	if task.StepCount == 2 && len(task.Trace) >= 2 && len(task.Trace[1].Evidence) > 0 {
		return &PlanDecision{
			ThoughtSummary: "Read the best candidate file",
			Action:         "read_file",
			Parameters: map[string]any{
				"path": task.Trace[1].Evidence[0].Path,
			},
		}, nil
	}

	return &PlanDecision{
		ThoughtSummary: "Enough work completed",
		Stop:           true,
		FinalAnswer:    "planner stopped after completing available steps",
		Action:         "none",
		Parameters:     map[string]any{},
	}, nil
}
