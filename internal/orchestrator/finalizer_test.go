package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/tools"
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
	if _, _, reason := (&Engine{}).finalizeAnswerDetailed(context.Background(), task, "fallback"); reason != "not_initialized" {
		t.Fatalf("nil finalizer reason = %q", reason)
	}
	if got, _ := (&Engine{}).finalizeAnswer(context.Background(), task, "fallback"); got != "fallback" {
		t.Fatalf("nil finalizer result = %q", got)
	}
	disabled := &Engine{Finalizer: stubFinalizer{answer: "synthesized"}, LLMSceneEnabled: func(string) bool { return false }}
	if got, _ := disabled.finalizeAnswer(context.Background(), task, "fallback"); got != "fallback" {
		t.Fatalf("disabled finalizer result = %q", got)
	}
	if _, _, reason := disabled.finalizeAnswerDetailed(context.Background(), task, "fallback"); reason != "scene_disabled" {
		t.Fatalf("disabled finalizer reason = %q", reason)
	}
	engine := &Engine{Finalizer: usageFinalizer{answer: "synthesized", usage: types.TokenUsage{TotalTokens: 12}}, LLMSceneEnabled: func(string) bool { return true }}
	if got, usage := engine.finalizeAnswer(context.Background(), task, "fallback"); got != "synthesized" || usage.TotalTokens != 12 {
		t.Fatalf("enabled finalizer = %q, usage=%+v", got, usage)
	}
	engine.Finalizer = usageFinalizer{err: errors.New("failed")}
	if got, _, reason := engine.finalizeAnswerDetailed(context.Background(), task, "fallback"); got != "fallback" || reason != "provider_error" {
		t.Fatalf("failed finalizer result = %q", got)
	}
}

func TestFinalizeBeforeRetrievalExpansionReservesAnswerCapacity(t *testing.T) {
	engine := &Engine{
		Finalizer:       stubFinalizer{answer: "基于现有证据的顾问名单"},
		LLMSceneEnabled: func(string) bool { return true },
	}
	task := &types.Task{
		ID:         "budget-aware-finalize",
		Goal:       "数学科学术顾问有哪些",
		Status:     types.StatusRunning,
		MaxSteps:   6,
		StepCount:  4,
		ToolBudget: 2,
		Trace: []types.StepTrace{{
			Action:      "rag_fetch",
			Observation: "fetched 2 rag item(s)",
			Evidence:    []types.Evidence{{Path: "rag-a", Lines: []string{"顾问甲"}}},
		}},
	}
	decision := &planner.PlanDecision{
		Actions:    []planner.ActionCall{{Action: "rag_search", Parameters: map[string]any{"query": "另一个相似查询"}}},
		TokenUsage: types.TokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
	}
	if !engine.finalizeBeforeRetrievalExpansion(context.Background(), task, decision) {
		t.Fatal("expected retrieval expansion to be replaced by finalization")
	}
	if task.Status != types.StatusCompleted || task.FinalAnswer != "基于现有证据的顾问名单" {
		t.Fatalf("status=%s answer=%q", task.Status, task.FinalAnswer)
	}
	last := task.Trace[len(task.Trace)-1]
	if last.Action != "budget_finalize" || last.TokenUsage.TotalTokens != 10 {
		t.Fatalf("finalization trace=%+v", last)
	}
}

func TestFinalizeBeforeRetrievalExpansionMarksPartialWhenFinalizerUnavailable(t *testing.T) {
	engine := &Engine{LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{
		ID: "finalizer-unavailable", Goal: "数学学术顾问有哪些", Status: types.StatusRunning,
		MaxSteps: 8, StepCount: 6, ToolBudget: 2,
		Trace: []types.StepTrace{{Action: "rag_fetch", Evidence: []types.Evidence{{Path: "rag-a", Lines: []string{"顾问甲"}}}}},
	}
	decision := &planner.PlanDecision{Actions: []planner.ActionCall{{Action: "rag_fetch", Parameters: map[string]any{"ids": []string{"rag-b"}}}}}
	if !engine.finalizeBeforeRetrievalExpansion(context.Background(), task, decision) {
		t.Fatal("capacity guard did not stop retrieval")
	}
	if task.Status != types.StatusPartial {
		t.Fatalf("status=%s, want partial", task.Status)
	}
	if last := task.Trace[len(task.Trace)-1]; last.Action != "retrieval_guard" {
		t.Fatalf("last trace=%+v", last)
	}
}

func TestFinalizeBeforeRetrievalExpansionBlocksEquivalentCachedSearch(t *testing.T) {
	taskID := "equivalent-search-guard"
	tools.ClearRetrievalContext(taskID)
	t.Cleanup(func() { tools.ClearRetrievalContext(taskID) })
	oldTools := make(map[string]tools.Tool)
	for _, name := range []string{"rag_search", "rag_fetch", "memory_search", "memory_get"} {
		oldTools[name], _ = tools.Get(name)
	}
	tools.RegisterRetrievalTools(tools.RetrievalDependencies{SearchRAG: func(context.Context, string) ([]types.Memory, error) {
		return []types.Memory{{Goal: "数学学术顾问", KeyFindings: "顾问甲"}}, nil
	}})
	t.Cleanup(func() {
		for _, tool := range oldTools {
			if tool != nil {
				tools.Register(tool)
			}
		}
	})
	search, _ := tools.Get("rag_search")
	ctx := tools.WithRetrievalExecutionContext(context.Background(), taskID, "default")
	if _, err := search.Execute(ctx, "", map[string]any{"query": "数学学术顾问有哪些", "top_k": 1}); err != nil {
		t.Fatalf("seed retrieval state: %v", err)
	}

	engine := &Engine{Finalizer: stubFinalizer{answer: "顾问甲"}, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{
		ID: taskID, Goal: "数学学术顾问有哪些", Status: types.StatusRunning, MaxSteps: 8, ToolBudget: 6,
		Trace: []types.StepTrace{{Action: "rag_fetch", Evidence: []types.Evidence{{Path: "rag-a", Lines: []string{"顾问甲"}}}}},
	}
	refinement := &planner.PlanDecision{Actions: []planner.ActionCall{{Action: "rag_search", Parameters: map[string]any{"query": "数学学术顾问 成员 姓名 老师 团队"}}}}
	if engine.finalizeBeforeRetrievalExpansion(context.Background(), task, refinement) {
		t.Fatal("refinement query must be allowed while execution capacity remains")
	}
	decision := &planner.PlanDecision{Actions: []planner.ActionCall{{Action: "rag_search", Parameters: map[string]any{"query": "数学 学术顾问"}}}}
	if !engine.finalizeBeforeRetrievalExpansion(context.Background(), task, decision) {
		t.Fatal("equivalent cached search was not blocked")
	}
	if task.Status != types.StatusCompleted || task.FinalAnswer != "顾问甲" {
		t.Fatalf("status=%s answer=%q", task.Status, task.FinalAnswer)
	}
	state := tools.RetrievalStateForTask(taskID)
	if state.NetworkSearchCalls["rag"] != 1 || state.RetrievalCycles["rag"] != 1 {
		t.Fatalf("equivalent query consumed another retrieval cycle: %+v", state)
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

type countingFinalizer struct{ calls int }

func (f *countingFinalizer) Finalize(context.Context, *types.Task) (string, types.TokenUsage, error) {
	f.calls++
	return "rewritten", types.TokenUsage{TotalTokens: 10}, nil
}

func TestPlannerStopWithAnswerSkipsFinalizer(t *testing.T) {
	finalizer := &countingFinalizer{}
	engine := &Engine{
		Planner: stopPlanner{}, Finalizer: finalizer,
		Mode: ModeLegacy, LLMSceneEnabled: func(string) bool { return true },
	}
	task := &types.Task{ID: "planner-answer", Status: types.StatusRunning, MaxSteps: 3, ToolBudget: 3}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("next: %v", err)
	}
	if finalizer.calls != 0 || task.FinalAnswer != "original" || task.Status != types.StatusCompleted {
		t.Fatalf("calls=%d status=%s answer=%q", finalizer.calls, task.Status, task.FinalAnswer)
	}
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
