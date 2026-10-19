package store

import (
	"context"
	"errors"
	"reflect"

	"github.com/wuxujun/ai-agent/internal/types"
)

var ErrTaskProgressConflict = errors.New("task progress checkpoint is stale")

// TaskProgressCheckpoint is a sealed, detached copy of the previously observed
// prefix. It is only an optimization hint; a transaction must verify its version.
type TaskProgressCheckpoint struct {
	owner   Store
	taskID  string
	version int64
	trace   []types.StepTrace
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
	detached := types.CloneTask(&types.Task{Trace: task.Trace})
	// CloneTask normalizes nil slices for its public snapshot contract. Preserve
	// the exact representation here so an untouched prefix compares equal.
	for i, item := range task.Trace {
		if item.Evidence == nil {
			detached.Trace[i].Evidence = nil
		}
		for j, evidence := range item.Evidence {
			if evidence.Lines != nil && len(evidence.Lines) == 0 {
				detached.Trace[i].Evidence[j].Lines = []string{}
			}
		}
	}
	return &TaskProgressCheckpoint{owner: st, taskID: task.ID, version: task.MaintenanceVersion, trace: detached.Trace}
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
	count := len(checkpoint.trace)
	if count > len(task.Trace) || !reflect.DeepEqual(checkpoint.trace, task.Trace[:count]) {
		return 0, nil
	}
	return count, nil
}
