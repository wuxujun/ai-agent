package types

import (
	"context"
	"errors"
)

var ErrExecutionAdmission = errors.New("execution admission denied")

// ExecutionAdmissionChecker is installed by trusted construction code before
// execution. It only checks the immutable task ID/tenant scope; it must obey
// context cancellation. No checker is selected from task or artifact fields.
type ExecutionAdmissionChecker interface {
	CheckExecutionAdmission(context.Context, *Task) error
}

// A non-zero-size owner keeps nested RunAll/Next and coordinator fallbacks from
// adding duplicate policy checks. Construct once; do not copy a live executor.
type ExecutionAdmissionOwner struct{ marker byte }
type executionAdmissionKey struct{}
type executionAdmissionBinding struct {
	owner            *ExecutionAdmissionOwner
	checker          ExecutionAdmissionChecker
	taskID, tenantID string
	parent           *executionAdmissionBinding
}

func BindExecutionAdmission(ctx context.Context, owner *ExecutionAdmissionOwner, checker ExecutionAdmissionChecker, task *Task) (context.Context, error) {
	binding, _ := ctx.Value(executionAdmissionKey{}).(*executionAdmissionBinding)
	if checker == nil {
		return ctx, CheckExecutionAdmissionForTask(ctx, task)
	}
	if owner == nil || task == nil {
		return ctx, ErrExecutionAdmission
	}
	depth := 0
	for prior := binding; prior != nil; prior = prior.parent {
		depth++
		if prior.owner == owner {
			return ctx, CheckExecutionAdmissionForTask(ctx, task)
		}
	}
	if depth >= 8 {
		return ctx, ErrExecutionAdmission
	}
	next := &executionAdmissionBinding{owner: owner, checker: checker, taskID: task.ID, tenantID: task.TenantID, parent: binding}
	ctx = context.WithValue(ctx, executionAdmissionKey{}, next)
	return ctx, CheckExecutionAdmissionForTask(ctx, task)
}

func CheckExecutionAdmissionForTask(ctx context.Context, task *Task) error {
	binding, _ := ctx.Value(executionAdmissionKey{}).(*executionAdmissionBinding)
	for prior := binding; prior != nil; prior = prior.parent {
		if task == nil || task.ID != prior.taskID || task.TenantID != prior.tenantID {
			return ErrExecutionAdmission
		}
	}
	return CheckExecutionAdmission(ctx)
}
func CheckExecutionAdmission(ctx context.Context) error {
	binding, _ := ctx.Value(executionAdmissionKey{}).(*executionAdmissionBinding)
	if binding != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	for prior := binding; prior != nil; prior = prior.parent {
		// The checker cannot mutate the live engine's task or its scope binding.
		scope := &Task{ID: prior.taskID, TenantID: prior.tenantID}
		if err := prior.checker.CheckExecutionAdmission(ctx, scope); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return ErrExecutionAdmission
		}
	}
	return nil
}
