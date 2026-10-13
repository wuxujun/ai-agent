package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// Matches Redis/Lua and JSON's exact integer range across all stores.
const MaxMaintenanceVersion int64 = (1 << 53) - 1

var ErrMaintenanceVersionExhausted = errors.New("task maintenance version exhausted or invalid")

type taskLifecycle struct {
	Status             types.TaskStatus `json:"status"`
	TerminalSince      *time.Time       `json:"terminal_since,omitempty"`
	LastMaintenanceAt  *time.Time       `json:"last_maintenance_at,omitempty"`
	MaintenanceVersion int64            `json:"maintenance_version,string"`
}

func lifecycleOf(task *types.Task) *taskLifecycle {
	if task == nil {
		return nil
	}
	return &taskLifecycle{task.Status, task.TerminalSince, task.LastMaintenanceAt, task.MaintenanceVersion}
}
func nextTaskLifecycle(previous *taskLifecycle, status types.TaskStatus, now time.Time) (taskLifecycle, error) {
	next := taskLifecycle{Status: status, MaintenanceVersion: 1}
	now = now.UTC()
	if previous != nil {
		if previous.MaintenanceVersion < 0 || previous.MaintenanceVersion >= MaxMaintenanceVersion {
			return taskLifecycle{}, ErrMaintenanceVersionExhausted
		}
		next.MaintenanceVersion = previous.MaintenanceVersion + 1
		if previous.MaintenanceVersion > 0 && previous.LastMaintenanceAt != nil && now.Before(*previous.LastMaintenanceAt) {
			now = previous.LastMaintenanceAt.UTC()
		}
		if previous.MaintenanceVersion > 0 && previous.Status == status && types.IsTerminalTaskStatus(status) && previous.TerminalSince != nil {
			at := previous.TerminalSince.UTC()
			next.TerminalSince = &at
		}
	}
	if types.IsTerminalTaskStatus(status) && (previous == nil || previous.Status != status) {
		at := now
		next.TerminalSince = &at
	}
	next.LastMaintenanceAt = &now
	return next, nil
}
func applyTaskLifecycle(task *types.Task, life taskLifecycle) {
	task.TerminalSince = nil
	task.LastMaintenanceAt = nil
	task.MaintenanceVersion = life.MaintenanceVersion
	if life.TerminalSince != nil {
		at := *life.TerminalSince
		task.TerminalSince = &at
	}
	if life.LastMaintenanceAt != nil {
		at := *life.LastMaintenanceAt
		task.LastMaintenanceAt = &at
	}
}
func lifecycleSQLMarker(postgres bool, n int) string {
	if postgres {
		return fmt.Sprintf("$%d", n)
	}
	return fmt.Sprintf("?%d", n)
}
func readTaskLifecycleSQL(ctx context.Context, tx *sql.Tx, id string, postgres bool) (*taskLifecycle, error) {
	if postgres {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ai-agent-task-lifecycle:' || $1,0))`, id); err != nil {
			return nil, err
		}
	}
	query := `SELECT status,terminal_since,last_maintenance_at,maintenance_version FROM tasks WHERE id=` + lifecycleSQLMarker(postgres, 1)
	if postgres {
		query += " FOR UPDATE"
	}
	var life taskLifecycle
	var terminal, maint sql.NullTime
	err := tx.QueryRowContext(ctx, query, id).Scan(&life.Status, &terminal, &maint, &life.MaintenanceVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if terminal.Valid {
		at := terminal.Time.UTC()
		life.TerminalSince = &at
	}
	if maint.Valid {
		at := maint.Time.UTC()
		life.LastMaintenanceAt = &at
	}
	return &life, nil
}
func writeTaskLifecycleSQL(ctx context.Context, tx *sql.Tx, id string, life taskLifecycle, postgres bool) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET terminal_since=`+lifecycleSQLMarker(postgres, 1)+`,last_maintenance_at=`+lifecycleSQLMarker(postgres, 2)+`,maintenance_version=`+lifecycleSQLMarker(postgres, 3)+` WHERE id=`+lifecycleSQLMarker(postgres, 4), life.TerminalSince, life.LastMaintenanceAt, life.MaintenanceVersion, id)
	return err
}

func cloneLifecycleTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	at := value.UTC()
	return &at
}
