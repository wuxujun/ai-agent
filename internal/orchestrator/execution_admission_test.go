package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type admissionPlanner struct {
	calls int
	run   func(context.Context, *types.Task) (*planner.PlanDecision, error)
}

func (p *admissionPlanner) PlanNext(ctx context.Context, task *types.Task, _ func(string)) (*planner.PlanDecision, error) {
	p.calls++
	if p.run != nil {
		return p.run(ctx, task)
	}
	return &planner.PlanDecision{Stop: true}, nil
}

func engineAdmissionFixture(t *testing.T) (*Engine, *store.SQLiteStore, *types.Task, context.Context, int64) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewSQLiteStoreWithExecutionGate(filepath.Join(dir, "engine.sqlite"), strings.Repeat("a", 64))
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
	attempt, err := st.StartSQLiteExecutionAttempt(t.Context(), task.ID, task.TenantID, "attempt", strings.Repeat("b", 64), epoch, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithSQLiteExecutionAttempt(t.Context(), attempt)
	engine := &Engine{Mode: ModeLegacy, Store: st, ExecutionAdmission: st, Planner: &admissionPlanner{}}
	return engine, st, task, ctx, epoch
}
func TestExecutionAdmissionDirectEngineDeniedBeforeEffects(t *testing.T) {
	for _, runAll := range []bool{false, true} {
		t.Run(map[bool]string{true: "RunAll", false: "Next"}[runAll], func(t *testing.T) {
			engine, st, task, ctx, epoch := engineAdmissionFixture(t)
			before := *task
			if _, err := st.BlockSQLiteExecutionGate(t.Context(), task.ID, task.TenantID, epoch, false); err != nil {
				t.Fatal(err)
			}
			var err error
			if runAll {
				err = engine.RunAll(ctx, task)
			} else {
				err = engine.Next(ctx, task)
			}
			if !errors.Is(err, types.ErrExecutionAdmission) || engine.Planner.(*admissionPlanner).calls != 0 || !reflect.DeepEqual(before, *task) {
				t.Fatal("denied execution had effects", err)
			}
			out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), task.ID, task.TenantID)
			if err != nil || out.Attempts[0].State != "running" || out.ReceiptsComplete {
				t.Fatal("denial fabricated exit", err)
			}
		})
	}
	engine, _, task, _, _ := engineAdmissionFixture(t)
	if err := engine.Next(t.Context(), task); !errors.Is(err, types.ErrExecutionAdmission) {
		t.Fatal("missing attempt context accepted", err)
	}
}
func TestExecutionAdmissionValidRunAllAndPreDispatchRevocation(t *testing.T) {
	engine, st, task, ctx, _ := engineAdmissionFixture(t)
	if err := engine.RunAll(ctx, task); err != nil || engine.Planner.(*admissionPlanner).calls != 1 {
		t.Fatal("valid nested RunAll/Next failed", err)
	}
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), task.ID, task.TenantID)
	if err != nil || out.Attempts[0].State != "running" || out.ReceiptsComplete {
		t.Fatal("engine return fabricated finalization", err)
	}
	engine, st, task, ctx, epoch := engineAdmissionFixture(t)
	exec := &recoveryExecutor{}
	engine.Executor = exec
	engine.Planner = &admissionPlanner{run: func(context.Context, *types.Task) (*planner.PlanDecision, error) {
		if _, err := st.BlockSQLiteExecutionGate(t.Context(), task.ID, task.TenantID, epoch, false); err != nil {
			return nil, err
		}
		return &planner.PlanDecision{Actions: []planner.ActionCall{{Action: "fixture-read"}}}, nil
	}}
	if err := engine.Next(ctx, task); !errors.Is(err, types.ErrExecutionAdmission) || exec.calls != 0 || task.Status != types.StatusRunning {
		t.Fatal("revocation bypassed tool dispatch", err)
	}
}
func TestExecutionAdmissionPreservesLeaseAndRejectsScopeChange(t *testing.T) {
	engine, _, task, ctx, _ := engineAdmissionFixture(t)
	ctx = WithExecutionLeaseCheck(ctx, func() error { return store.ErrTaskLeaseLost })
	if err := engine.Next(ctx, task); !errors.Is(err, store.ErrTaskLeaseLost) {
		t.Fatal("existing lease check lost", err)
	}
	engine, _, task, ctx, _ = engineAdmissionFixture(t)
	task.TenantID = "other"
	if err := engine.Next(ctx, task); !errors.Is(err, types.ErrExecutionAdmission) {
		t.Fatal("tenant scope mismatch accepted", err)
	}
}
func TestExecutionAdmissionApprovalRecoveryDeniedBeforeAcquire(t *testing.T) {
	engine, st, task, ctx, epoch := engineAdmissionFixture(t)
	if _, err := st.BlockSQLiteExecutionGate(t.Context(), task.ID, task.TenantID, epoch, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.beginApprovalRecovery(ctx, task, "ordinary-recovery"); !errors.Is(err, types.ErrExecutionAdmission) {
		t.Fatal("recovery bypassed admission", err)
	}
}

func TestExecutionAdmissionRejectsAnotherSourceAttempt(t *testing.T) {
	_, _, _, foreignContext, _ := engineAdmissionFixture(t)
	engine, _, task, _, _ := engineAdmissionFixture(t)
	// IDs, tenants, instance hashes, gate epochs and task incarnations match;
	// only the independently created database origin differs.
	if err := engine.Next(foreignContext, task); !errors.Is(err, types.ErrExecutionAdmission) || engine.Planner.(*admissionPlanner).calls != 0 {
		t.Fatal("foreign source ticket accepted", err)
	}
}
