package orchestrator

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// SQLiteOwnedApprovalOptionsProvider is trusted construction-time code, never
// selected from task/approval payloads or HTTP fields. It must supply unique
// attempt identity, current gate epoch and actual scope-owned cleanup.
type SQLiteOwnedApprovalOptionsProvider func(context.Context, string, string) (SQLiteOwnedExecutionOptions, error)

type sqliteOwnedApprovalDispatcher struct {
	store   *store.SQLiteStore
	options SQLiteOwnedApprovalOptionsProvider
}

// SetOwnedSQLiteApprovalDispatcher must be called before scans or requests start.
// Default nil preserves legacy recovery. No server/config option enables it.
func (e *Engine) SetOwnedSQLiteApprovalDispatcher(provider SQLiteOwnedApprovalOptionsProvider) error {
	st, ok := e.Store.(*store.SQLiteStore)
	if !ok || provider == nil || e.ExecutionAdmission != st {
		return types.ErrExecutionAdmission
	}
	e.ownedApprovalDispatcher = &sqliteOwnedApprovalDispatcher{st, provider}
	return nil
}

func (e *Engine) HasOwnedSQLiteApprovalDispatcher() bool { return e.ownedApprovalDispatcher != nil }

// DispatchApprovalRecovery is shared by startup scanning and HTTP background
// recovery. Owned selection occurs before ordinary task lease acquisition.
// It never falls back to legacy execution on provider/admission/cleanup failure.
func (e *Engine) DispatchApprovalRecovery(ctx context.Context, task *types.Task, approval *types.DurableApproval, legacyOwner string) (bool, error) {
	if task == nil || approval == nil || task.ID != approval.TaskID || task.TenantID != approval.TenantID {
		return false, types.ErrExecutionAdmission
	}
	if approval.Status != types.ApprovalApproved && approval.Status != types.ApprovalRejected {
		return false, nil
	}
	if adapter := e.ownedApprovalDispatcher; adapter != nil {
		if e.Store != adapter.store || e.ExecutionAdmission != adapter.store {
			return false, types.ErrExecutionAdmission
		}
		options, err := adapter.options(ctx, task.ID, task.TenantID)
		if err != nil {
			return false, types.ErrExecutionAdmission
		}
		// This dispatcher owns its notification and execution mode. Provider callbacks
		// cannot bypass registration or insert an unrelated run-all execution.
		options.OnAdmitted = nil
		return e.RunOwnedSQLiteApprovalRecovery(ctx, adapter.store, task, approval, options)
	}
	if approval.Status == types.ApprovalApproved {
		return e.RecoverApprovedApproval(ctx, task, approval, legacyOwner)
	}
	return e.RecoverRejectedApproval(ctx, task, approval, legacyOwner)
}
