package api

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// inMemShutdownStore is a minimal store.Store stub for testing Shutdown rollback.
type inMemShutdownStore struct {
	mu    sync.Mutex
	tasks map[string]*types.Task
}

func newInMemShutdownStore() *inMemShutdownStore {
	return &inMemShutdownStore{tasks: make(map[string]*types.Task)}
}

func (s *inMemShutdownStore) SaveFullTask(_ context.Context, task *types.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *task
	s.tasks[task.ID] = &clone
	return nil
}

func (s *inMemShutdownStore) GetTask(_ context.Context, id string) (*types.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	clone := *t
	return &clone, nil
}

func (s *inMemShutdownStore) ListTasks(_ context.Context, _ store.ListFilter) ([]*types.Task, error) {
	return nil, nil
}
func (s *inMemShutdownStore) ExistsTask(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (s *inMemShutdownStore) SaveMemory(_ context.Context, _ *types.Memory) error { return nil }
func (s *inMemShutdownStore) QueryMemories(_ context.Context, _ string, _ []float32, _ int) ([]*types.Memory, error) {
	return nil, nil
}
func (s *inMemShutdownStore) TryTransitionTaskStatus(_ context.Context, id string, from []types.TaskStatus, to types.TaskStatus) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, nil
	}
	for _, f := range from {
		if t.Status == f {
			t.Status = to
			return true, nil
		}
	}
	return false, nil
}
func (s *inMemShutdownStore) AcquireTaskLease(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	return true, nil
}
func (s *inMemShutdownStore) ReleaseTaskLease(_ context.Context, _, _ string) error { return nil }
func (s *inMemShutdownStore) Close() error                                          { return nil }

// TestShutdownRollback_RunningTaskPaused verifies that Shutdown transitions
// a task that was running (but interrupted by shutdown) to StatusPaused.
func TestShutdownRollback_RunningTaskPaused(t *testing.T) {
	st := newInMemShutdownStore()

	// Pre-populate a running task.
	task := &types.Task{ID: "t1", Status: types.StatusRunning, Goal: "do stuff"}
	_ = st.SaveFullTask(context.Background(), task)

	h := &Handler{
		store:       st,
		activeTasks: make(map[string]*activeRun),
	}

	// Register a fake activeRun for the task.
	_, cancel := context.WithCancel(context.Background())
	h.activeTasks[task.ID] = &activeRun{cancel: cancel}

	// Run Shutdown with a generous deadline.
	ctx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	// Shutdown will cancel the task context, wait for WaitGroup (already 0),
	// then attempt to roll back status to paused.
	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	got, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != types.StatusPaused {
		t.Errorf("expected StatusPaused after shutdown rollback, got %q", got.Status)
	}
}

// TestShutdownRollback_CompletedTaskUntouched verifies that Shutdown does NOT
// modify tasks that were already completed before the shutdown.
func TestShutdownRollback_CompletedTaskUntouched(t *testing.T) {
	st := newInMemShutdownStore()

	task := &types.Task{
		ID:          "t2",
		Status:      types.StatusCompleted,
		Goal:        "already done",
		FinalAnswer: "Here is the full answer with substantial content.",
	}
	_ = st.SaveFullTask(context.Background(), task)

	h := &Handler{
		store:       st,
		activeTasks: make(map[string]*activeRun),
	}
	_, cancel := context.WithCancel(context.Background())
	h.activeTasks[task.ID] = &activeRun{cancel: cancel}

	ctx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = h.Shutdown(ctx)

	got, _ := st.GetTask(context.Background(), task.ID)
	// Completed tasks must never be rolled back to paused.
	if got.Status != types.StatusCompleted {
		t.Errorf("completed task status changed; got %q want %q", got.Status, types.StatusCompleted)
	}
}

// TestCancelTaskByID_LocalHit verifies CancelTaskByID returns true and removes
// the entry from activeTasks when the task is running locally.
func TestCancelTaskByID_LocalHit(t *testing.T) {
	h := &Handler{
		store:       newInMemShutdownStore(),
		activeTasks: make(map[string]*activeRun),
	}
	cancelled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	h.activeTasks["task-local"] = &activeRun{cancel: func() {
		cancel()
		close(cancelled)
	}}

	if !h.CancelTaskByID("task-local") {
		t.Fatal("CancelTaskByID should return true for locally running task")
	}
	select {
	case <-cancelled:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancel function was not called")
	}
	_ = ctx

	// Entry must be removed.
	h.activeTasksMu.Lock()
	_, exists := h.activeTasks["task-local"]
	h.activeTasksMu.Unlock()
	if exists {
		t.Error("activeTasks entry should have been removed after CancelTaskByID")
	}
}

// TestCancelTaskByID_Miss verifies CancelTaskByID returns false for unknown tasks.
func TestCancelTaskByID_Miss(t *testing.T) {
	h := &Handler{
		store:       newInMemShutdownStore(),
		activeTasks: make(map[string]*activeRun),
	}
	if h.CancelTaskByID("nonexistent") {
		t.Error("CancelTaskByID should return false for unknown task")
	}
}

// TestShutdownRollback_RunningTaskSkippedOnTimeout verifies that if shutdown times out,
// a still-running task (still present in activeTasks) is skipped to avoid a race.
func TestShutdownRollback_RunningTaskSkippedOnTimeout(t *testing.T) {
	st := newInMemShutdownStore()

	// Pre-populate a running task.
	task := &types.Task{ID: "t3", Status: types.StatusRunning, Goal: "run forever"}
	_ = st.SaveFullTask(context.Background(), task)

	h := &Handler{
		store:       st,
		activeTasks: make(map[string]*activeRun),
	}

	// Register a fake activeRun for the task.
	_, cancel := context.WithCancel(context.Background())
	h.activeTasks[task.ID] = &activeRun{cancel: cancel}

	// Simulate an already timed-out/canceled context.
	ctx, shutdownCancel := context.WithCancel(context.Background())
	shutdownCancel() // Cancel it immediately to force timeout path

	// Shutdown should return context error, but not roll back the running task.
	err := h.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown should return error on timeout context")
	}

	got, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	// The task status must still be Running, not Paused.
	if got.Status != types.StatusRunning {
		t.Errorf("expected still-running task status to be %q, got %q", types.StatusRunning, got.Status)
	}
}
