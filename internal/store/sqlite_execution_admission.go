package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

type sqliteExecutionAttemptContextKey struct{}

// WithSQLiteExecutionAttempt binds a copy of an issued prototype attempt. The
// context is not an authenticated credential: every execution check rechecks
// the durable attempt/gate/incarnation/lease. No receipt is produced here.
func WithSQLiteExecutionAttempt(ctx context.Context, attempt *SQLiteExecutionAttempt) context.Context {
	if attempt == nil {
		return context.WithValue(ctx, sqliteExecutionAttemptContextKey{}, SQLiteExecutionAttempt{})
	}
	ctx = WithTaskLease(ctx, attempt.TaskID, "execution-attempt:"+attempt.ID)
	return context.WithValue(ctx, sqliteExecutionAttemptContextKey{}, *attempt)
}

// CheckExecutionAdmission implements the optional executor policy for C03
// fixtures. It never enrolls, starts, renews, exits or finalizes an attempt.
func (s *SQLiteStore) CheckExecutionAdmission(ctx context.Context, task *types.Task) error {
	if s.executionGate == nil || task == nil {
		return types.ErrExecutionAdmission
	}
	attempt, ok := ctx.Value(sqliteExecutionAttemptContextKey{}).(SQLiteExecutionAttempt)
	if !ok || attempt.Origin != s.restoreVersions.origin || attempt.NamespaceSHA256 != s.restoreVersions.namespace || attempt.TaskID != task.ID || attempt.TenantID != task.TenantID || !validObservationID(attempt.ID) || !sqliteRestoreContentValidDigest(attempt.InstanceSHA256) {
		return types.ErrExecutionAdmission
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.restoreVersions.checkFile(); err != nil {
		return types.ErrExecutionAdmission
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return executionGateError(ctx, err)
	}
	defer tx.Rollback()
	if err := executionControlValidate(ctx, tx); err != nil {
		return executionGateError(ctx, err)
	}
	if _, err := s.restoreVersions.read(ctx, tx); err != nil {
		return executionGateError(ctx, err)
	}
	var phase, state, owner string
	var gateEpoch, incarnation, expiry int64
	err = tx.QueryRowContext(ctx, `SELECT g.phase,g.epoch,g.task_incarnation,a.state,l.owner,l.expires_at FROM execution_gates g JOIN execution_attempts a ON a.task_id=g.task_id JOIN tasks t ON t.id=g.task_id JOIN restore_row_versions v ON v.kind='task' AND v.id=t.id JOIN task_leases l ON l.task_id=t.id WHERE a.id=? AND a.instance_sha256=? AND a.tenant_id=? AND g.tenant_id=a.tenant_id AND t.tenant_id=a.tenant_id AND a.task_id=? AND a.task_incarnation=g.task_incarnation AND v.incarnation=g.task_incarnation AND v.deleted=0 AND a.gate_epoch=?`, attempt.ID, attempt.InstanceSHA256, task.TenantID, task.ID, attempt.GateEpoch).Scan(&phase, &gateEpoch, &incarnation, &state, &owner, &expiry)
	if err != nil {
		return types.ErrExecutionAdmission
	}
	if phase != "open" || gateEpoch != attempt.GateEpoch || incarnation != attempt.TaskIncarnation || state != "running" || owner != "execution-attempt:"+attempt.ID || expiry <= time.Now().UnixNano() {
		return types.ErrExecutionAdmission
	}
	if err := tx.Commit(); err != nil {
		return executionGateError(ctx, err)
	}
	return nil
}

var _ types.ExecutionAdmissionChecker = (*SQLiteStore)(nil)
