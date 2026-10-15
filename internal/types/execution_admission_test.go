package types

import (
	"context"
	"errors"
	"testing"
)

type executionCheckerFunc func(context.Context, *Task) error

func (f executionCheckerFunc) CheckExecutionAdmission(ctx context.Context, task *Task) error {
	return f(ctx, task)
}
func TestExecutionAdmissionNestedPolicyScopeAndSanitization(t *testing.T) {
	task := &Task{ID: "task", TenantID: "tenant"}
	owner := &ExecutionAdmissionOwner{}
	calls := 0
	checker := executionCheckerFunc(func(_ context.Context, scope *Task) error { calls++; scope.ID = "mutated"; return nil })
	ctx, err := BindExecutionAdmission(t.Context(), owner, checker, task)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		ctx, err = BindExecutionAdmission(ctx, owner, checker, task)
		if err != nil {
			t.Fatal(err)
		}
	}
	before := calls
	if err := CheckExecutionAdmission(ctx); err != nil || calls != before+1 || task.ID != "task" {
		t.Fatal("duplicate binding or mutation", err)
	}
	changed := *task
	changed.TenantID = "other"
	if err := CheckExecutionAdmissionForTask(ctx, &changed); !errors.Is(err, ErrExecutionAdmission) {
		t.Fatal("scope change accepted", err)
	}
	other := &ExecutionAdmissionOwner{}
	denied := executionCheckerFunc(func(context.Context, *Task) error { return errors.New("secret-provider-body") })
	if _, err := BindExecutionAdmission(ctx, other, denied, task); err != ErrExecutionAdmission {
		t.Fatal("provider error not sanitized", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := CheckExecutionAdmission(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := CheckExecutionAdmission(t.Context()); err != nil {
		t.Fatal("default context changed", err)
	}
}
