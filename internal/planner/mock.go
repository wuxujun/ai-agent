package planner

import (
	"context"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

type MockPlanner struct{}

func (m *MockPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*PlanDecision, error) {
	log.Info("generating decision", "task_id", task.ID, "step_count", task.StepCount)

	var decision *PlanDecision

	if task.StepCount == 0 {
		decision = &PlanDecision{
			ThoughtSummary: "Find candidate files first",
			Actions: []ActionCall{
				{
					Action: "find_files",
					Parameters: map[string]any{
						"pattern": "*",
					},
				},
			},
		}
	} else if task.StepCount == 1 {
		parts := strings.Fields(task.Goal)
		if len(parts) == 0 {
			decision = &PlanDecision{
				ThoughtSummary: "Goal is empty, stopping",
				Stop:           true,
				FinalAnswer:    "empty goal provided",
				Actions: []ActionCall{
					{
						Action: "none",
						Parameters: map[string]any{},
					},
				},
			}
		} else {
			q := parts[len(parts)-1]
			decision = &PlanDecision{
				ThoughtSummary: "Search likely keyword in candidate files",
				Actions: []ActionCall{
					{
						Action: "search_text",
						Parameters: map[string]any{
							"query": q,
							"glob":  "",
						},
					},
				},
			}
		}
	} else if task.StepCount == 2 && len(task.Trace) >= 2 && len(task.Trace[1].Evidence) > 0 {
		decision = &PlanDecision{
			ThoughtSummary: "Read the best candidate file",
			Actions: []ActionCall{
				{
					Action: "read_file",
					Parameters: map[string]any{
						"path": task.Trace[1].Evidence[0].Path,
					},
				},
			},
		}
	} else {
		decision = &PlanDecision{
			ThoughtSummary: "Answer found in file",
			Stop:           true,
			FinalAnswer:    "The answer is inside " + task.Trace[2].Query,
			Actions: []ActionCall{
				{
					Action:     "none",
					Parameters: map[string]any{},
				},
			},
		}
	}

	log.Info("decision ready", "task_id", task.ID, "thought", decision.ThoughtSummary, "num_actions", len(decision.Actions), "stop", decision.Stop, "final_answer", decision.FinalAnswer)

	return decision, nil
}
