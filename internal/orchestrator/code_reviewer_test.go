package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/review"
	"github.com/wuxujun/ai-agent/internal/types"
)

type stubCodeReviewer struct {
	result *review.Result
	usage  types.TokenUsage
}

func (s stubCodeReviewer) Review(context.Context, *types.Task, review.ChangeSet) (*review.Result, types.TokenUsage, error) {
	return s.result, s.usage, nil
}

func TestReviewCodeChangesAppendsFindingsAndTrace(t *testing.T) {
	engine := &Engine{
		CodeReviewer:    stubCodeReviewer{result: &review.Result{Summary: "issue found", Findings: []review.Finding{{Severity: "high", Path: "main.go", Line: 12, Title: "unchecked error", Detail: "handle the returned error"}}}, usage: types.TokenUsage{TotalTokens: 9}},
		LLMSceneEnabled: func(scene string) bool { return scene == config.LLMSceneCodeReviewer },
		CollectCodeChanges: func(context.Context, string) (review.ChangeSet, error) {
			return review.ChangeSet{Paths: []string{"main.go"}, Diff: "+ risky()"}, nil
		},
	}
	task := &types.Task{FinalAnswer: "implemented", Trace: []types.StepTrace{{Action: "write_file"}}}
	engine.reviewCodeChanges(context.Background(), task)
	if !strings.Contains(task.FinalAnswer, "## Code review") || !strings.Contains(task.FinalAnswer, "main.go:12") {
		t.Fatalf("final answer=%q", task.FinalAnswer)
	}
	last := task.Trace[len(task.Trace)-1]
	if last.Action != "code_review" || last.TokenUsage.TotalTokens != 9 || len(last.Evidence) != 1 {
		t.Fatalf("trace=%+v", last)
	}
	engine.reviewCodeChanges(context.Background(), task)
	if len(task.Trace) != 2 {
		t.Fatalf("review should run once, traces=%d", len(task.Trace))
	}
}

func TestReviewCodeChangesSkipsWithoutMutatingAction(t *testing.T) {
	called := false
	engine := &Engine{
		CodeReviewer:    stubCodeReviewer{},
		LLMSceneEnabled: func(string) bool { return true },
		CollectCodeChanges: func(context.Context, string) (review.ChangeSet, error) {
			called = true
			return review.ChangeSet{}, nil
		},
	}
	task := &types.Task{FinalAnswer: "answer", Trace: []types.StepTrace{{Action: "read_file"}}}
	engine.reviewCodeChanges(context.Background(), task)
	if called || len(task.Trace) != 1 || task.FinalAnswer != "answer" {
		t.Fatalf("called=%t task=%+v", called, task)
	}
}
