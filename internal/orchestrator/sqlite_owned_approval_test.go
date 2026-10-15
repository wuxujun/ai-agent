package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSQLiteOwnedApprovalRecoveryUsesSingleTaskOwner(t *testing.T) {
	for _, approved := range []bool{true, false} {
		t.Run(map[bool]string{true: "approved", false: "rejected"}[approved], func(t *testing.T) {
			engine, st, task, options := sqliteOwnedFixture(t)
			task.Status = types.StatusAwaitingApproval
			task.StepCount = 0
			if err := st.SaveFullTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x33}, 32))
			if err != nil {
				t.Fatal(err)
			}
			engine.ApprovalCodec = codec
			exec := &recoveryExecutor{}
			engine.Executor = exec
			action, _ := json.Marshal(planner.ActionCall{Action: "write_file", Parameters: map[string]any{"path": "fixture-only", "content": "test"}})
			encrypted, err := codec.Encrypt(action)
			if err != nil {
				t.Fatal(err)
			}
			resolution, _ := json.Marshal(types.ApprovalResult{Approved: approved, Message: "fixture-feedback"})
			encryptedResolution, err := codec.Encrypt(resolution)
			if err != nil {
				t.Fatal(err)
			}
			status := types.ApprovalRejected
			if approved {
				status = types.ApprovalApproved
			}
			approval := &types.DurableApproval{ID: "approval", TaskID: task.ID, TenantID: task.TenantID, Request: types.ApprovalRequest{ID: "approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: encrypted, ResolutionPayload: encryptedResolution, Status: status}
			if err := st.CreateApproval(t.Context(), approval); err != nil {
				t.Fatal(err)
			}
			options.Cleanup = func(ctx context.Context, _ SQLiteOwnedExecutionIdentity) (string, error) {
				out := ownedSnapshot(t, st)
				if out.LeaseState != "active-unresolved" || out.Attempts[0].State != "exited" {
					t.Fatal("nested recovery released outer lease")
				}
				saved, err := st.GetApproval(ctx, approval.ID, approval.TenantID)
				if err != nil || saved.Status != types.ApprovalConsumed {
					t.Fatal("approval not consumed", err)
				}
				return strings.Repeat("c", 64), nil
			}
			recovered, err := engine.RunOwnedSQLiteApprovalRecovery(t.Context(), st, task, approval, options)
			if err != nil || !recovered || task.Status != types.StatusPaused {
				t.Fatal("owned recovery", recovered, err)
			}
			expectedCalls := 0
			if approved {
				expectedCalls = 1
			}
			if exec.calls != expectedCalls {
				t.Fatal("unexpected tool calls", exec.calls)
			}
			out := ownedSnapshot(t, st)
			if len(out.Attempts) != 1 || out.Attempts[0].State != "complete" || out.LeaseState != "absent" {
				t.Fatal("owned recovery completion")
			}
			// Closed gate must reject a subsequent recovery before consumption/execution.
			if _, err := st.BlockSQLiteExecutionGate(t.Context(), task.ID, task.TenantID, options.ExpectedGateEpoch, false); err != nil {
				t.Fatal(err)
			}
			options.AttemptID = "recovery-retry"
			recovered, err = engine.RunOwnedSQLiteApprovalRecovery(t.Context(), st, task, approval, options)
			if err == nil || recovered || exec.calls != expectedCalls {
				t.Fatal("blocked recovery executed", err)
			}
		})
	}
}
func TestSQLiteOwnedApprovalRejectsCrossTenantBeforeRegistration(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	approval := &types.DurableApproval{TaskID: task.ID, TenantID: "other", Status: types.ApprovalApproved}
	if ok, err := engine.RunOwnedSQLiteApprovalRecovery(t.Context(), st, task, approval, options); ok || !errors.Is(err, types.ErrExecutionAdmission) {
		t.Fatal(err)
	}
	if len(ownedSnapshot(t, st).Attempts) != 0 {
		t.Fatal("invalid recovery registered attempt")
	}
}

func TestSQLiteOwnedApprovalDispatcherProviderFailureDoesNotFallback(t *testing.T) {
	engine, st, task, _ := sqliteOwnedFixture(t)
	calls := 0
	if err := engine.SetOwnedSQLiteApprovalDispatcher(func(context.Context, string, string) (SQLiteOwnedExecutionOptions, error) {
		calls++
		return SQLiteOwnedExecutionOptions{}, errors.New("secret-provider-error")
	}); err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{TaskID: task.ID, TenantID: task.TenantID, Status: types.ApprovalApproved}
	recovered, err := engine.DispatchApprovalRecovery(t.Context(), task, approval, "ordinary-owner")
	if recovered || !errors.Is(err, types.ErrExecutionAdmission) || strings.Contains(err.Error(), "secret") || calls != 1 || len(ownedSnapshot(t, st).Attempts) != 0 {
		t.Fatal("provider failure fallback", err)
	}
	// A changed Engine/store binding cannot reuse the trusted adapter.
	engine.ExecutionAdmission = nil
	if _, err := engine.DispatchApprovalRecovery(t.Context(), task, approval, "ordinary-owner"); !errors.Is(err, types.ErrExecutionAdmission) || calls != 1 {
		t.Fatal("changed binding", err)
	}
}
