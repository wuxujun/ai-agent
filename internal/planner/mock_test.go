package planner

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestMockPlannerNoPanicOnShortTrace(t *testing.T) {
	mp := &MockPlanner{}

	// Recreate the scenario: StepCount = 2, but Trace only has 0, 1, or 2 elements.
	// Since step 1 didn't have evidence, it falls through to the final else.
	for i := 0; i <= 2; i++ {
		trace := make([]types.StepTrace, i)
		for idx := range trace {
			trace[idx] = types.StepTrace{
				Query: "test-query",
			}
		}

		task := &types.Task{
			ID:        "test-task",
			StepCount: 2,
			Trace:     trace,
			Goal:      "test-goal",
		}

		decision, err := mp.PlanNext(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("PlanNext returned error: %v", err)
		}
		if decision == nil {
			t.Fatal("expected non-nil decision")
		}
		if !decision.Stop {
			t.Error("expected decision to stop")
		}
		expectedAnswerPart := "test-query"
		if i == 0 {
			expectedAnswerPart = "test-goal"
		}
		if decision.FinalAnswer != "The answer is inside "+expectedAnswerPart {
			t.Errorf("got FinalAnswer %q, expected containing %q", decision.FinalAnswer, expectedAnswerPart)
		}
	}
}
