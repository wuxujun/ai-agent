package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type ownedScanExecutor struct{ calls atomic.Int32 }

func (e *ownedScanExecutor) Execute(_ context.Context, task *types.Task, _ *planner.PlanDecision) ([]types.StepTrace, error) {
	e.calls.Add(1)
	return []types.StepTrace{{Step: task.StepCount + 1, Action: "write_file", Observation: "fixture"}}, nil
}
func ownedScanFixture(t *testing.T, approved bool, cleanup func(context.Context, orchestrator.SQLiteOwnedExecutionIdentity) (string, error)) (*store.SQLiteStore, *orchestrator.Engine, *ownedScanExecutor, int64) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewSQLiteStoreWithExecutionGate(filepath.Join(dir, "scan.sqlite"), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	task := &types.Task{ID: "scan-owned", TenantID: "default", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	epoch, err := st.EnrollSQLiteExecutionGate(t.Context(), task.ID, task.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	action, _ := json.Marshal(planner.ActionCall{Action: "write_file", Parameters: map[string]any{"path": "fixture"}})
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
	approval := &types.DurableApproval{ID: "scan-decision", TaskID: task.ID, TenantID: task.TenantID, Status: status, Request: types.ApprovalRequest{ID: "scan-decision", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: payload, ResolutionPayload: encrypted}
	if err := st.CreateApproval(t.Context(), approval); err != nil {
		t.Fatal(err)
	}
	executor := &ownedScanExecutor{}
	eng := &orchestrator.Engine{Store: st, ExecutionAdmission: st, ApprovalCodec: codec, Executor: executor}
	if cleanup == nil {
		cleanup = func(context.Context, orchestrator.SQLiteOwnedExecutionIdentity) (string, error) {
			return strings.Repeat("c", 64), nil
		}
	}
	if err := eng.SetOwnedSQLiteApprovalDispatcher(func(context.Context, string, string) (orchestrator.SQLiteOwnedExecutionOptions, error) {
		return orchestrator.SQLiteOwnedExecutionOptions{AttemptID: "scan-attempt", InstanceSHA256: strings.Repeat("b", 64), ExpectedGateEpoch: epoch, TTL: 3 * time.Second, Cleanup: cleanup}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return st, eng, executor, epoch
}
func TestOwnedSQLiteStartupApprovalRecovery(t *testing.T) {
	for _, scenario := range []string{"approved", "rejected", "blocked"} {
		t.Run(scenario, func(t *testing.T) {
			st, eng, exec, epoch := ownedScanFixture(t, scenario != "rejected", nil)
			if scenario == "blocked" {
				if _, err := st.BlockSQLiteExecutionGate(t.Context(), "scan-owned", "default", epoch, false); err != nil {
					t.Fatal(err)
				}
			}
			runner := startPausedTaskScan(st, eng)
			runner.Wait()
			runner.Stop()
			out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "scan-owned", "default")
			saved, loadErr := st.GetTask(t.Context(), "scan-owned")
			approval, approvalErr := st.GetApproval(t.Context(), "scan-decision", "default")
			if err != nil || loadErr != nil || approvalErr != nil {
				t.Fatal(err, loadErr, approvalErr)
			}
			if scenario == "blocked" {
				if len(out.Attempts) != 0 || exec.calls.Load() != 0 || approval.Status != types.ApprovalApproved || saved.Status != types.StatusAwaitingApproval {
					t.Fatal("blocked scan bypass")
				}
				return
			}
			want := int32(0)
			if scenario == "approved" {
				want = 1
			}
			if len(out.Attempts) != 1 || out.Attempts[0].State != "complete" || out.LeaseState != "absent" || exec.calls.Load() != want || approval.Status != types.ApprovalConsumed || saved.Status != types.StatusPaused {
				t.Fatal("scan lifetime incomplete")
			}
		})
	}
}
func TestOwnedSQLiteStartupStopWaitsForCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	st, eng, _, _ := ownedScanFixture(t, true, func(ctx context.Context, _ orchestrator.SQLiteOwnedExecutionIdentity) (string, error) {
		close(entered)
		select {
		case <-release:
			return strings.Repeat("c", 64), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	runner := startPausedTaskScan(st, eng)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup absent")
	}
	runner.Stop()
	done := make(chan struct{})
	go func() { runner.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("scan did not join cleanup")
	case <-time.After(20 * time.Millisecond):
	}
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "scan-owned", "default")
	if err != nil || out.Attempts[0].State != "exited" || out.LeaseState == "absent" {
		t.Fatal("stop released owner", err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scan stop blocked")
	}
	out, err = st.ReadSQLiteExecutionGateSnapshot(t.Context(), "scan-owned", "default")
	if err != nil || out.Attempts[0].State != "complete" || out.LeaseState != "absent" {
		t.Fatal("scan stop finalize", err)
	}
}
