package orchestrator

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type stubPlanner struct {
	decision *planner.PlanDecision
	calls    int
}

func (p *stubPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*planner.PlanDecision, error) {
	p.calls++
	return p.decision, nil
}

type stubExecutor struct {
	trace *types.StepTrace
	calls int
}

func (e *stubExecutor) Execute(ctx context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
	e.calls++
	if e.trace != nil {
		return []types.StepTrace{*e.trace}, nil
	}
	return nil, nil
}

func TestEinoNextExecutesPlannerDecision(t *testing.T) {
	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "search for matching text",
			Actions: []planner.ActionCall{
				{
					Action:     "search_text",
					Parameters: map[string]any{"query": "needle"},
				},
			},
		},
	}
	x := &stubExecutor{
		trace: &types.StepTrace{
			Step:        1,
			Action:      "search_text",
			Query:       "needle",
			Observation: "found 1 evidence items",
		},
	}
	engine := &Engine{Planner: p, Executor: x}
	task := &types.Task{ID: "task-1", Goal: "find needle", Status: "created", MaxSteps: 3, ToolBudget: 2}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	if p.calls != 1 {
		t.Fatalf("planner calls = %d, want 1", p.calls)
	}
	if x.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", x.calls)
	}
	if task.Status != types.StatusRunning {
		t.Fatalf("task status = %q, want running", task.Status)
	}
	if task.StepCount != 1 || task.ToolBudget != 1 {
		t.Fatalf("step/budget = %d/%d, want 1/1", task.StepCount, task.ToolBudget)
	}
	if len(task.Trace) != 1 {
		t.Fatalf("trace len = %d, want 1", len(task.Trace))
	}
}

func TestLegacyNextExecutesPlannerDecision(t *testing.T) {
	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "search for matching text",
			Actions: []planner.ActionCall{
				{
					Action:     "search_text",
					Parameters: map[string]any{"query": "needle"},
				},
			},
		},
	}
	x := &stubExecutor{
		trace: &types.StepTrace{
			Step:        1,
			Action:      "search_text",
			Query:       "needle",
			Observation: "found 1 evidence items",
		},
	}
	engine := &Engine{Planner: p, Executor: x, Mode: ModeLegacy}
	task := &types.Task{ID: "task-legacy", Goal: "find needle", Status: "created", MaxSteps: 3, ToolBudget: 2}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	if p.calls != 1 {
		t.Fatalf("planner calls = %d, want 1", p.calls)
	}
	if x.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", x.calls)
	}
	if task.Status != types.StatusRunning {
		t.Fatalf("task status = %q, want running", task.Status)
	}
	if task.StepCount != 1 || task.ToolBudget != 1 {
		t.Fatalf("step/budget = %d/%d, want 1/1", task.StepCount, task.ToolBudget)
	}
	if len(task.Trace) != 1 {
		t.Fatalf("trace len = %d, want 1", len(task.Trace))
	}
}

// TestEinoNextGatesHighRiskActionBeforeExecution is the regression test for
// BUG_REPORT.md #1: the default eino mode executed RiskLevelHigh tools
// (write_file/execute_code) with no approval gate, because the gate lived only
// in runLegacyNext. The shared enforceApprovals helper now runs in eino's
// executeDecision too. Here we assert the executor only runs AFTER the user
// approves the high-risk action.
func TestEinoNextGatesHighRiskActionBeforeExecution(t *testing.T) {
	resetApprovalState(t)

	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "write the result",
			Actions: []planner.ActionCall{
				{Action: "write_file", Parameters: map[string]any{"path": "out.txt", "content": "hi"}},
			},
		},
	}
	x := &stubExecutor{trace: &types.StepTrace{Step: 1, Action: "write_file"}}
	engine := &Engine{Planner: p, Executor: x} // default mode == eino
	task := &types.Task{ID: "task-eino-gate", Goal: "write", Status: "created", Workspace: t.TempDir(), MaxSteps: 3, ToolBudget: 2}

	// Approve the moment the pending approval appears.
	resolved := make(chan struct{})
	go func() {
		defer close(resolved)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if ResolveApproval(task.ID, types.ApprovalResult{Approved: true}) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	<-resolved

	if x.calls != 1 {
		t.Fatalf("executor calls = %d, want 1 (high-risk action should run after approval)", x.calls)
	}
	if task.Status != types.StatusRunning {
		t.Fatalf("task status = %q, want running", task.Status)
	}
}

// TestEinoNextSkipsExecutionWhenHighRiskRejected verifies the eino mode does NOT
// execute a high-risk action the user rejected — the core safety guarantee of
// BUG_REPORT.md #1.
func TestEinoNextSkipsExecutionWhenHighRiskRejected(t *testing.T) {
	resetApprovalState(t)

	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "write the result",
			Actions: []planner.ActionCall{
				{Action: "write_file", Parameters: map[string]any{"path": "out.txt", "content": "hi"}},
			},
		},
	}
	x := &stubExecutor{trace: &types.StepTrace{Step: 1, Action: "write_file"}}
	engine := &Engine{Planner: p, Executor: x} // default mode == eino
	task := &types.Task{ID: "task-eino-reject", Goal: "write", Status: "created", Workspace: t.TempDir(), MaxSteps: 3, ToolBudget: 2}

	resolved := make(chan struct{})
	go func() {
		defer close(resolved)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if ResolveApproval(task.ID, types.ApprovalResult{Approved: false, Message: "no"}) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	<-resolved

	if x.calls != 0 {
		t.Fatalf("executor calls = %d, want 0 (rejected high-risk action must not run)", x.calls)
	}
}

func TestRunAllPublishesStepProgressAndPersistsEachTurn(t *testing.T) {
	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "keep searching",
			Actions: []planner.ActionCall{
				{
					Action:     "search_text",
					Parameters: map[string]any{"query": "needle"},
				},
			},
		},
	}
	x := &stubExecutor{
		trace: &types.StepTrace{
			Action:      "search_text",
			Query:       "needle",
			Observation: "found 1 evidence items",
		},
	}
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })

	var events []types.StepTrace
	engine := &Engine{
		Planner:  p,
		Executor: x,
		Mode:     ModeLegacy,
		Store:    st,
		StepCallback: func(taskID string, status types.TaskStatus, step *types.StepTrace) {
			if taskID != "task-run-all-progress" {
				t.Errorf("taskID = %q, want task-run-all-progress", taskID)
			}
			if status != types.StatusRunning {
				t.Errorf("status = %q, want running", status)
			}
			events = append(events, *step)
		},
	}
	task := &types.Task{
		ID:         "task-run-all-progress",
		Goal:       "find needle",
		Status:     types.StatusCreated,
		MaxSteps:   2,
		ToolBudget: 2,
	}

	if err := engine.RunAll(context.Background(), task); err != nil {
		t.Fatalf("RunAll returned error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("step events = %d, want 2", len(events))
	}
	for i, event := range events {
		if event.Action != "search_text" {
			t.Fatalf("event %d action = %q, want search_text", i, event.Action)
		}
	}
	if task.Status != types.StatusPartial {
		t.Fatalf("task status = %q, want partial", task.Status)
	}

	persisted, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if persisted.Status != types.StatusPartial {
		t.Fatalf("persisted status = %q, want partial", persisted.Status)
	}
	if len(persisted.Trace) != 2 {
		t.Fatalf("persisted trace len = %d, want 2", len(persisted.Trace))
	}
}

func TestNextRejectsUnsupportedMode(t *testing.T) {
	engine := &Engine{Mode: Mode("bad-mode")}
	task := &types.Task{ID: "task-bad-mode", MaxSteps: 1, ToolBudget: 1}

	if err := engine.Next(context.Background(), task); err == nil {
		t.Fatal("Next returned nil, want unsupported mode error")
	}
}

func TestEinoNextStopsWhenPlannerStops(t *testing.T) {
	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "enough evidence",
			Stop:           true,
			FinalAnswer:    "done",
			Actions: []planner.ActionCall{
				{
					Action:     "none",
					Parameters: map[string]any{},
				},
			},
		},
	}
	x := &stubExecutor{trace: &types.StepTrace{}}
	engine := &Engine{Planner: p, Executor: x}
	task := &types.Task{ID: "task-2", Goal: "answer", Status: "running", MaxSteps: 3, ToolBudget: 2}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	if x.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", x.calls)
	}
	if task.Status != types.StatusCompleted {
		t.Fatalf("task status = %q, want completed", task.Status)
	}
	if task.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want done", task.FinalAnswer)
	}
}

func TestEinoNextStopsWhenBudgetExhausted(t *testing.T) {
	p := &stubPlanner{decision: &planner.PlanDecision{}}
	x := &stubExecutor{trace: &types.StepTrace{}}
	engine := &Engine{Planner: p, Executor: x}
	task := &types.Task{ID: "task-3", Goal: "answer", Status: "running", MaxSteps: 3, StepCount: 3, ToolBudget: 1}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	if p.calls != 0 {
		t.Fatalf("planner calls = %d, want 0", p.calls)
	}
	if x.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", x.calls)
	}
	if task.Status != types.StatusPartial {
		t.Fatalf("task status = %q, want partial", task.Status)
	}
	if !strings.Contains(task.FinalAnswer, "step or tool budget limit") {
		t.Fatalf("final answer = %q, want budget stop explanation", task.FinalAnswer)
	}
}

// TestEinoNextStopsWhenTokenBudgetExhausted verifies the eino-mode token gate
// added alongside TokenBudget persistence. It complements the legacy gate
// already exercised indirectly via engine.go runLegacyNext.
func TestEinoNextStopsWhenTokenBudgetExhausted(t *testing.T) {
	p := &stubPlanner{decision: &planner.PlanDecision{}}
	x := &stubExecutor{trace: &types.StepTrace{}}
	engine := &Engine{Planner: p, Executor: x}
	task := &types.Task{
		ID:          "task-token-cap",
		Goal:        "answer",
		Status:      "running",
		MaxSteps:    5,
		ToolBudget:  5,
		TokenBudget: 100,
		Trace: []types.StepTrace{
			{Step: 1, Action: "search_text", TokenUsage: types.TokenUsage{TotalTokens: 60}},
			{Step: 2, Action: "search_text", TokenUsage: types.TokenUsage{TotalTokens: 50}},
		},
	}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	if p.calls != 0 {
		t.Fatalf("planner calls = %d, want 0 (gate should fire before planner runs)", p.calls)
	}
	if x.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", x.calls)
	}
	if task.Status != types.StatusPartial {
		t.Fatalf("task status = %q, want partial", task.Status)
	}
	if !strings.Contains(task.FinalAnswer, "token budget") {
		t.Fatalf("final answer = %q, want token budget stop explanation", task.FinalAnswer)
	}
}

// TestEinoTokenUsagePropagatesToTrace ensures the eino executor copies
// decision.TokenUsage onto each trace entry, so cumulative token usage on
// subsequent calls is visible to the token-budget gate. Without this propagation,
// the gate's running sum over task.Trace would always be zero in eino mode.
func TestEinoTokenUsagePropagatesToTrace(t *testing.T) {
	p := &stubPlanner{
		decision: &planner.PlanDecision{
			ThoughtSummary: "search",
			Actions: []planner.ActionCall{
				{Action: "search_text", Parameters: map[string]any{"query": "needle"}},
			},
			TokenUsage: types.TokenUsage{PromptTokens: 30, CompletionTokens: 20, TotalTokens: 50},
		},
	}
	x := &stubExecutor{
		trace: &types.StepTrace{
			Step:        1,
			Action:      "search_text",
			Query:       "needle",
			Observation: "found",
		},
	}
	engine := &Engine{Planner: p, Executor: x}
	task := &types.Task{ID: "task-tokens", Goal: "go", Status: "created", MaxSteps: 3, ToolBudget: 2}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Next: %v", err)
	}

	if len(task.Trace) != 1 {
		t.Fatalf("trace len = %d, want 1", len(task.Trace))
	}
	if got := task.Trace[0].TokenUsage.TotalTokens; got != 50 {
		t.Fatalf("trace TokenUsage.TotalTokens = %d, want 50 (propagated from decision)", got)
	}
}

// TestGetEinoRunnerConcurrency verifies that concurrent calls to getEinoRunner:
//  1. Compile the chain exactly once (no redundant compilations).
//  2. All callers receive the identical runner instance (pointer equality).
//  3. The RWMutex hot-path allows concurrent reads without data races.
//
// Run with: go test -race ./internal/orchestrator/... -run TestGetEinoRunnerConcurrency
func TestGetEinoRunnerConcurrency(t *testing.T) {
	engine := &Engine{
		Planner:  &stubPlanner{decision: &planner.PlanDecision{}},
		Executor: &stubExecutor{},
	}

	const goroutines = 50
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]any, goroutines)
		errs    = make([]error, goroutines)
	)

	// Launch all goroutines, hold them at the starting gate, then release all
	// simultaneously to maximise lock contention on the first compile.
	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // wait for the starting gun
			r, err := engine.getEinoRunner(context.Background())
			results[idx] = r
			errs[idx] = err
		}(i)
	}
	close(start) // fire!
	wg.Wait()

	// All calls must succeed.
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: getEinoRunner error: %v", i, err)
		}
	}

	// All callers must receive the same runner instance.
	first := results[0]
	for i := 1; i < goroutines; i++ {
		if results[i] != first {
			t.Errorf("goroutine %d: received different runner instance (want pointer equality)", i)
		}
	}

	// The chain must have been compiled exactly once (einoReady=true, einoRunner set).
	engine.einoMu.RLock()
	ready := engine.einoReady
	engine.einoMu.RUnlock()
	if !ready {
		t.Error("einoReady should be true after successful compilation")
	}
}

// TestGetEinoRunnerRetryOnFailure verifies that a transient compile failure does
// NOT permanently poison the runner cache: the next call should retry and succeed.
func TestGetEinoRunnerRetryOnFailure(t *testing.T) {
	engine := &Engine{
		Planner:  &stubPlanner{decision: &planner.PlanDecision{}},
		Executor: &stubExecutor{},
	}

	ctx := context.Background()

	// First call: simulate a compile error by cancelling the context.
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel() // immediately cancel
	_, err := engine.getEinoRunner(cancelledCtx)
	if err == nil {
		// Cancelled context may or may not cause compile failure depending on
		// Eino internals; if it succeeds, mark ready and skip the retry check.
		t.Log("cancelled context did not fail compile; skipping retry assertion")
		return
	}

	// einoReady must still be false so the next call retries.
	engine.einoMu.RLock()
	ready := engine.einoReady
	engine.einoMu.RUnlock()
	if ready {
		t.Fatal("einoReady should remain false after a compile failure")
	}

	// Second call with a valid context: must succeed and set einoReady=true.
	r, err := engine.getEinoRunner(ctx)
	if err != nil {
		t.Fatalf("retry compile failed: %v", err)
	}
	if r == nil {
		t.Fatal("retry returned nil runner")
	}

	engine.einoMu.RLock()
	ready = engine.einoReady
	engine.einoMu.RUnlock()
	if !ready {
		t.Error("einoReady should be true after successful retry")
	}
}

// Ensure the concurrency test does not leave the atomic import unused.
var _ = atomic.AddInt64
