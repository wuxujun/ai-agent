package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type recoverySQLStore interface {
	store.Store
	store.DurableApprovalStore
	store.TaskLeaseStore
	store.TaskDeletionStore
}

type recoverySQLOpener func() (recoverySQLStore, error)

func TestDurableApprovalRecoverySQLiteContract(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "recovery.db")
	runDurableApprovalRecoverySQLContract(t, func() (recoverySQLStore, error) {
		return store.NewSQLiteStore(dsn)
	})
}

// Explicit integration test: only dedicated test services may be supplied.
func TestExternalPostgresDurableApprovalRecoveryContract(t *testing.T) {
	if os.Getenv("AI_AGENT_RUN_EXTERNAL_INTEGRATION") != "true" {
		t.Skip("set AI_AGENT_RUN_EXTERNAL_INTEGRATION=true to use dedicated external services")
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("TEST_POSTGRES_DSN is required when external integration is enabled")
	}
	runDurableApprovalRecoverySQLContract(t, func() (recoverySQLStore, error) {
		return store.NewPostgresStore(dsn)
	})
}

func runDurableApprovalRecoverySQLContract(t *testing.T, open recoverySQLOpener) {
	t.Helper()
	for _, tc := range []struct {
		name       string
		parameters map[string]any
		want       map[string]any
	}{
		{"replacement", map[string]any{"path": "approved.txt"}, map[string]any{"path": "approved.txt"}},
		{"unchanged", nil, map[string]any{"path": "original.txt", "content": "original"}},
		{"empty_replacement", map[string]any{}, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, peer, codec, taskID, approvalID := seedRecoverySQLCheckpoint(t, open, tc.parameters)
			var calls atomic.Int32
			entered := make(chan planner.ActionCall, 2)
			resume := make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			executor := recoverySQLExecutor(func(ctx context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
				calls.Add(1)
				// A second connection must observe consumption BEFORE the side effect.
				approval, err := peer.GetApproval(ctx, approvalID, "tenant-recovery")
				if err != nil || approval.Status != types.ApprovalConsumed {
					t.Errorf("approval at execution = %+v, %v; want consumed", approval, err)
				}
				if len(decision.Actions) != 1 {
					return nil, errors.New("recovery must execute exactly one action")
				}
				entered <- decision.Actions[0]
				select {
				case <-resume:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return []types.StepTrace{recoverySQLResultTrace()}, nil
			})
			var events atomic.Int32
			engine := &orchestrator.Engine{Store: first, ApprovalCodec: codec, Executor: executor}
			engine.EventCallback = func(id string, status types.TaskStatus) {
				events.Add(1)
				if id != taskID || status != types.StatusPaused {
					t.Errorf("recovery event = %q/%s", id, status)
				}
				// Publication must happen after the SQL transaction is visible to readers.
				stored, err := peer.GetTask(context.Background(), id)
				if err != nil || stored.Status != types.StatusPaused || len(stored.Trace) != 3 {
					t.Errorf("task visible at recovery event = %+v, %v", stored, err)
				}
			}
			handler := &Handler{store: first, engine: engine}
			handler.startDurableApprovalRecovery(taskID, approvalID)
			done := make(chan struct{})
			go func() { handler.Wait(); close(done) }()
			t.Cleanup(func() { release(); awaitRecoverySQLSignal(t, done) })
			select {
			case action := <-entered:
				if action.Action != "write_file" || !reflect.DeepEqual(action.Parameters, tc.want) {
					t.Fatalf("executed action = %+v, want write_file with %#v", action, tc.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("recovery did not enter the executor")
			}

			peerTask, peerApproval := loadRecoverySQLCheckpoint(t, peer, taskID, approvalID)
			// Supply the pre-consumption snapshot to exercise the real task lease,
			// not the cheap early return for an already-consumed approval.
			peerApproval.Status = types.ApprovalApproved
			peerEngine := &orchestrator.Engine{Store: peer, ApprovalCodec: codec, Executor: executor}
			if recovered, err := peerEngine.RecoverApprovedApproval(t.Context(), peerTask, peerApproval, "competing-owner"); recovered || !errors.Is(err, store.ErrTaskLeaseBusy) {
				t.Fatalf("competing recovery = %v, %v; want lease busy", recovered, err)
			}
			release()
			awaitRecoverySQLSignal(t, done)

			stored, approval := loadRecoverySQLCheckpoint(t, peer, taskID, approvalID)
			wantTrace := append(recoverySQLInitialTrace(), recoverySQLResultTrace())
			if stored.Status != types.StatusPaused || stored.StepCount != 2 || !reflect.DeepEqual(stored.Trace, wantTrace) {
				t.Fatalf("recovered task = %+v; want paused, 2 execution steps and all 3 trace events", stored)
			}
			if approval.Status != types.ApprovalConsumed || approval.Version != 3 || calls.Load() != 1 || events.Load() != 1 {
				t.Fatalf("approval=%+v calls=%d events=%d", approval, calls.Load(), events.Load())
			}
			// A stale recovery snapshot must not execute again after the lease releases.
			if recovered, err := peerEngine.RecoverApprovedApproval(t.Context(), peerTask, peerApproval, "replay-owner"); recovered || err != nil || calls.Load() != 1 {
				t.Fatalf("replayed recovery = %v, %v; calls=%d", recovered, err, calls.Load())
			}
			assertRecoverySQLHTTPRead(t, &Handler{store: peer}, taskID, wantTrace)
		})
	}

	t.Run("consumed_checkpoint_after_restart", func(t *testing.T) {
		first, peer, codec, taskID, approvalID := seedRecoverySQLCheckpoint(t, open, nil)
		staleTask, staleApproval := loadRecoverySQLCheckpoint(t, first, taskID, approvalID)
		// Model a crash after consumption but before the recovered task is saved.
		// The persisted task is still awaiting approval, so task status cannot
		// accidentally hide a regression in the durable consumption boundary.
		if ok, err := first.TransitionApproval(t.Context(), approvalID, staleTask.TenantID, staleApproval.Version, types.ApprovalApproved, types.ApprovalConsumed, staleApproval.ResolutionPayload); err != nil || !ok {
			t.Fatalf("consume before restart = %v, %v", ok, err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		if err := peer.Close(); err != nil {
			t.Fatal(err)
		}
		restarted, err := open()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = restarted.Close() })
		before, consumed := loadRecoverySQLCheckpoint(t, restarted, taskID, approvalID)
		if before.Status != types.StatusAwaitingApproval || consumed.Status != types.ApprovalConsumed || consumed.Version != 3 {
			t.Fatalf("crash checkpoint = %+v, %+v", before, consumed)
		}
		var calls, events atomic.Int32
		engine := &orchestrator.Engine{Store: restarted, ApprovalCodec: codec,
			Executor: recoverySQLExecutor(func(context.Context, *types.Task, *planner.PlanDecision) ([]types.StepTrace, error) {
				calls.Add(1)
				return []types.StepTrace{recoverySQLResultTrace()}, nil
			}),
			EventCallback: func(string, types.TaskStatus) { events.Add(1) },
		}
		if recovered, err := engine.RecoverApprovedApproval(t.Context(), staleTask, staleApproval, "restart-owner"); recovered || err != nil {
			t.Fatalf("consumed checkpoint replay = %v, %v", recovered, err)
		}
		after, approval := loadRecoverySQLCheckpoint(t, restarted, taskID, approvalID)
		if calls.Load() != 0 || events.Load() != 0 || !reflect.DeepEqual(after, before) || approval.Status != types.ApprovalConsumed || approval.Version != 3 {
			t.Fatalf("consumed checkpoint replay changed durable state: calls=%d events=%d task=%+v approval=%+v", calls.Load(), events.Load(), after, approval)
		}
	})

	t.Run("lost_lease_before_commit", func(t *testing.T) {
		first, peer, codec, taskID, approvalID := seedRecoverySQLCheckpoint(t, open, nil)
		blocked := &recoverySQLCommitBarrier{recoverySQLStore: first, entered: make(chan struct{}), resume: make(chan struct{})}
		release := sync.OnceFunc(func() { close(blocked.resume) })
		var events atomic.Int32
		var calls atomic.Int32
		engine := &orchestrator.Engine{Store: blocked, ApprovalCodec: codec,
			Executor: recoverySQLExecutor(func(context.Context, *types.Task, *planner.PlanDecision) ([]types.StepTrace, error) {
				calls.Add(1)
				return []types.StepTrace{recoverySQLResultTrace()}, nil
			}),
			EventCallback: func(string, types.TaskStatus) { events.Add(1) },
		}
		task, approval := loadRecoverySQLCheckpoint(t, first, taskID, approvalID)
		done := make(chan struct{})
		var recoveryErr error
		var recovered bool
		go func() {
			recovered, recoveryErr = engine.RecoverApprovedApproval(t.Context(), task, approval, "old-owner")
			close(done)
		}()
		t.Cleanup(func() { release(); awaitRecoverySQLSignal(t, done) })
		awaitRecoverySQLSignal(t, blocked.entered)
		if err := peer.ReleaseTaskLease(t.Context(), taskID, "old-owner"); err != nil {
			t.Fatal(err)
		}
		if ok, err := peer.AcquireTaskLease(t.Context(), taskID, "new-owner", time.Minute); err != nil || !ok {
			t.Fatalf("peer acquire = %v, %v", ok, err)
		}
		peerTask, _ := loadRecoverySQLCheckpoint(t, peer, taskID, approvalID)
		peerTask.Goal = "peer-owned result"
		peerTask.Status = types.StatusRunning
		peerTask.Trace = append(peerTask.Trace, types.StepTrace{Step: 2, Action: "peer_action", Observation: "must survive"})
		peerTask.StepCount = 2
		if err := peer.SaveFullTask(store.WithTaskLease(t.Context(), taskID, "new-owner"), peerTask); err != nil {
			t.Fatal(err)
		}
		before, _ := loadRecoverySQLCheckpoint(t, peer, taskID, approvalID)
		release()
		awaitRecoverySQLSignal(t, done)
		if !recovered || !errors.Is(recoveryErr, store.ErrTaskLeaseLost) || calls.Load() != 1 || events.Load() != 0 {
			t.Fatalf("stale recovery = %v, %v; calls=%d events=%d", recovered, recoveryErr, calls.Load(), events.Load())
		}
		after, consumed := loadRecoverySQLCheckpoint(t, peer, taskID, approvalID)
		if !reflect.DeepEqual(after, before) || consumed.Status != types.ApprovalConsumed {
			t.Fatalf("stale recovery changed peer task or reset consumption: task=%+v approval=%+v", after, consumed)
		}
		if ok, err := first.AcquireTaskLease(t.Context(), taskID, "intruder", time.Minute); err != nil || ok {
			t.Fatalf("old recovery released peer lease: acquired=%v err=%v", ok, err)
		}
	})
}

type recoverySQLExecutor func(context.Context, *types.Task, *planner.PlanDecision) ([]types.StepTrace, error)

func (execute recoverySQLExecutor) Execute(ctx context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
	return execute(ctx, task, decision)
}

// Only scheduling is controlled; the actual lease check and commit use SQL.
type recoverySQLCommitBarrier struct {
	recoverySQLStore
	entered, resume chan struct{}
}

func (s *recoverySQLCommitBarrier) SaveFullTask(ctx context.Context, task *types.Task) error {
	close(s.entered)
	select {
	case <-s.resume:
		return s.recoverySQLStore.SaveFullTask(ctx, task)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func seedRecoverySQLCheckpoint(t *testing.T, open recoverySQLOpener, parameters map[string]any) (recoverySQLStore, recoverySQLStore, *approvalcrypto.Codec, string, string) {
	t.Helper()
	taskID := "recovery-contract-" + uuid.NewString()
	approvalID := taskID + "-approval"
	seed, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	t.Cleanup(func() {
		cleanup, err := open()
		if err != nil {
			t.Errorf("open fixture cleanup store: %v", err)
			return
		}
		defer cleanup.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := cleanup.DeleteTask(ctx, taskID); err != nil {
			t.Errorf("delete recovery fixture: %v", err)
		}
	})
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x59}, 32))
	if err != nil {
		t.Fatal(err)
	}
	task := &types.Task{ID: taskID, TenantID: "tenant-recovery", Goal: "recovery fixture", Status: types.StatusAwaitingApproval, StepCount: 1, Trace: recoverySQLInitialTrace()}
	if err := seed.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	action, err := json.Marshal(planner.ActionCall{Action: "write_file", Parameters: map[string]any{"path": "original.txt", "content": "original"}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := codec.Encrypt(action)
	if err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{ID: approvalID, TaskID: taskID, TenantID: task.TenantID, ActionPayload: payload, Status: types.ApprovalPending,
		Request: types.ApprovalRequest{ID: approvalID, TaskID: taskID, Action: "write_file", RiskLevel: types.RiskLevelHigh},
	}
	if err := seed.CreateApproval(t.Context(), approval); err != nil {
		t.Fatal(err)
	}
	engine := &orchestrator.Engine{Store: seed, ApprovalCodec: codec}
	if _, exists, persisted, err := engine.PersistApprovalResolution(t.Context(), taskID, approvalID, types.ApprovalResult{Approved: true, Parameters: parameters}); err != nil || !exists || !persisted {
		t.Fatalf("persist resolution: exists=%v persisted=%v err=%v", exists, persisted, err)
	}
	// Discard the original store/engine before constructing independent recovery clients.
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	peer, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	return first, peer, codec, taskID, approvalID
}

func loadRecoverySQLCheckpoint(t *testing.T, st recoverySQLStore, taskID, approvalID string) (*types.Task, *types.DurableApproval) {
	t.Helper()
	task, err := st.GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := st.GetApproval(t.Context(), approvalID, "tenant-recovery")
	if err != nil {
		t.Fatal(err)
	}
	return task, approval
}

func recoverySQLInitialTrace() []types.StepTrace {
	return []types.StepTrace{
		{Step: 1, Action: "read_file", Observation: "original source", Evidence: []types.Evidence{{Path: "source.txt", Lines: []string{"source"}}}},
		{Step: 1, Action: "citation_verify", Observation: "verified", AgentRole: types.AgentRoleVerifier},
	}
}

func recoverySQLResultTrace() types.StepTrace {
	return types.StepTrace{Step: 2, Action: "write_file", Observation: "approved output", AgentRole: types.AgentRoleExecutor, TokenUsage: types.TokenUsage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}}
}

func awaitRecoverySQLSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for recovery contract barrier")
	}
}

func assertRecoverySQLHTTPRead(t *testing.T, handler *Handler, taskID string, wantTrace []types.StepTrace) {
	t.Helper()
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskID, nil)
	c.Params = gin.Params{{Key: "id", Value: taskID}}
	handler.getTask(c)
	var got types.Task
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil {
		t.Fatalf("task read status=%d body=%s", response.Code, response.Body.String())
	}
	if got.ID != taskID || got.Status != types.StatusPaused || got.StepCount != 2 || !reflect.DeepEqual(got.Trace, wantTrace) {
		t.Fatalf("HTTP task does not preserve recovery checkpoint: %+v", got)
	}
}
