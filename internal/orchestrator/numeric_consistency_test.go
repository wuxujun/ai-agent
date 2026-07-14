package orchestrator

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/numericconsistency"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fixedNumericConsistencyChecker struct{ calls int }

func (c *fixedNumericConsistencyChecker) Check(_ context.Context, _ *types.Task, _ string) (*numericconsistency.Result, types.TokenUsage, error) {
	c.calls++
	return &numericconsistency.Result{HasNumericClaims: true, Status: "inconsistent", Reasons: []string{"value_mismatch"}, SourceIDs: []string{"E1"}, Summary: "mismatch"}, types.TokenUsage{TotalTokens: 8}, nil
}

func TestCheckNumericConsistencyRecordsRiskOnce(t *testing.T) {
	checker := &fixedNumericConsistencyChecker{}
	engine := &Engine{NumericConsistencyChecker: checker, LLMSceneEnabled: func(scene string) bool { return scene == config.LLMSceneNumericConsistencyChecker }}
	task := &types.Task{FinalAnswer: "answer is 20", Trace: []types.StepTrace{{Action: "web_search", Observation: "answer is 10"}}}
	engine.checkNumericConsistency(context.Background(), task)
	engine.checkNumericConsistency(context.Background(), task)
	if checker.calls != 1 || !taskHasAction(task, numericconsistency.TraceAction) {
		t.Fatalf("calls=%d task=%+v", checker.calls, task)
	}
	trace := task.Trace[len(task.Trace)-1]
	if trace.TokenUsage.TotalTokens != 8 || trace.Query != "inconsistent" || len(trace.Evidence) != 1 {
		t.Fatalf("trace=%+v", trace)
	}
}
