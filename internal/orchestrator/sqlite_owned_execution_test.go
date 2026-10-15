package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func sqliteOwnedFixture(t *testing.T) (*Engine, *store.SQLiteStore, *types.Task, SQLiteOwnedExecutionOptions) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewSQLiteStoreWithExecutionGate(filepath.Join(dir, "owned.sqlite"), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	task := &types.Task{ID: "task", TenantID: "tenant", Goal: "fixture", Status: types.StatusRunning, StepCount: 1, MaxSteps: 3, ToolBudget: 3}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	epoch, err := st.EnrollSQLiteExecutionGate(t.Context(), task.ID, task.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Mode: ModeLegacy, Store: st, ExecutionAdmission: st, Planner: &admissionPlanner{}}
	options := SQLiteOwnedExecutionOptions{AttemptID: "owned", InstanceSHA256: strings.Repeat("b", 64), ExpectedGateEpoch: epoch, TTL: 3 * time.Second, RunAll: true, Cleanup: func(context.Context, SQLiteOwnedExecutionIdentity) (string, error) {
		return strings.Repeat("c", 64), nil
	}}
	return engine, st, task, options
}
func ownedSnapshot(t *testing.T, st *store.SQLiteStore) *store.SQLiteExecutionGateSnapshot {
	t.Helper()
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "task", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestSQLiteOwnedLifecycleOrderAndReload(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	// The wrapper must reload the durable checkpoint, not run the stale caller.
	task.Goal = "stale"
	task.StepCount = 0
	options.Cleanup = func(ctx context.Context, identity SQLiteOwnedExecutionIdentity) (string, error) {
		out := ownedSnapshot(t, st)
		saved, err := st.GetTask(ctx, identity.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if out.Attempts[0].State != "exited" || out.LeaseState != "active-unresolved" || saved.Status != types.StatusCompleted || saved.Goal != "fixture" {
			t.Fatal("cleanup preceded exit/final flush")
		}
		return strings.Repeat("c", 64), nil
	}
	if err := engine.RunOwnedSQLiteExecution(t.Context(), st, task, options); err != nil {
		t.Fatal(err)
	}
	out := ownedSnapshot(t, st)
	if out.Attempts[0].State != "complete" || out.LeaseState != "absent" || out.ReceiptsComplete || out.Status != "unverified" {
		t.Fatal("owned completion")
	}
	if _, err := st.BlockSQLiteExecutionGate(t.Context(), task.ID, task.TenantID, options.ExpectedGateEpoch, false); err != nil {
		t.Fatal(err)
	}
	if !ownedSnapshot(t, st).ReceiptsComplete {
		t.Fatal("local receipts absent")
	}
}
func TestSQLiteOwnedCleanupFailureRetainsExited(t *testing.T) {
	for _, failure := range []string{"error", "missing", "panic"} {
		t.Run(failure, func(t *testing.T) {
			engine, st, task, options := sqliteOwnedFixture(t)
			options.Cleanup = func(context.Context, SQLiteOwnedExecutionIdentity) (string, error) {
				switch failure {
				case "panic":
					panic("fixture cleanup panic")
				case "error":
					return "", errors.New("secret-cleanup-error")
				}
				return "", nil
			}
			if failure == "panic" {
				func() {
					defer func() {
						if recover() == nil {
							t.Fatal("panic swallowed")
						}
					}()
					_ = engine.RunOwnedSQLiteExecution(t.Context(), st, task, options)
				}()
			} else if err := engine.RunOwnedSQLiteExecution(t.Context(), st, task, options); !errors.Is(err, ErrSQLiteOwnedCleanup) || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
			out := ownedSnapshot(t, st)
			if out.Attempts[0].State != "exited" || out.Attempts[0].FlushSHA256 != "" || out.Attempts[0].CleanupSHA256 != "" || out.LeaseState == "absent" || out.ReceiptsComplete {
				t.Fatal("cleanup failure finalized")
			}
		})
	}
}
func TestSQLiteOwnedCancelFlushesBeforeFinalize(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	engine.Planner = &admissionPlanner{run: func(context.Context, *types.Task) (*planner.PlanDecision, error) {
		cancel()
		return nil, context.Canceled
	}}
	called := false
	options.Cleanup = func(cleanupCtx context.Context, _ SQLiteOwnedExecutionIdentity) (string, error) {
		called = true
		if cleanupCtx.Err() != nil {
			t.Fatal("cleanup inherited cancellation")
		}
		saved, err := st.GetTask(cleanupCtx, "task")
		if err != nil || saved.Status != task.Status {
			t.Fatal("cancelled task not flushed", err)
		}
		return strings.Repeat("c", 64), nil
	}
	err := engine.RunOwnedSQLiteExecution(ctx, st, task, options)
	if !errors.Is(err, context.Canceled) || !called || ownedSnapshot(t, st).Attempts[0].State != "complete" {
		t.Fatal("cancel lifecycle", err)
	}
}
func TestSQLiteOwnedRenewalContinuesDuringCleanup(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	options.TTL = 2 * time.Second
	options.Cleanup = func(ctx context.Context, _ SQLiteOwnedExecutionIdentity) (string, error) {
		timer := time.NewTimer(4200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
		}
		if ownedSnapshot(t, st).LeaseState != "active-unresolved" {
			t.Fatal("ownership expired during cleanup")
		}
		return strings.Repeat("c", 64), nil
	}
	if err := engine.RunOwnedSQLiteExecution(t.Context(), st, task, options); err != nil {
		t.Fatal(err)
	}
	if ownedSnapshot(t, st).Attempts[0].State != "complete" {
		t.Fatal("renewed attempt not completed")
	}
}
func TestSQLiteOwnedFlushFailureStillCleans(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	cleaned := false
	engine.Planner = &admissionPlanner{run: func(context.Context, *types.Task) (*planner.PlanDecision, error) {
		task.TenantID = "wrong"
		return &planner.PlanDecision{Stop: true}, nil
	}}
	options.RunAll = false
	options.Cleanup = func(context.Context, SQLiteOwnedExecutionIdentity) (string, error) {
		cleaned = true
		return strings.Repeat("c", 64), nil
	}
	err := engine.RunOwnedSQLiteExecution(t.Context(), st, task, options)
	if !errors.Is(err, ErrSQLiteOwnedFlush) || !cleaned {
		t.Fatal("flush failure did not clean", err)
	}
	if ownedSnapshot(t, st).Attempts[0].State != "exited" {
		t.Fatal("unflushed attempt completed")
	}
}
func TestSQLiteOwnedValidatesBeforeRegistration(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	options.Cleanup = nil
	if err := engine.RunOwnedSQLiteExecution(t.Context(), st, task, options); !errors.Is(err, types.ErrExecutionAdmission) {
		t.Fatal(err)
	}
	if len(ownedSnapshot(t, st).Attempts) != 0 {
		t.Fatal("invalid options registered attempt")
	}
}

func TestSQLiteOwnedEnginePanicRetainsRunningAttempt(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	cleaned := false
	options.Cleanup = func(context.Context, SQLiteOwnedExecutionIdentity) (string, error) {
		cleaned = true
		return strings.Repeat("c", 64), nil
	}
	engine.Planner = &admissionPlanner{run: func(context.Context, *types.Task) (*planner.PlanDecision, error) { panic("fixture engine panic") }}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		_ = engine.RunOwnedSQLiteExecution(t.Context(), st, task, options)
	}()
	out := ownedSnapshot(t, st)
	if cleaned || out.Attempts[0].State != "running" || out.Attempts[0].ExitSHA256 != "" || out.ReceiptsComplete {
		t.Fatal("panic fabricated engine exit/cleanup")
	}
}
