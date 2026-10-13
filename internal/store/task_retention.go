package store

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// RetentionObservation contains bounded metadata, never Task/Trace bodies.
// It is diagnostic evidence, not a purge authorization or a content version.
type RetentionObservation struct {
	ID                  string                `json:"task_id"`
	TenantID            string                `json:"tenant_id"`
	SessionID           string                `json:"session_id,omitempty"`
	Status              types.TaskStatus      `json:"status"`
	TerminationKind     types.TerminationKind `json:"termination_kind,omitempty"`
	CreatedAt           time.Time             `json:"created_at"`
	UpdatedAt           time.Time             `json:"updated_at"`
	TerminalSince       *time.Time            `json:"terminal_since,omitempty"`
	LastMaintenanceAt   *time.Time            `json:"last_maintenance_at,omitempty"`
	MaintenanceVersion  int64                 `json:"maintenance_version,omitempty,string"`
	TraceCount          *int                  `json:"trace_count"`
	LeaseActive         *bool                 `json:"lease_active"`
	ApprovalsUnresolved *bool                 `json:"approvals_unresolved"`
}

// TaskRetentionReader must not repair indexes, acquire leases, or write data.
type TaskRetentionReader interface {
	ListTaskRetentionObservations(context.Context, TaskSummaryFilter) ([]RetentionObservation, error)
}

var ErrRetentionReadModelUnavailable = errors.New("retention read model unavailable")

func validateRetentionFilter(f TaskSummaryFilter) error {
	if f.TenantID == "" || f.Limit < 1 || f.Limit > 51 || f.Status != "" || f.SessionID != "" {
		return errors.New("invalid retention filter")
	}
	if (f.BeforeID == "") != f.BeforeCreatedAt.IsZero() {
		return errors.New("invalid retention cursor")
	}
	return nil
}
func (m *MemoryStore) ListTaskRetentionObservations(ctx context.Context, f TaskSummaryFilter) ([]RetentionObservation, error) {
	if err := validateRetentionFilter(f); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	selected := make(summaryPageHeap, 0, f.Limit)
	for _, task := range m.tasks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if task.TenantID != f.TenantID {
			continue
		}
		item := TaskSummary{ID: task.ID, CreatedAt: task.CreatedAt}
		if !summaryBefore(item, f) {
			continue
		}
		if len(selected) < f.Limit {
			heap.Push(&selected, item)
		} else if selected.LessSummary(selected[0], item) {
			selected[0] = item
			heap.Fix(&selected, 0)
		}
	}
	sortSummaries(selected)
	items := make([]RetentionObservation, 0, len(selected))
	positions := map[string]int{}
	now := time.Now()
	for _, item := range selected {
		task := m.tasks[item.ID]
		count := len(task.Trace)
		lease, exists := m.leases[task.ID]
		active := exists && now.Before(lease.expiresAt)
		pending := false
		positions[task.ID] = len(items)
		items = append(items, RetentionObservation{ID: task.ID, TenantID: task.TenantID, SessionID: task.SessionID, Status: task.Status, TerminationKind: task.TerminationKind, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt, TerminalSince: cloneLifecycleTime(task.TerminalSince), LastMaintenanceAt: cloneLifecycleTime(task.LastMaintenanceAt), MaintenanceVersion: task.MaintenanceVersion, TraceCount: &count, LeaseActive: &active, ApprovalsUnresolved: &pending})
	}
	for _, approval := range m.approvals {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if i, ok := positions[approval.TaskID]; ok && approval.Status != types.ApprovalConsumed && approval.Status != types.ApprovalExpired {
			*items[i].ApprovalsUnresolved = true
		}
	}
	return items, ctx.Err()
}
func (s *SQLiteStore) ListTaskRetentionObservations(ctx context.Context, f TaskSummaryFilter) ([]RetentionObservation, error) {
	return sqlRetentionObservations(ctx, s.db, f, false)
}
func (p *PostgresStore) ListTaskRetentionObservations(ctx context.Context, f TaskSummaryFilter) ([]RetentionObservation, error) {
	return sqlRetentionObservations(ctx, p.db, f, true)
}
func sqlRetentionObservations(ctx context.Context, db *sql.DB, f TaskSummaryFilter, postgres bool) ([]RetentionObservation, error) {
	if err := validateRetentionFilter(f); err != nil {
		return nil, err
	}
	marker := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	args := []any{time.Now().UnixNano(), f.TenantID}
	query := `SELECT t.id,t.tenant_id,t.session_id,t.status,t.termination_kind,t.created_at,t.updated_at,t.terminal_since,t.last_maintenance_at,t.maintenance_version,
 (SELECT COUNT(*) FROM traces r WHERE r.task_id=t.id),
 EXISTS(SELECT 1 FROM task_leases l WHERE l.task_id=t.id AND l.expires_at>` + marker(1) + `),
 EXISTS(SELECT 1 FROM approvals a WHERE a.task_id=t.id AND a.status NOT IN ('consumed','expired'))
 FROM tasks t WHERE t.tenant_id=` + marker(2)
	if f.BeforeID != "" {
		query += ` AND (t.created_at,t.id)<(` + marker(3) + `,` + marker(4) + `)`
		args = append(args, f.BeforeCreatedAt, f.BeforeID)
	}
	query += ` ORDER BY t.created_at DESC,t.id DESC LIMIT ` + marker(len(args)+1)
	args = append(args, f.Limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RetentionObservation, 0, f.Limit)
	for rows.Next() {
		var r RetentionObservation
		var terminal, maint sql.NullTime
		var count int
		var lease, pending bool
		if err := rows.Scan(&r.ID, &r.TenantID, &r.SessionID, &r.Status, &r.TerminationKind, &r.CreatedAt, &r.UpdatedAt, &terminal, &maint, &r.MaintenanceVersion, &count, &lease, &pending); err != nil {
			return nil, err
		}
		if terminal.Valid {
			r.TerminalSince = cloneLifecycleTime(&terminal.Time)
		}
		if maint.Valid {
			r.LastMaintenanceAt = cloneLifecycleTime(&maint.Time)
		}
		r.TraceCount = &count
		r.LeaseActive = &lease
		r.ApprovalsUnresolved = &pending
		result = append(result, r)
	}
	return result, rows.Err()
}
