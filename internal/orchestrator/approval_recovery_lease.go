package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Recovery is also called by startup scans, so ownership belongs here rather
// than only in the HTTP caller. Each attempt reloads its checkpoint after locking.
func (e *Engine) beginApprovalRecovery(ctx context.Context, task *types.Task, owner string) (context.Context, func(), error) {
	renewer, ok := e.Store.(store.TaskLeaseStore)
	if !ok {
		return nil, nil, errors.New("store does not support task lease renewal")
	}
	started := time.Now()
	acquired, err := e.Store.AcquireTaskLease(ctx, task.ID, owner, 30*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if !acquired {
		return nil, nil, store.ErrTaskLeaseBusy
	}
	ownedCtx, cancel := context.WithCancelCause(store.WithTaskLease(ctx, task.ID, owner))
	lease := store.NewExecutionLease(func() { cancel(store.ErrTaskLeaseLost) })
	ownedCtx = WithExecutionLeaseCheck(ownedCtx, lease.Err)
	lease.Start(ownedCtx, renewer, task.ID, owner, 30*time.Second, started)
	taskID := task.ID
	cleanup := func() {
		lease.Finish()
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer releaseCancel()
		_ = e.Store.ReleaseTaskLease(releaseCtx, taskID, owner)
		cancel(context.Canceled)
	}
	latest, err := e.Store.GetTask(ownedCtx, taskID)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if latest.TenantID != task.TenantID {
		cleanup()
		return nil, nil, errors.New("recovery task tenant changed")
	}
	*task = *latest
	return ownedCtx, cleanup, nil
}

func recoveryExecutionError(ctx context.Context) error {
	if err := executionLeaseError(ctx); err != nil {
		return err
	}
	return ctx.Err()
}
