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
	blockCh chan struct{}
}

func (mp *mockPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*planner.PlanDecision, error) {
	if mp.blockCh != nil {
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
	mp := &mockPlanner{blockCh: blockCh}
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

	// Give it a tiny moment to start running and hit the block
	time.Sleep(100 * time.Millisecond)

	// Verify it is running
	taskCheck, _ := st.GetTask(context.Background(), "task-active")
	if taskCheck.Status != types.StatusRunning {
		t.Errorf("expected task to be running, got %s", taskCheck.Status)
	}

	// Cancel it
	wCancel := httptest.NewRecorder()
	reqCancel, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-active/cancel", nil)
	r.ServeHTTP(wCancel, reqCancel)

	if wCancel.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", wCancel.Code)
	}

	// Give it some time to process cancellation and persist
	time.Sleep(100 * time.Millisecond)

	// Verify that the task status is failed/canceled in the store
	updatedTask, err := st.GetTask(context.Background(), "task-active")
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if updatedTask.Status != types.StatusFailed {
		t.Errorf("expected status failed, got %s", updatedTask.Status)
	}
}
