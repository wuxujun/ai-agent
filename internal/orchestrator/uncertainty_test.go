package orchestrator

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/uncertainty"
)

type fixedUncertaintyCalibrator struct{ calls int }

func (c *fixedUncertaintyCalibrator) Calibrate(_ context.Context, _ *types.Task, _ string) (*uncertainty.Result, types.TokenUsage, error) {
	c.calls++
	return &uncertainty.Result{Confidence: "medium", NeedsQualification: true, Reasons: []string{"limited_scope"}, Summary: "limited"}, types.TokenUsage{TotalTokens: 6}, nil
}

func TestCalibrateAnswerUncertaintyAppendsAdvisoryOnce(t *testing.T) {
	calibrator := &fixedUncertaintyCalibrator{}
	engine := &Engine{AnswerUncertaintyCalibrator: calibrator, LLMSceneEnabled: func(scene string) bool { return scene == config.LLMSceneAnswerUncertaintyCalibrator }}
	task := &types.Task{FinalAnswer: "answer", Trace: []types.StepTrace{{Action: "http_fetch", Observation: "partial result"}}}
	engine.calibrateAnswerUncertainty(context.Background(), task)
	engine.calibrateAnswerUncertainty(context.Background(), task)
	if calibrator.calls != 1 || !taskHasAction(task, uncertainty.TraceAction) || task.Trace[len(task.Trace)-1].TokenUsage.TotalTokens != 6 {
		t.Fatalf("calls=%d task=%+v", calibrator.calls, task)
	}
	if task.FinalAnswer != "answer\n\n## Evidence confidence\nMedium confidence. Treat affected claims as uncertain because of limited evidence scope." {
		t.Fatalf("answer=%q", task.FinalAnswer)
	}
}
