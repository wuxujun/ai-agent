package multiagent

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

type admissionCheckerFunc func(context.Context, *types.Task) error

func (f admissionCheckerFunc) CheckExecutionAdmission(ctx context.Context, task *types.Task) error {
	return f(ctx, task)
}
func TestExecutionAdmissionDirectCoordinatorAndDispatch(t *testing.T) {
	task := &types.Task{ID: "root", TenantID: "tenant", Status: types.StatusRunning}
	denied := admissionCheckerFunc(func(context.Context, *types.Task) error { return types.ErrExecutionAdmission })
	c := &Coordinator{ExecutionAdmission: denied}
	if err := c.Run(t.Context(), task); !errors.Is(err, types.ErrExecutionAdmission) || len(task.Trace) != 0 {
		t.Fatal("direct coordinator bypassed admission", err)
	}
	allowed := true
	checker := admissionCheckerFunc(func(context.Context, *types.Task) error {
		if !allowed {
			return types.ErrExecutionAdmission
		}
		return nil
	})
	owner := &types.ExecutionAdmissionOwner{}
	ctx, err := types.BindExecutionAdmission(t.Context(), owner, checker, task)
	if err != nil {
		t.Fatal(err)
	}
	// The Engine's inherited policy is sufficient even when Coordinator's
	// explicit checker is nil, including dispatch after an earlier admission.
	allowed = false
	c = &Coordinator{}
	if err := c.Run(ctx, task); !errors.Is(err, types.ErrExecutionAdmission) {
		t.Fatal("inherited admission lost", err)
	}
	if _, err := c.executeWorkflowStep(ctx, "fixture", ResearchStep{}); !errors.Is(err, types.ErrExecutionAdmission) {
		t.Fatal("dispatch bypassed inherited policy", err)
	}
}
