package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/api"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type mockPlanner struct {
	blockCh   chan struct{}
	startedCh chan struct{}
}

func (mp *mockPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*planner.PlanDecision, error) {
	if mp.blockCh != nil {
		if mp.startedCh != nil {
			select {
			case mp.startedCh <- struct{}{}:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-mp.blockCh:
		}
	}
	return &planner.PlanDecision{
		Stop:        true,
		FinalAnswer: "Mock final answer",
	}, nil
}

type mockExecutor struct{}

func (m *mockExecutor) Execute(ctx context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
	return nil, nil
}

func setupTestRouter(st store.Store, eng *orchestrator.Engine) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api.RegisterRoutes(r, st, eng, nil)
	return r
}

func waitForTaskStatus(t *testing.T, st store.Store, taskID string, want types.TaskStatus) *types.Task {
	t.Helper()

	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		task, err := st.GetTask(context.Background(), taskID)
		if err != nil {
			t.Fatalf("failed to get task: %v", err)
		}
		if task.Status == want {
			return task
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for task %s status %s; last status %s", taskID, want, task.Status)
		case <-ticker.C:
		}
	}
}

func TestCancelTask_NotFound(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(st, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks/non-existent/cancel", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestCancelTask_NotRunning(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(st, nil)

	task := &types.Task{
		ID:     "task-1",
		Status: types.StatusCompleted,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-1/cancel", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestCancelTask_Orphaned(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(st, nil)

	task := &types.Task{
		ID:     "task-1",
		Status: types.StatusRunning,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-1/cancel", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Verify that the task status was updated to failed in the store
	updatedTask, err := st.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if updatedTask.Status != types.StatusFailed {
		t.Errorf("expected status failed, got %s", updatedTask.Status)
	}
}

func TestCancelTask_Active(t *testing.T) {
	st := store.NewMemoryStore()
	blockCh := make(chan struct{})
	startedCh := make(chan struct{}, 1)
	mp := &mockPlanner{blockCh: blockCh, startedCh: startedCh}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	r := setupTestRouter(st, engine)

	task := &types.Task{
		ID:         "task-active",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	// Start running it asynchronously
	wRun := httptest.NewRecorder()
	reqRun, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-active/run-all", nil)
	r.ServeHTTP(wRun, reqRun)

	if wRun.Code != http.StatusAccepted {
		t.Fatalf("expected run-all status 202, got %d", wRun.Code)
	}

	select {
	case <-startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for planner to start")
	}
	waitForTaskStatus(t, st, "task-active", types.StatusRunning)

	// Cancel it
	wCancel := httptest.NewRecorder()
	reqCancel, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-active/cancel", nil)
	r.ServeHTTP(wCancel, reqCancel)

	if wCancel.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", wCancel.Code)
	}

	// Verify that the task status is failed/canceled in the store
	updatedTask := waitForTaskStatus(t, st, "task-active", types.StatusFailed)
	if updatedTask.Status != types.StatusFailed {
		t.Errorf("expected status failed, got %s", updatedTask.Status)
	}
}

func TestRunAllDuplicateActiveTaskReturnsConflict(t *testing.T) {
	st := store.NewMemoryStore()
	blockCh := make(chan struct{})
	mp := &mockPlanner{blockCh: blockCh}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	r := setupTestRouter(st, engine)

	task := &types.Task{
		ID:         "task-duplicate",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	wFirst := httptest.NewRecorder()
	reqFirst, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-duplicate/run-all", nil)
	r.ServeHTTP(wFirst, reqFirst)
	if wFirst.Code != http.StatusAccepted {
		t.Fatalf("expected first run-all status 202, got %d", wFirst.Code)
	}

	wSecond := httptest.NewRecorder()
	reqSecond, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-duplicate/run-all", nil)
	r.ServeHTTP(wSecond, reqSecond)
	if wSecond.Code != http.StatusConflict {
		t.Fatalf("expected duplicate run-all status 409, got %d", wSecond.Code)
	}

	wCancel := httptest.NewRecorder()
	reqCancel, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-duplicate/cancel", nil)
	r.ServeHTTP(wCancel, reqCancel)
	if wCancel.Code != http.StatusOK {
		t.Fatalf("expected cancel status 200, got %d", wCancel.Code)
	}
}
