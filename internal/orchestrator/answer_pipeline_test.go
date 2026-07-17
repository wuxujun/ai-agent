package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type pipelineCapture struct {
	status types.TaskStatus
	answer string
	calls  int
}

func (p *pipelineCapture) Process(_ context.Context, task *types.Task, _ string) (*types.AnswerAuditReport, error) {
	p.calls++
	p.status = task.Status
	p.answer = task.FinalAnswer
	report := &types.AnswerAuditReport{Publishable: true}
	task.AnswerAudit = report
	return report, nil
}

type pipelineErrorPlanner struct{}

func (pipelineErrorPlanner) PlanNext(context.Context, *types.Task, func(string)) (*planner.PlanDecision, error) {
	return nil, errors.New("boom")
}

func TestPipelineRunsAfterFailureNormalization(t *testing.T) {
	pipeline := &pipelineCapture{}
	engine := &Engine{Planner: pipelineErrorPlanner{}, Mode: ModeLegacy, AnswerPipeline: pipeline}
	task := &types.Task{ID: "failed", Status: types.StatusRunning, MaxSteps: 2, ToolBudget: 1}
	if err := engine.Next(context.Background(), task); err == nil {
		t.Fatal("expected execution error")
	}
	if pipeline.calls != 1 || pipeline.status != types.StatusFailed || pipeline.answer != "Failed: boom" {
		t.Fatalf("pipeline=%+v task=%+v", pipeline, task)
	}
}
