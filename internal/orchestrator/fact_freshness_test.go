package orchestrator

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/factfreshness"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fixedFactFreshnessChecker struct{ calls int }

func (c *fixedFactFreshnessChecker) Check(_ context.Context, _ *types.Task, _ string) (*factfreshness.Result, types.TokenUsage, error) {
	c.calls++
	return &factfreshness.Result{TimeSensitive: true, Status: "unknown", Reasons: []string{"missing_date"}, SourceIDs: []string{"E1"}, Summary: "missing date"}, types.TokenUsage{TotalTokens: 7}, nil
}

func TestCheckFactFreshnessRecordsRiskOnce(t *testing.T) {
	checker := &fixedFactFreshnessChecker{}
	engine := &Engine{FactFreshnessChecker: checker, LLMSceneEnabled: func(scene string) bool { return scene == config.LLMSceneFactFreshnessChecker }}
	task := &types.Task{FinalAnswer: "answer", Trace: []types.StepTrace{{Action: "web_search", Observation: "current value"}}}
	engine.checkFactFreshness(context.Background(), task)
	engine.checkFactFreshness(context.Background(), task)
	if checker.calls != 1 || !taskHasAction(task, factfreshness.TraceAction) {
		t.Fatalf("calls=%d task=%+v", checker.calls, task)
	}
	trace := task.Trace[len(task.Trace)-1]
	if trace.TokenUsage.TotalTokens != 7 || trace.Query != "unknown" || len(trace.Evidence) != 1 {
		t.Fatalf("trace=%+v", trace)
	}
}
