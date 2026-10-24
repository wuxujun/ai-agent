package store

import (
	"context"
	"errors"

	"github.com/wuxujun/ai-agent/internal/types"
)

var ErrTaskProgressConflict = errors.New("task progress checkpoint is stale")

// TaskProgressCheckpoint is a sealed, detached copy of the previously observed
// prefix. It is only an optimization hint; a transaction must verify its version.
type TaskProgressCheckpoint struct {
	owner   Store
	taskID  string
	version int64
	trace   progressTraceSnapshot
}

type TaskProgressStore interface {
	SaveTaskProgress(context.Context, *types.Task, *TaskProgressCheckpoint) error
}

// CaptureTaskProgress must be called immediately after a successful complete
// read/save, before any mutation. The version binds that acknowledged snapshot;
// it cannot attest to an arbitrary caller-edited task with the same version.
func CaptureTaskProgress(st Store, task *types.Task) *TaskProgressCheckpoint {
	if _, ok := st.(TaskProgressStore); !ok || task == nil || task.MaintenanceVersion <= 0 {
		return nil
	}
	// Restore/owned fixtures maintain additional durable control versions via
	// triggers. Keep their existing complete-snapshot lifecycle unchanged.
	if sqlite, ok := st.(*SQLiteStore); ok && sqlite.restoreVersions != nil {
		return nil
	}
	return &TaskProgressCheckpoint{owner: st, taskID: task.ID, version: task.MaintenanceVersion, trace: progressTraceSnapshot{}.extend(task.Trace)}
}

// A step can persist an approval/checkpoint itself. In that case the newly
// advanced task version requires the existing full-snapshot path. Custom stores
// also retain the original Store contract.
func SaveTaskProgress(ctx context.Context, st Store, task *types.Task, checkpoint *TaskProgressCheckpoint) error {
	if progress, ok := st.(TaskProgressStore); ok && checkpoint != nil && checkpoint.version == task.MaintenanceVersion {
		return progress.SaveTaskProgress(ctx, task, checkpoint)
	}
	return st.SaveFullTask(ctx, task)
}

func (s *SQLiteStore) SaveTaskProgress(ctx context.Context, task *types.Task, checkpoint *TaskProgressCheckpoint) error {
	if checkpoint != nil && checkpoint.owner != s {
		return ErrTaskProgressConflict
	}
	return s.saveFullTask(ctx, task, false, checkpoint)
}
func (p *PostgresStore) SaveTaskProgress(ctx context.Context, task *types.Task, checkpoint *TaskProgressCheckpoint) error {
	if checkpoint != nil && checkpoint.owner != p {
		return ErrTaskProgressConflict
	}
	return p.saveFullTask(ctx, task, false, checkpoint)
}

func traceProgressOffset(task *types.Task, checkpoint *TaskProgressCheckpoint, previous *taskLifecycle) (int, error) {
	if checkpoint == nil {
		return 0, nil
	}
	if checkpoint.taskID != task.ID || previous == nil || checkpoint.version != task.MaintenanceVersion || checkpoint.version != previous.MaintenanceVersion {
		return 0, ErrTaskProgressConflict
	}
	count := checkpoint.trace.count
	if !checkpoint.trace.matches(task.Trace) {
		return 0, nil
	}
	return count, nil
}

// AdvanceTaskProgress captures an acknowledged save while sharing immutable
// historical blocks with the previous checkpoint. Call only after a successful
// complete save, before further task mutation, just like CaptureTaskProgress.
// Rewrites and version/store changes fall back to a fully detached capture.
func AdvanceTaskProgress(st Store, task *types.Task, previous *TaskProgressCheckpoint) *TaskProgressCheckpoint {
	if task == nil || previous == nil || !sameProgressOwner(st, previous.owner) || previous.taskID != task.ID || previous.version+1 != task.MaintenanceVersion || !previous.trace.matches(task.Trace) {
		return CaptureTaskProgress(st, task)
	}
	if sqlite, ok := st.(*SQLiteStore); ok && sqlite.restoreVersions != nil {
		return nil
	}
	return &TaskProgressCheckpoint{owner: st, taskID: task.ID, version: task.MaintenanceVersion, trace: previous.trace.extend(task.Trace[previous.trace.count:])}
}

func sameProgressOwner(a, b Store) bool {
	// Reuse is limited to the built-in SQL implementations. Other stores retain
	// CaptureTaskProgress's original capability and snapshot contract.
	switch a := a.(type) {
	case *SQLiteStore:
		other, ok := b.(*SQLiteStore)
		return ok && a == other
	case *PostgresStore:
		other, ok := b.(*PostgresStore)
		return ok && a == other
	default:
		return false
	}
}
