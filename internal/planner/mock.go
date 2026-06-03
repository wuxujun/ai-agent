package planner

import (
	"context"
	"log"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

type MockPlanner struct{}

func (m *MockPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*PlanDecision, error) {
	log.Printf("[Mock Planner] Generating decision for task %s (StepCount: %d)", task.ID, task.StepCount)

	var decision *PlanDecision

	if task.StepCount == 0 {
		decision = &PlanDecision{
			ThoughtSummary: "Find candidate files first",
			Action:         "find_files",
			Parameters: map[string]any{
				"pattern": "*",
			},
		}
	} else if task.StepCount == 1 {
		parts := strings.Fields(task.Goal)
		if len(parts) == 0 {
			decision = &PlanDecision{
				ThoughtSummary: "Goal is empty, stopping",
				Action:         "stop",
				Stop:           true,
				FinalAnswer:    "empty goal provided",
			}
		} else {
			q := parts[len(parts)-1]
			decision = &PlanDecision{
				ThoughtSummary: "Search likely keyword in candidate files",
				Action:         "search_text",
				Parameters: map[string]any{
					"query": q,
					"glob":  "",
				},
			}
		}
	} else if task.StepCount == 2 && len(task.Trace) >= 2 && len(task.Trace[1].Evidence) > 0 {
		decision = &PlanDecision{
			ThoughtSummary: "Read the best candidate file",
			Action:         "read_file",
			Parameters: map[string]any{
				"path": task.Trace[1].Evidence[0].Path,
			},
		}
	} else {
		decision = &PlanDecision{
			ThoughtSummary: "Enough work completed",
			Stop:           true,
			FinalAnswer:    "planner stopped after completing available steps",
			Action:         "none",
			Parameters:     map[string]any{},
		}
	}

	log.Printf("[Mock Planner] Task %s decision - Thought: %q | Action: %q | Stop: %t | FinalAnswer: %q | Parameters: %+v",
		task.ID, decision.ThoughtSummary, decision.Action, decision.Stop, decision.FinalAnswer, decision.Parameters)

	return decision, nil
}
