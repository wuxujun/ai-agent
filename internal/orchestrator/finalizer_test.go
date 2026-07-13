package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

type stubFinalizer struct {
	answer string
	err    error
}

func (s stubFinalizer) Finalize(context.Context, *types.Task) (string, types.TokenUsage, error) {
	return s.answer, types.TokenUsage{}, s.err
}

func TestFinalizeAnswer(t *testing.T) {
	task := &types.Task{ID: "task-finalize"}
	if got, _ := (&Engine{}).finalizeAnswer(context.Background(), task, "fallback"); got != "fallback" {
		t.Fatalf("nil finalizer result = %q", got)
	}
	if got, _ := (&Engine{Finalizer: stubFinalizer{answer: "synthesized"}}).finalizeAnswer(context.Background(), task, "fallback"); got != "fallback" {
		t.Fatalf("disabled finalizer result = %q", got)
	}
	engine := &Engine{Finalizer: usageFinalizer{answer: "synthesized", usage: types.TokenUsage{TotalTokens: 12}}, LLMSceneEnabled: func(string) bool { return true }}
	if got, usage := engine.finalizeAnswer(context.Background(), task, "fallback"); got != "synthesized" || usage.TotalTokens != 12 {
		t.Fatalf("enabled finalizer = %q, usage=%+v", got, usage)
	}
	engine.Finalizer = usageFinalizer{err: errors.New("failed")}
	if got, _ := engine.finalizeAnswer(context.Background(), task, "fallback"); got != "fallback" {
		t.Fatalf("failed finalizer result = %q", got)
	}
}

type usageFinalizer struct {
	answer string
	usage  types.TokenUsage
	err    error
}

func (s usageFinalizer) Finalize(context.Context, *types.Task) (string, types.TokenUsage, error) {
	return s.answer, s.usage, s.err
}

type stubCitationVerifier struct {
	result *planner.CitationVerification
	usage  types.TokenUsage
	err    error
	calls  int
}

func (s *stubCitationVerifier) Verify(context.Context, *types.Task, string) (*planner.CitationVerification, types.TokenUsage, error) {
	s.calls++
	return s.result, s.usage, s.err
}

type stopPlanner struct{}

func (stopPlanner) PlanNext(context.Context, *types.Task, func(string)) (*planner.PlanDecision, error) {
	return &planner.PlanDecision{Stop: true, FinalAnswer: "original"}, nil
}

func TestNextVerifiesCitationsAtSharedCompletionBoundary(t *testing.T) {
	verifier := &stubCitationVerifier{
		result: &planner.CitationVerification{Supported: true, VerifiedAnswer: "verified [E1]"},
		usage:  types.TokenUsage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
	}
	engine := &Engine{
		Planner:          stopPlanner{},
		CitationVerifier: verifier,
		Mode:             ModeLegacy,
		LLMSceneEnabled:  func(string) bool { return true },
	}
	task := &types.Task{
		ID:         "citation-task",
		Status:     types.StatusRunning,
		MaxSteps:   5,
		StepCount:  1,
		ToolBudget: 1,
		Trace:      []types.StepTrace{{Evidence: []types.Evidence{{Path: "source", Lines: []string{"fact"}}}}},
	}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.FinalAnswer != "verified [E1]" || verifier.calls != 1 {
		t.Fatalf("answer=%q calls=%d", task.FinalAnswer, verifier.calls)
	}
	last := task.Trace[len(task.Trace)-1]
	if last.Action != "citation_verify" || last.TokenUsage.TotalTokens != 6 {
		t.Fatalf("last trace = %+v", last)
	}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if verifier.calls != 1 {
		t.Fatalf("completed task was verified again: calls=%d", verifier.calls)
	}
}

func TestVerifyCitationsFailsOpen(t *testing.T) {
	verifier := &stubCitationVerifier{err: errors.New("unavailable")}
	engine := &Engine{CitationVerifier: verifier, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{
		ID:          "citation-failure",
		FinalAnswer: "original",
		Trace:       []types.StepTrace{{Evidence: []types.Evidence{{Lines: []string{"fact"}}}}},
	}
	engine.verifyCitations(context.Background(), task)
	if task.FinalAnswer != "original" || len(task.Trace) != 1 {
		t.Fatalf("task changed after verifier failure: %+v", task)
	}
}
