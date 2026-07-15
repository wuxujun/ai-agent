package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/api"
	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type mockPlanner struct {
	blockCh   chan struct{}
	startedCh chan struct{}
	runtimeCh chan *llmcore.Runtime
	tokens    []string
}

func (mp *mockPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*planner.PlanDecision, error) {
	if mp.runtimeCh != nil {
		select {
		case mp.runtimeCh <- llmcore.RuntimeFromContext(ctx):
		default:
		}
	}
	for _, tok := range mp.tokens {
		onChunk(tok)
	}
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

func setupTestRouter(t *testing.T, st store.Store, eng *orchestrator.Engine) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if config.Get().API.APIKey != "" && config.Get().API.APIKey != "my-test-secret-api-key" {
		t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
			cfg.API.APIKey = ""
		}))
	}
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
	r := setupTestRouter(t, st, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks/non-existent/cancel", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestReadyReportsConfiguredProbeMode(t *testing.T) {
	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.LLM.Provider = "openai"
		cfg.LLM.Model = "gpt-test"
		cfg.LLM.ReadinessMode = config.LLMReadinessConfigOnly
	})
	t.Cleanup(restore)
	r := setupTestRouter(t, store.NewMemoryStore(), nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/ready", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var response struct {
		Ready         bool   `json:"ready"`
		Verified      bool   `json:"llm_verified"`
		ReadinessMode string `json:"llm_readiness_mode"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if !response.Ready || response.Verified || response.ReadinessMode != config.LLMReadinessConfigOnly {
		t.Fatalf("readiness response = %+v", response)
	}
}

func TestCancelTask_NotRunning(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

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
	r := setupTestRouter(t, st, nil)

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
	r := setupTestRouter(t, st, engine)

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

// TestCreateTaskPersistsTokenBudget verifies that token_budget in the POST body
// flows through CreateTaskRequest into the persisted Task. This closes the
// silent break where the field was on the type but unreachable via the API,
// making the token budget gate a dead branch.
func TestCreateTaskPersistsTokenBudget(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	body := `{"id":"task-tb","goal":"x","workspace":"./testdata","max_steps":3,"tool_budget":3,"token_budget":2500}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var created types.Task
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.TokenBudget != 2500 {
		t.Errorf("response TokenBudget = %d, want 2500", created.TokenBudget)
	}

	got, err := st.GetTask(context.Background(), "task-tb")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.TokenBudget != 2500 {
		t.Errorf("persisted TokenBudget = %d, want 2500", got.TokenBudget)
	}
}

func TestCreateTaskPersistsLLMBudgets(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = nil
		cfg.LLM.Gateway.InputCostPerMillionUSD = apiTestPtr(1.0)
	}))
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	body := `{"id":"task-llm-budget","goal":"x","workspace":"./testdata","llm_call_budget":4,"llm_cost_budget_usd":1.5}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}
	got, err := st.GetTask(context.Background(), "task-llm-budget")
	if err != nil {
		t.Fatal(err)
	}
	if got.LLMCallBudget != 4 || got.LLMCostBudgetUSD != 1.5 {
		t.Fatalf("persisted LLM budgets = %+v", got)
	}
}

func TestCreateTaskRejectsNegativeLLMBudget(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"goal":"x","workspace":"./testdata","llm_call_budget":-1}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func apiTestPtr[T any](value T) *T { return &value }

func TestCreateTask_AutoGenerateID(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	body := `{"goal":"x","workspace":"./testdata","max_steps":3,"tool_budget":3}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var created types.Task
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.ID == "" {
		t.Errorf("expected generated ID in response, got empty")
	}

	got, err := st.GetTask(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("persisted ID = %q, want %q", got.ID, created.ID)
	}
}

// TestRunAllAlreadyRunningInDBDoesNotLeakReservation covers the cleanup path
// that #7 hardened: when the DB TryTransitionTaskStatus rejects (because the
// task is already Running in the persisted store — possibly from a peer
// process), the handler now reserves the in-process activeTasks slot BEFORE
// the DB call and must release it on rejection. If the cleanup leaked, a
// subsequent runAll on the same task ID would return 409 forever, even after
// the DB row is corrected.
func TestRunAllAlreadyRunningInDBDoesNotLeakReservation(t *testing.T) {
	st := store.NewMemoryStore()
	mp := &mockPlanner{}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	r := setupTestRouter(t, st, engine)

	// Simulate an unsupported persisted state. The status is not in the allowed
	// transition set, so the
	// TryTransitionTaskStatus inside runAll will return false.
	task := &types.Task{
		ID:         "task-stale-running",
		Status:     types.TaskStatus("invalid"),
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-stale-running/run-all", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected first run-all to 409 on DB rejection, got %d: %s", w.Code, w.Body.String())
	}

	// Now flip the DB row back to a startable state. If the previous run-all
	// leaked an activeTasks entry, this second call would still see the
	// reservation and 409. The fix uses compare-and-delete cleanup so the slot
	// is released.
	task.Status = types.StatusCreated
	_ = st.SaveFullTask(context.Background(), task)

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-stale-running/run-all", nil)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusAccepted {
		t.Fatalf("expected second run-all to 202 after DB row corrected, got %d: %s", w2.Code, w2.Body.String())
	}

	// Drain the running task so the test doesn't leak a goroutine that
	// outlives the test binary.
	wCancel := httptest.NewRecorder()
	reqCancel, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-stale-running/cancel", nil)
	r.ServeHTTP(wCancel, reqCancel)
	if wCancel.Code != http.StatusOK {
		t.Errorf("cleanup cancel returned %d", wCancel.Code)
	}
}

func TestRunLeaseBlocksPeerRunAndRunAll(t *testing.T) {
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
	// Separate routers create separate Handler.activeTasks maps, simulating two
	// service instances that coordinate only through the shared Store lease.
	r1 := setupTestRouter(t, st, engine)
	r2 := setupTestRouter(t, st, engine)

	task := &types.Task{
		ID:         "task-peer-lease",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatalf("SaveFullTask: %v", err)
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-peer-lease/run", nil)
		r1.ServeHTTP(w, req)
		firstDone <- w
	}()

	select {
	case <-startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first /run to acquire lease")
	}

	wRun := httptest.NewRecorder()
	reqRun, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-peer-lease/run", nil)
	r2.ServeHTTP(wRun, reqRun)
	if wRun.Code != http.StatusConflict {
		t.Fatalf("peer /run status = %d, want 409: %s", wRun.Code, wRun.Body.String())
	}

	wRunAll := httptest.NewRecorder()
	reqRunAll, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-peer-lease/run-all", nil)
	r2.ServeHTTP(wRunAll, reqRunAll)
	if wRunAll.Code != http.StatusConflict {
		t.Fatalf("peer /run-all status = %d, want 409: %s", wRunAll.Code, wRunAll.Body.String())
	}

	close(blockCh)
	select {
	case w := <-firstDone:
		if w.Code != http.StatusOK {
			t.Fatalf("first /run status = %d, want 200: %s", w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first /run did not finish after planner was released")
	}
}

// TestRunAllTimeoutConfigHasDefault is a smoke test that the new
// orchestrator.run_all_timeout_seconds knob is registered with a sane default,
// so existing deployments running without this key still get the original
// 10-minute budget rather than a zero-timeout context that cancels instantly.
func TestRunAllTimeoutConfigHasDefault(t *testing.T) {
	cfg := config.Get()
	if cfg.Orchestrator.RunAllTimeoutSeconds <= 0 {
		t.Fatalf("RunAllTimeoutSeconds default = %d, want positive (>= 600)", cfg.Orchestrator.RunAllTimeoutSeconds)
	}
}

func TestRunAllPreservesLLMRuntimeWithoutRequestCancellation(t *testing.T) {
	st := store.NewMemoryStore()
	blockCh := make(chan struct{})
	runtimeCh := make(chan *llmcore.Runtime, 1)
	mp := &mockPlanner{blockCh: blockCh, runtimeCh: runtimeCh}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}

	gin.SetMode(gin.TestMode)
	runtime := llmcore.NewDefaultRuntime(nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(llmcore.WithRuntime(c.Request.Context(), runtime))
		c.Next()
	})
	handler := api.RegisterRoutes(r, st, engine, nil)

	task := &types.Task{ID: "task-runtime-context", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatalf("SaveFullTask: %v", err)
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(requestCtx, http.MethodPost, "/api/tasks/task-runtime-context/run-all", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("run-all status = %d, want 202: %s", w.Code, w.Body.String())
	}

	select {
	case got := <-runtimeCh:
		if got != runtime {
			t.Fatalf("background runtime = %p, want injected runtime %p", got, runtime)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background planner")
	}

	// The accepted task owns its lifecycle; disconnecting the HTTP caller must
	// not cancel it, but the request's Runtime value must remain available.
	cancelRequest()
	close(blockCh)
	handler.Wait()
	want := waitForTaskStatus(t, st, task.ID, types.StatusCompleted)
	if want.FinalAnswer != "Mock final answer" {
		t.Fatalf("FinalAnswer = %q, want mock answer", want.FinalAnswer)
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
	r := setupTestRouter(t, st, engine)

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

// TestApproveSinglePendingResolvesImplicitly covers the common path: exactly
// one pending approval for a task → POST /approve with no body succeeds and
// returns the approval payload.
func TestApproveSinglePendingResolvesImplicitly(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	id, ch := orchestrator.RegisterApproval("task-approve-single", &types.ApprovalRequest{
		TaskID: "task-approve-single",
		Action: "write_file",
	})
	t.Cleanup(func() { orchestrator.RemoveApproval(id) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-approve-single/approve", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case got := <-ch:
		if got.Approved != true {
			t.Errorf("approval channel received %v, want true", got.Approved)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("approval channel not signaled after /approve")
	}

	var body struct {
		Message  string                 `json:"message"`
		Approval *types.ApprovalRequest `json:"approval"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Approval == nil || body.Approval.ID != id {
		t.Errorf("response approval = %+v, want ID %q", body.Approval, id)
	}
}

// TestApproveMultiPendingReturnsConflict guards the disambiguation contract:
// when >1 approvals are pending for the same task, an implicit /approve must
// surface 409 with the pending IDs so the client can pick one.
func TestApproveMultiPendingReturnsConflict(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	taskID := "task-approve-multi"
	id1, _ := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "first"})
	id2, _ := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "second"})
	t.Cleanup(func() {
		orchestrator.RemoveApproval(id1)
		orchestrator.RemoveApproval(id2)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/approve", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on ambiguous pending, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Error        string   `json:"error"`
		PendingCount int      `json:"pending_count"`
		ApprovalIDs  []string `json:"approval_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.PendingCount != 2 {
		t.Errorf("pending_count = %d, want 2", body.PendingCount)
	}
	gotIDs := map[string]bool{}
	for _, id := range body.ApprovalIDs {
		gotIDs[id] = true
	}
	if !gotIDs[id1] || !gotIDs[id2] {
		t.Errorf("approval_ids = %v, want both %q and %q", body.ApprovalIDs, id1, id2)
	}

	// Both must still be pending — the conflict response must NOT have resolved either.
	if got := orchestrator.PendingApprovalCount(taskID); got != 2 {
		t.Errorf("pending count after 409 = %d, want 2 (neither should have been resolved)", got)
	}
}

// TestRejectByApprovalIDResolvesSpecificEntry covers the explicit-ID path: the
// client picks one of the IDs the 409 surfaced and rejects it; only that
// entry's channel must receive false, and the sibling must stay pending.
func TestRejectByApprovalIDResolvesSpecificEntry(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	taskID := "task-reject-explicit"
	id1, ch1 := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "first"})
	id2, ch2 := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "second"})
	t.Cleanup(func() {
		orchestrator.RemoveApproval(id1)
		orchestrator.RemoveApproval(id2)
	})

	body := `{"approval_id":"` + id2 + `"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/reject", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case got := <-ch2:
		if got.Approved != false {
			t.Errorf("ch2 received %v, want false", got.Approved)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ch2 not signaled after /reject")
	}

	// ch1 must still be blocking — only the targeted entry was resolved.
	select {
	case got := <-ch1:
		t.Fatalf("ch1 unexpectedly received %v; only id2 was rejected", got)
	case <-time.After(50 * time.Millisecond):
		// Expected: id1 still pending.
	}

	if got := orchestrator.PendingApprovalCount(taskID); got != 1 {
		t.Errorf("after rejecting id2, pending count = %d, want 1", got)
	}
}

// TestApproveByApprovalIDNotFoundReturns404 guards the negative path:
// an unknown approval_id must 404 rather than resolving the unrelated single
// pending entry that happens to share the task.
func TestApproveByApprovalIDNotFoundReturns404(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	taskID := "task-approve-unknown-id"
	id, ch := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "real"})
	t.Cleanup(func() { orchestrator.RemoveApproval(id) })

	body := `{"approval_id":"definitely-not-a-real-id"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/approve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown approval_id, got %d: %s", w.Code, w.Body.String())
	}

	// The real pending entry must not have been resolved as collateral damage.
	select {
	case got := <-ch:
		t.Fatalf("real pending was unexpectedly resolved with %v after a 404 on a different ID", got)
	case <-time.After(50 * time.Millisecond):
	}

	if got := orchestrator.PendingApprovalCount(taskID); got != 1 {
		t.Errorf("pending count after 404 = %d, want 1 (real entry untouched)", got)
	}
}

func TestAuthMiddleware(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = "my-test-secret-api-key"
	}))

	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	// Test 1: No Key provided -> 401
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when key is missing, got %d", w1.Code)
	}

	// Test 2: Invalid Key provided -> 401
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req2.Header.Set("X-API-Key", "wrong-key")
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when key is wrong, got %d", w2.Code)
	}

	// Test 3: Correct X-API-Key -> 200
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req3.Header.Set("X-API-Key", "my-test-secret-api-key")
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("expected 200 with correct X-API-Key, got %d: %s", w3.Code, w3.Body.String())
	}

	// Test 4: Correct Authorization Bearer -> 200
	w4 := httptest.NewRecorder()
	req4, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req4.Header.Set("Authorization", "Bearer my-test-secret-api-key")
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Errorf("expected 200 with correct Authorization Bearer, got %d: %s", w4.Code, w4.Body.String())
	}
}

func TestTenantAuthenticationAndTaskIsolation(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = "my-test-secret-api-key"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "tenant-a-test-key", DailyLLMCallBudget: 2},
			"tenant-b": {APIKey: "tenant-b-test-key"},
		}
	}))
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	create := func(key, id string) {
		t.Helper()
		w := httptest.NewRecorder()
		body := `{"id":"` + id + `","goal":"x","workspace":"./testdata"}`
		req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", key)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", id, w.Code, w.Body.String())
		}
	}
	create("tenant-a-test-key", "tenant-a-task")
	create("tenant-b-test-key", "tenant-b-task")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/tasks/tenant-b-task", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	var list struct {
		Tasks []types.Task `json:"tasks"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Tasks) != 1 || list.Tasks[0].TenantID != "tenant-a" {
		t.Fatalf("tenant list = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("tenant metrics access = %d", w.Code)
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/usage", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tenant_id":"tenant-a"`) || !strings.Contains(w.Body.String(), `"llm_calls":2`) {
		t.Fatalf("tenant usage = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("X-API-Key", "my-test-secret-api-key")
	r.ServeHTTP(w, req)
	list.Tasks = nil
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Tasks) != 2 {
		t.Fatalf("admin list = %d %s", w.Code, w.Body.String())
	}
}

func TestAuthMiddleware_FailClosed(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
	}))

	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	// Temporarily switch mode to DebugMode so the test-bypass is not triggered.
	originalMode := gin.Mode()
	gin.SetMode(gin.DebugMode)
	defer gin.SetMode(originalMode)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable when API key is empty/unset, got %d", w.Code)
	}
}

func TestRunTaskStep_Streaming(t *testing.T) {
	st := store.NewMemoryStore()
	mp := &mockPlanner{
		tokens: []string{"token1", "token2"},
	}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	engine.TokenCallback = func(taskID string, chunk string) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: types.StatusRunning,
			Token:  chunk,
		})
	}
	r := setupTestRouter(t, st, engine)

	task := &types.Task{
		ID:         "task-stream-run",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-stream-run/run?stream=true", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if contentType := w.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %q", contentType)
	}

	body := w.Body.String()
	if !strings.Contains(body, "token1") || !strings.Contains(body, "token2") {
		t.Errorf("expected body to contain streamed tokens, got %q", body)
	}
}

func TestRunAll_Streaming(t *testing.T) {
	st := store.NewMemoryStore()
	mp := &mockPlanner{
		tokens: []string{"token-all"},
	}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	engine.TokenCallback = func(taskID string, chunk string) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: types.StatusRunning,
			Token:  chunk,
		})
	}
	engine.StepCallback = func(taskID string, status types.TaskStatus, step *types.StepTrace) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: status,
			Step:   step,
		})
	}
	r := setupTestRouter(t, st, engine)

	task := &types.Task{
		ID:         "task-stream-runall",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-stream-runall/run-all?stream=true", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if contentType := w.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %q", contentType)
	}

	body := w.Body.String()
	if !strings.Contains(body, "token-all") {
		t.Errorf("expected body to contain streamed events, got %q", body)
	}
	if !strings.Contains(body, `"final_answer":"Mock final answer"`) {
		t.Errorf("expected terminal event to contain final answer, got %q", body)
	}
}
