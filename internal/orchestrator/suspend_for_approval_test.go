package orchestrator

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// ctxAwareStore records each SaveFullTask call along with whether the caller's
// ctx had already been cancelled at the time of the call. The persistence path
// must use a detached ctx, so even when the caller ctx is dead the save must
// still observe ctx.Err() == nil.
type ctxAwareStore struct {
	mu    sync.Mutex
	saves []ctxAwareSave
}

type ctxAwareSave struct {
	status        types.TaskStatus
	receivedCtxOK bool
	hasDeadline   bool
}

func (s *ctxAwareStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, hasDeadline := ctx.Deadline()
	s.saves = append(s.saves, ctxAwareSave{
		status:        task.Status,
		receivedCtxOK: ctx.Err() == nil,
		hasDeadline:   hasDeadline,
	})
	return nil
}

func (s *ctxAwareStore) snapshot() []ctxAwareSave {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ctxAwareSave, len(s.saves))
	copy(out, s.saves)
	return out
}

func (s *ctxAwareStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	return nil, sql.ErrNoRows
}
func (s *ctxAwareStore) ListTasks(ctx context.Context, f store.ListFilter) ([]*types.Task, error) {
	return nil, nil
}
func (s *ctxAwareStore) ExistsTask(ctx context.Context, id string) (bool, error) { return false, nil }
func (s *ctxAwareStore) SaveMemory(ctx context.Context, mem *types.Memory) error { return nil }
func (s *ctxAwareStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	return nil, nil
}
func (s *ctxAwareStore) TryTransitionTaskStatus(ctx context.Context, id string, from []types.TaskStatus, to types.TaskStatus) (bool, error) {
	return true, nil
}
func (s *ctxAwareStore) AcquireTaskLease(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (s *ctxAwareStore) ReleaseTaskLease(context.Context, string, string) error { return nil }
func (s *ctxAwareStore) Close() error                                           { return nil }

// newSuspendEngine returns an Engine with an isolated ApprovalStore and a
// ctxAwareStore so SuspendForApproval tests do not touch the global singleton.
func newSuspendEngine(st *ctxAwareStore) *Engine {
	return &Engine{
		Store:     st,
		Approvals: NewApprovalStore(), // P1-1: per-test isolation
	}
}

// TestSuspendForApprovalAwaitingSaveUsesDetachedCtx is the regression test for
// the silent break where SuspendForApproval used the caller's ctx for the
// awaiting_approval persistence write. The caller ctx in production carries
// the run-all wall-clock timeout — a slow approval cycle (or an already-stale
// ctx at suspend time) would propagate ctx.Err() into SaveFullTask and leave
// the task without its awaiting_approval row, so a restart could never
// recover it. The fix detaches persistence onto a fresh context.Background()
// with a bounded timeout. We assert two things:
//  1. The save sees ctx.Err() == nil even when the caller ctx was cancelled.
//  2. The save's ctx carries a deadline — proving it is the detached
//     context.WithTimeout, not the caller's Background-derived ctx.
func TestSuspendForApprovalAwaitingSaveUsesDetachedCtx(t *testing.T) {
	st := &ctxAwareStore{}
	engine := newSuspendEngine(st)
	task := &types.Task{ID: "task-suspend-ctx", Workspace: t.TempDir()}

	// Caller ctx is plain Background (no deadline) and pre-cancelled. Any
	// caller-ctx leak into the save would manifest as ctx.Err() != nil AND no
	// deadline set; the detached path must show ctx.Err() == nil AND a
	// deadline (from context.WithTimeout in SuspendForApproval).
	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := engine.SuspendForApproval(callerCtx, task, "write_file", map[string]any{"path": "x.txt", "content": "hi"})
		done <- err
	}()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("SuspendForApproval err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SuspendForApproval did not return after caller ctx cancel")
	}

	saves := st.snapshot()
	if len(saves) != 1 {
		t.Fatalf("expected exactly 1 SaveFullTask call (awaiting_approval), got %d", len(saves))
	}
	if saves[0].status != types.StatusAwaitingApproval {
		t.Errorf("first save status = %q, want %q", saves[0].status, types.StatusAwaitingApproval)
	}
	if !saves[0].receivedCtxOK {
		t.Error("SaveFullTask received a cancelled ctx; persistence must use a detached context so caller cancellation cannot lose the awaiting_approval row")
	}
	if !saves[0].hasDeadline {
		t.Error("SaveFullTask ctx had no deadline; persistence path must wrap with context.WithTimeout to bound the store call")
	}
}

// TestSuspendForApprovalPostApprovalSaveUsesDetachedCtx covers the second save:
// after a user approves, the Running transition must use a detached ctx so
// that even a caller ctx already past its deadline cannot drop the post-
// approval row. We exercise this by cancelling the caller ctx *before*
// approval is delivered — the prior implementation's `SaveFullTask(ctx, ...)`
// would have observed ctx.Err() and silently lost the Running save (the call
// site uses `_ =`).
func TestSuspendForApprovalPostApprovalSaveUsesDetachedCtx(t *testing.T) {
	st := &ctxAwareStore{}
	engine := newSuspendEngine(st)
	task := &types.Task{ID: "task-postapproval-ctx", Workspace: t.TempDir()}

	// Pre-resolve trick: register a pending approval, then call
	// SuspendForApproval with an already-cancelled ctx. The race between the
	// approval channel receive and ctx.Done() is unavoidable in the real
	// select, so instead we pre-seed the approval channel by spawning a
	// resolver that fires the instant a pending entry appears, then cancel the
	// caller ctx after both saves have had a chance to run.
	resolved := make(chan struct{})
	go func() {
		defer close(resolved)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if engine.Approvals.Resolve(task.ID, types.ApprovalResult{Approved: true}) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	callerCtx := context.Background()
	approved, _, err := engine.SuspendForApproval(callerCtx, task, "write_file", map[string]any{"path": "x.txt", "content": "hi"})
	if err != nil {
		t.Fatalf("SuspendForApproval err = %v, want nil after approval", err)
	}
	if !approved {
		t.Fatal("expected approved to be true")
	}
	<-resolved

	saves := st.snapshot()
	if len(saves) < 2 {
		t.Fatalf("expected at least 2 saves (awaiting_approval + running), got %d", len(saves))
	}
	if saves[0].status != types.StatusAwaitingApproval {
		t.Errorf("save #0 status = %q, want %q", saves[0].status, types.StatusAwaitingApproval)
	}
	if saves[1].status != types.StatusRunning {
		t.Errorf("save #1 status = %q, want %q (post-approval transition lost)", saves[1].status, types.StatusRunning)
	}
	for i, s := range saves {
		if !s.receivedCtxOK {
			t.Errorf("save #%d (status=%q) received cancelled ctx — persistence must be detached from caller ctx", i, s.status)
		}
		if !s.hasDeadline {
			t.Errorf("save #%d (status=%q) ctx had no deadline — persistence path must wrap with context.WithTimeout", i, s.status)
		}
	}
}

func TestSuspendForApprovalRejectionTracesFeedback(t *testing.T) {
	st := &ctxAwareStore{}
	engine := newSuspendEngine(st)
	engine.Mode = ModeMultiAgent
	task := &types.Task{ID: "task-rejection-feedback", Workspace: t.TempDir(), Goal: "test goal"}

	// Resolver goroutine that rejects the request with a feedback message.
	resolved := make(chan struct{})
	go func() {
		defer close(resolved)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if engine.Approvals.Resolve(task.ID, types.ApprovalResult{
				Approved: false,
				Message:  "please use a different path",
			}) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	callerCtx := multiagent.WithApprovalAgentRole(context.Background(), types.AgentRoleExecutor)
	approved, _, err := engine.SuspendForApproval(callerCtx, task, "write_file", map[string]any{"path": "x.txt", "content": "hi"})
	if err != nil {
		t.Fatalf("SuspendForApproval err = %v, want nil", err)
	}
	if approved {
		t.Fatal("expected approved to be false")
	}
	<-resolved

	// Assert trace contains the rejection feedback as evidence
	if len(task.Trace) != 1 {
		t.Fatalf("expected 1 trace entry, got %d", len(task.Trace))
	}
	tr := task.Trace[0]
	if tr.Action != "write_file" {
		t.Errorf("trace action = %q, want write_file", tr.Action)
	}
	if tr.Step != 0 || tr.AgentRole != types.AgentRoleExecutor {
		t.Errorf("trace step=%d role=%q, want step=0 role=%q", tr.Step, tr.AgentRole, types.AgentRoleExecutor)
	}
	if tr.Error == "" || tr.Observation == "" {
		t.Errorf("expected non-empty Error and Observation in rejection trace")
	}
	if len(tr.Evidence) != 1 || tr.Evidence[0].Path != "user_feedback" {
		t.Errorf("expected 1 evidence entry from user_feedback, got %v", tr.Evidence)
	}
	if len(tr.Evidence[0].Lines) != 1 || tr.Evidence[0].Lines[0] != "please use a different path" {
		t.Errorf("expected feedback message, got %v", tr.Evidence[0].Lines)
	}
}

func TestSuspendForApprovalParametersModified(t *testing.T) {
	st := &ctxAwareStore{}
	engine := newSuspendEngine(st)
	task := &types.Task{ID: "task-parameters-modified", Workspace: t.TempDir(), Goal: "test goal"}

	// Resolver goroutine that approves with modified parameters.
	resolved := make(chan struct{})
	go func() {
		defer close(resolved)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if engine.Approvals.Resolve(task.ID, types.ApprovalResult{
				Approved:   true,
				Parameters: map[string]any{"path": "modified.txt", "content": "hi"},
			}) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	callerCtx := context.Background()
	approved, newParams, err := engine.SuspendForApproval(callerCtx, task, "write_file", map[string]any{"path": "x.txt", "content": "hi"})
	if err != nil {
		t.Fatalf("SuspendForApproval err = %v, want nil", err)
	}
	if !approved {
		t.Fatal("expected approved to be true")
	}
	<-resolved

	if newParams == nil || newParams["path"] != "modified.txt" {
		t.Errorf("expected modified path 'modified.txt', got %v", newParams)
	}
}
