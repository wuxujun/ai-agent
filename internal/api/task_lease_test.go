package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type leaseApprovalPlanner struct{}

func (leaseApprovalPlanner) PlanNext(_ context.Context, task *types.Task, _ func(string)) (*planner.PlanDecision, error) {
	if task.StepCount > 0 {
		return &planner.PlanDecision{Stop: true, FinalAnswer: "done"}, nil
	}
	return &planner.PlanDecision{Actions: []planner.ActionCall{{Action: "write_file", Parameters: map[string]any{"path": "fixture.txt", "content": "fixture"}}}}, nil
}

type leaseCountingExecutor struct{ calls atomic.Int32 }

func (e *leaseCountingExecutor) Execute(_ context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
	e.calls.Add(1)
	return []types.StepTrace{{Step: task.StepCount + 1, Action: decision.Actions[0].Action, Observation: "fixture"}}, nil
}

type controlledLeaseStore struct {
	*store.MemoryStore
	renewals   atomic.Int32
	reject     atomic.Bool
	renewError atomic.Bool
}

func (s *controlledLeaseStore) RenewTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	s.renewals.Add(1)
	if s.reject.Load() {
		return false, nil
	}
	if s.renewError.Load() {
		return false, errors.New("fixture renewal unavailable")
	}
	return s.MemoryStore.RenewTaskLease(ctx, id, owner, ttl)
}

func leaseHandler(st store.Store, executor *leaseCountingExecutor) *Handler {
	return &Handler{store: st, engine: &orchestrator.Engine{Store: st, Mode: orchestrator.ModeLegacy, Planner: leaseApprovalPlanner{}, Executor: executor, Approvals: orchestrator.NewApprovalStore(), LLMSceneEnabled: func(string) bool { return false }}, activeTasks: make(map[string]*activeRun), taskSem: newResizableSemaphore()}
}

func runLeaseRequest(h *Handler, id string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/tasks/"+id+"/run-all", nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.runAll(c)
	return w
}

func leaseTestConfig(t *testing.T) {
	t.Helper()
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.Orchestrator.RunAllTimeoutSeconds = 600
		cfg.AnswerPipeline.Enabled = false
		cfg.RAG.ContextMode = "jit"
	}))
}

func TestRunAllRenewsLeaseDuringApprovalAcrossHandlers(t *testing.T) {
	leaseTestConfig(t)
	synctest.Test(t, func(t *testing.T) {
		defer GetBus().ForgetAll()
		st := &controlledLeaseStore{MemoryStore: store.NewMemoryStore()}
		executor := &leaseCountingExecutor{}
		first, peer := leaseHandler(st, executor), leaseHandler(st, executor)
		task := &types.Task{ID: "long-approval", TenantID: "tenant-a", Goal: "fixture", Workspace: "./testdata", Mode: "legacy", Status: types.StatusCreated, MaxSteps: 5, ToolBudget: 5}
		if err := st.SaveFullTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		if w := runLeaseRequest(first, task.ID); w.Code != http.StatusAccepted {
			t.Fatalf("first: %d %s", w.Code, w.Body.String())
		}
		synctest.Wait()
		pending := first.engine.Approvals.List(task.ID)
		if len(pending) != 1 {
			t.Fatal("first handler did not reach approval")
		}
		time.Sleep(640 * time.Second)
		synctest.Wait()
		if w := runLeaseRequest(peer, task.ID); w.Code != http.StatusConflict {
			first.CancelTaskByID(task.ID)
			peer.CancelTaskByID(task.ID)
			first.Wait()
			peer.Wait()
			t.Fatalf("peer: %d %s; expected live owner to retain lease", w.Code, w.Body.String())
		}
		if st.renewals.Load() == 0 {
			t.Fatal("lease was never renewed")
		}
		if !first.engine.Approvals.ResolveByID(pending[0].ID, types.ApprovalResult{Approved: true}) {
			t.Fatal("approval resolution failed")
		}
		first.Wait()
		stored, err := st.GetTask(context.Background(), task.ID)
		if err != nil || stored.Status != types.StatusCompleted || executor.calls.Load() != 1 {
			t.Fatalf("stored=%+v calls=%d err=%v", stored, executor.calls.Load(), err)
		}
		if ok, err := st.AcquireTaskLease(context.Background(), task.ID, "after-completion", time.Minute); err != nil || !ok {
			t.Fatalf("lease not released: %v %v", ok, err)
		}
	})
}

func TestRunAllLeaseLossStopsApprovalAndPreservesPeerResult(t *testing.T) {
	leaseTestConfig(t)
	for _, failWithError := range []bool{false, true} {
		name := "rejected"
		if failWithError {
			name = "storage_error"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				defer GetBus().ForgetAll()
				st := &controlledLeaseStore{MemoryStore: store.NewMemoryStore()}
				executor := &leaseCountingExecutor{}
				first := leaseHandler(st, executor)
				task := &types.Task{ID: "lost-approval", TenantID: "tenant-a", Goal: "fixture", Workspace: "./testdata", Mode: "legacy", Status: types.StatusCreated, MaxSteps: 5, ToolBudget: 5}
				if err := st.SaveFullTask(context.Background(), task); err != nil {
					t.Fatal(err)
				}
				ch, _ := GetBus().Subscribe(task.ID)
				defer GetBus().Unsubscribe(task.ID, ch)
				defer GetBus().Forget(task.ID)
				if w := runLeaseRequest(first, task.ID); w.Code != http.StatusAccepted {
					t.Fatalf("first: %d", w.Code)
				}
				synctest.Wait()
				first.activeTasksMu.Lock()
				run := first.activeTasks[task.ID]
				first.activeTasksMu.Unlock()
				if run == nil {
					t.Fatal("first run missing")
				}
				pending := first.engine.Approvals.List(task.ID)
				if len(pending) != 1 {
					t.Fatal("approval missing")
				}
				if failWithError {
					st.renewError.Store(true)
				} else {
					st.reject.Store(true)
				}
				// Model a peer taking over after the original storage lease was lost.
				if err := st.ReleaseTaskLease(context.Background(), task.ID, run.owner); err != nil {
					t.Fatal(err)
				}
				if ok, err := st.AcquireTaskLease(context.Background(), task.ID, "peer-owner", time.Hour); err != nil || !ok {
					t.Fatalf("peer lease: %v %v", ok, err)
				}
				fresh := types.CloneTask(task)
				fresh.Status = types.StatusRunning
				fresh.Goal = "peer-result"
				if err := st.SaveFullTask(context.Background(), fresh); err != nil {
					t.Fatal(err)
				}
				time.Sleep(220 * time.Second)
				synctest.Wait()
				// The stale approval must be unable to reanimate an abandoned execution.
				first.engine.Approvals.ResolveByID(pending[0].ID, types.ApprovalResult{Approved: true})
				first.Wait()
				stored, err := st.GetTask(context.Background(), task.ID)
				if err != nil || stored.Goal != "peer-result" || stored.Status != types.StatusRunning || executor.calls.Load() != 0 {
					t.Fatalf("stale execution changed peer: task=%+v calls=%d err=%v", stored, executor.calls.Load(), err)
				}
				select {
				case event := <-ch:
					t.Fatalf("stale terminal event=%+v", event)
				default:
				}
				if ok, err := st.AcquireTaskLease(context.Background(), task.ID, "intruder", time.Minute); err != nil || ok {
					t.Fatalf("stale cleanup released peer lease: %v %v", ok, err)
				}
			})
		})
	}
}

func TestShutdownRollbackPreservesPeerOwner(t *testing.T) {
	st := store.NewMemoryStore()
	task := &types.Task{ID: "shutdown-peer", Status: types.StatusRunning, Goal: "peer result"}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.AcquireTaskLease(t.Context(), task.ID, "peer-owner", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	h := &Handler{store: st, activeTasks: map[string]*activeRun{task.ID: {cancel: func() {}, owner: "old-owner"}}}
	if err := h.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(t.Context(), task.ID)
	if err != nil || got.Status != types.StatusRunning {
		t.Fatalf("peer result changed: %+v, %v", got, err)
	}
}

type leaseRenewFunc func(context.Context, string, string, time.Duration) (bool, error)

func (f leaseRenewFunc) RenewTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	return f(ctx, id, owner, ttl)
}

func TestExecutionLeaseDoesNotReviveAfterLateRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		defer GetBus().ForgetAll()
		var canceled atomic.Bool
		lease := store.NewExecutionLease(func() { canceled.Store(true) })
		renew := leaseRenewFunc(func(context.Context, string, string, time.Duration) (bool, error) {
			time.Sleep(10 * time.Second) // Simulate a backend ignoring its request deadline.
			return true, nil
		})
		lease.Start(t.Context(), renew, "task", "owner", 9*time.Second, time.Now())
		defer lease.Finish()
		time.Sleep(10 * time.Second)
		if !canceled.Load() {
			t.Fatal("local expiry did not cancel without polling Err")
		}
		if !errors.Is(lease.Err(), store.ErrTaskLeaseLost) {
			t.Fatal("expired local ownership did not cancel execution")
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if !errors.Is(lease.Err(), store.ErrTaskLeaseLost) {
			t.Fatal("late response revived a lost lease")
		}
	})
}

func TestExecutionLeaseRenewsUntilFlushFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		defer GetBus().ForgetAll()
		ctx, cancel := context.WithCancel(t.Context())
		var renewals atomic.Int32
		lease := store.NewExecutionLease(func() { t.Error("lease unexpectedly lost") })
		renew := leaseRenewFunc(func(context.Context, string, string, time.Duration) (bool, error) { renewals.Add(1); return true, nil })
		lease.Start(ctx, renew, "task", "owner", 3*time.Second, time.Now())
		cancel()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if lease.Err() != nil || renewals.Load() < 3 {
			t.Fatal("execution cancellation stopped renewal before flush completed")
		}
		lease.Finish()
		count := renewals.Load()
		time.Sleep(4 * time.Second)
		if renewals.Load() != count {
			t.Fatal("renewal continued after cleanup")
		}
	})
}

type checkpointBeforeAcquireStore struct {
	*store.MemoryStore
	changed atomic.Bool
}

func (s *checkpointBeforeAcquireStore) AcquireTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	if s.changed.CompareAndSwap(false, true) {
		fresh, err := s.GetTask(ctx, id)
		if err != nil {
			return false, err
		}
		fresh.Goal = "latest checkpoint"
		fresh.Status = types.StatusPaused
		fresh.StepCount = 1
		fresh.Trace = []types.StepTrace{{Step: 1, Action: "reason", Observation: "peer checkpoint"}}
		if err := s.SaveFullTask(ctx, fresh); err != nil {
			return false, err
		}
	}
	return s.MemoryStore.AcquireTaskLease(ctx, id, owner, ttl)
}
func TestRunAllReloadsCheckpointAfterAcquiringLease(t *testing.T) {
	leaseTestConfig(t)
	synctest.Test(t, func(t *testing.T) {
		defer GetBus().ForgetAll()
		st := &checkpointBeforeAcquireStore{MemoryStore: store.NewMemoryStore()}
		executor := &leaseCountingExecutor{}
		h := leaseHandler(st, executor)
		task := &types.Task{ID: "admission-checkpoint", TenantID: "tenant-a", Goal: "old checkpoint", Mode: "legacy", Status: types.StatusCreated, MaxSteps: 5, ToolBudget: 5}
		if err := st.SaveFullTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
		w := runLeaseRequest(h, task.ID)
		synctest.Wait()
		h.CancelTaskByID(task.ID)
		h.Wait()
		got, err := st.GetTask(t.Context(), task.ID)
		if w.Code != http.StatusAccepted || err != nil || got.Goal != "latest checkpoint" || got.Status != types.StatusCompleted || got.StepCount != 1 {
			t.Fatalf("stale snapshot executed: http=%d task=%+v err=%v", w.Code, got, err)
		}
	})
}

type delayedApprovalSaveStore struct {
	*store.MemoryStore
	entered, resume chan struct{}
	blocked         atomic.Bool
}

func (s *delayedApprovalSaveStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	if task.Status == types.StatusAwaitingApproval && s.blocked.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.resume
	}
	return s.MemoryStore.SaveFullTask(ctx, task)
}
func TestStreamStepHoldsLeaseThroughDetachedApprovalSave(t *testing.T) {
	leaseTestConfig(t)
	synctest.Test(t, func(t *testing.T) {
		defer GetBus().ForgetAll()
		st := &delayedApprovalSaveStore{MemoryStore: store.NewMemoryStore(), entered: make(chan struct{}), resume: make(chan struct{})}
		h := leaseHandler(st, &leaseCountingExecutor{})
		task := &types.Task{ID: "step-disconnect", TenantID: "tenant-a", Goal: "old result", Mode: "legacy", Status: types.StatusCreated, MaxSteps: 5, ToolBudget: 5}
		if err := st.SaveFullTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
		requestCtx, cancel := context.WithCancel(t.Context())
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/run?stream=true", nil).WithContext(requestCtx)
		c.Params = gin.Params{{Key: "id", Value: task.ID}}
		done := make(chan struct{})
		go func() { h.runTaskStep(c); close(done) }()
		<-st.entered
		h.activeTasksMu.Lock()
		run := h.activeTasks[task.ID]
		h.activeTasksMu.Unlock()
		cancel()
		<-done
		if ok, err := st.AcquireTaskLease(t.Context(), task.ID, "peer", time.Hour); err != nil || ok {
			t.Errorf("lease released before old execution exited: %v %v", ok, err)
		}
		// Simulate independent storage lease loss while the detached write is stalled.
		if err := st.ReleaseTaskLease(t.Context(), task.ID, run.owner); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.AcquireTaskLease(t.Context(), task.ID, "peer", time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
		fresh := types.CloneTask(task)
		fresh.Goal = "peer result"
		fresh.Status = types.StatusRunning
		if err := st.MemoryStore.SaveFullTask(t.Context(), fresh); err != nil {
			t.Fatal(err)
		}
		close(st.resume)
		synctest.Wait()
		h.Wait()
		got, err := st.GetTask(t.Context(), task.ID)
		if err != nil || got.Goal != "peer result" || got.Status != types.StatusRunning {
			t.Fatalf("old detached save replaced peer: %+v %v", got, err)
		}
	})
}
