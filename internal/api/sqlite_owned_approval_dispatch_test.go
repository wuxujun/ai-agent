package api

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

func seedOwnedDispatcherApproval(t *testing.T, h *Handler, approved bool) *competingRecoveryExecutor {
	t.Helper()
	st := h.ownedSQLiteStep.store
	task, err := st.GetTask(t.Context(), "owned-http")
	if err != nil {
		t.Fatal(err)
	}
	task.Status = types.StatusAwaitingApproval
	task.StepCount = 0
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	h.engine.ApprovalCodec = codec
	exec := &competingRecoveryExecutor{}
	h.engine.Executor = exec
	action, _ := json.Marshal(planner.ActionCall{Action: "write_file", Parameters: map[string]any{"path": "fixture", "content": "test"}})
	payload, err := codec.Encrypt(action)
	if err != nil {
		t.Fatal(err)
	}
	resolution, _ := json.Marshal(types.ApprovalResult{Approved: approved, Message: "fixture feedback"})
	encrypted, err := codec.Encrypt(resolution)
	if err != nil {
		t.Fatal(err)
	}
	status := types.ApprovalRejected
	if approved {
		status = types.ApprovalApproved
	}
	approval := &types.DurableApproval{ID: "owned-decision", TaskID: task.ID, TenantID: task.TenantID, Status: status, Request: types.ApprovalRequest{ID: "owned-decision", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: payload, ResolutionPayload: encrypted}
	if err := st.CreateApproval(t.Context(), approval); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.SetOwnedSQLiteApprovalDispatcher(orchestrator.SQLiteOwnedApprovalOptionsProvider(h.ownedSQLiteStep.options)); err != nil {
		t.Fatal(err)
	}
	return exec
}

func TestOwnedSQLiteApprovalDispatcherBackground(t *testing.T) {
	for _, approved := range []bool{true, false} {
		t.Run(map[bool]string{true: "approved", false: "rejected"}[approved], func(t *testing.T) {
			_, h, st, _, _ := ownedStepFixture(t, nil)
			exec := seedOwnedDispatcherApproval(t, h, approved)
			h.startDurableApprovalRecovery("owned-http", "owned-decision")
			h.Wait()
			out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
			saved, loadErr := st.GetTask(t.Context(), "owned-http")
			decision, decisionErr := st.GetApproval(t.Context(), "owned-decision", "default")
			want := int32(0)
			if approved {
				want = 1
			}
			if err != nil || loadErr != nil || decisionErr != nil || exec.calls.Load() != want || saved.Status != types.StatusPaused || decision.Status != types.ApprovalConsumed || len(out.Attempts) != 1 || out.Attempts[0].State != "complete" || out.LeaseState != "absent" {
				t.Fatal("background owned recovery", err, loadErr, decisionErr)
			}
		})
	}
}

func TestOwnedSQLiteApprovalDispatcherBlockedPreservesDecision(t *testing.T) {
	_, h, st, _, epoch := ownedStepFixture(t, nil)
	exec := seedOwnedDispatcherApproval(t, h, true)
	if _, err := st.BlockSQLiteExecutionGate(t.Context(), "owned-http", "default", epoch, false); err != nil {
		t.Fatal(err)
	}
	h.startDurableApprovalRecovery("owned-http", "owned-decision")
	h.Wait()
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	decision, decisionErr := st.GetApproval(t.Context(), "owned-decision", "default")
	if err != nil || decisionErr != nil || len(out.Attempts) != 0 || exec.calls.Load() != 0 || decision.Status != types.ApprovalApproved {
		t.Fatal("gate bypassed", err, decisionErr)
	}
}

func TestOwnedSQLiteApprovalDispatcherShutdownWaitsForCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	_, h, st, _, _ := ownedStepFixture(t, func(ctx context.Context, _ orchestrator.SQLiteOwnedExecutionIdentity) (string, error) {
		close(entered)
		select {
		case <-release:
			return strings.Repeat("c", 64), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	exec := seedOwnedDispatcherApproval(t, h, true)
	h.startDurableApprovalRecovery("owned-http", "owned-decision")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no cleanup")
	}
	// Duplicate dispatch cannot replace this local owner or execute again.
	h.startDurableApprovalRecovery("owned-http", "owned-decision")
	done := make(chan error, 1)
	go func() { done <- h.Shutdown(t.Context()) }()
	select {
	case <-done:
		t.Fatal("shutdown skipped cleanup")
	case <-time.After(20 * time.Millisecond):
	}
	h.activeTasksMu.Lock()
	retained := h.activeTasks["owned-http"] != nil
	h.activeTasksMu.Unlock()
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if !retained || err != nil || out.Attempts[0].State != "exited" || out.LeaseState == "absent" {
		t.Fatal("shutdown released owner", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown blocked")
	}
	out, err = st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if err != nil || out.Attempts[0].State != "complete" || out.LeaseState != "absent" || exec.calls.Load() != 1 {
		t.Fatal("shutdown finalize", err)
	}
}

func TestOwnedSQLiteApprovalDispatcherQueuedBehindLocalOwner(t *testing.T) {
	for _, cancelPending := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "cancel pending"}[cancelPending], func(t *testing.T) {
			_, h, st, _, _ := ownedStepFixture(t, nil)
			exec := seedOwnedDispatcherApproval(t, h, true)
			occupied := &activeRun{owned: true, cancel: func() {}}
			h.activeTasksMu.Lock()
			h.activeTasks["owned-http"] = occupied
			h.activeTasksMu.Unlock()
			h.startDurableApprovalRecovery("owned-http", "owned-decision")
			h.activeTasksMu.Lock()
			worker, exists := h.ownedApprovalWorkers["owned-decision"]
			h.activeTasksMu.Unlock()
			if !exists || worker.run == occupied {
				t.Fatal("queued recovery not tracked")
			}
			if cancelPending {
				if !h.CancelTaskByID("owned-http") {
					t.Fatal("pending cancel missing")
				}
				h.Wait()
			} else {
				h.activeTasksMu.Lock()
				delete(h.activeTasks, "owned-http")
				h.activeTasksMu.Unlock()
				h.Wait()
			}
			out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
			if err != nil {
				t.Fatal(err)
			}
			if cancelPending {
				if len(out.Attempts) != 0 || exec.calls.Load() != 0 {
					t.Fatal("canceled pending worker executed")
				}
				h.activeTasksMu.Lock()
				retained := h.activeTasks["owned-http"] == occupied
				delete(h.activeTasks, "owned-http")
				h.activeTasksMu.Unlock()
				if !retained {
					t.Fatal("pending cleanup removed peer slot")
				}
			} else if len(out.Attempts) != 1 || out.Attempts[0].State != "complete" || exec.calls.Load() != 1 {
				t.Fatal("queued decision dropped")
			}
		})
	}
}
