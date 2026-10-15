package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"modernc.org/sqlite"
)

var ErrSQLiteExecutionGate = errors.New("sqlite execution gate invalid")
var ErrSQLiteExecutionBlocked = errors.New("sqlite execution dispatch blocked")
var ErrSQLiteExecutionConflict = errors.New("sqlite execution attempt conflict")
var sqliteExecutionClockRegistrationError error

func init() {
	sqliteExecutionClockRegistrationError = sqlite.RegisterScalarFunction("ai_agent_execution_now_ns_v1", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		return time.Now().UnixNano(), nil
	})
}

type sqliteExecutionGate struct{}

type SQLiteExecutionAttempt struct {
	Origin          string `json:"origin"`
	NamespaceSHA256 string `json:"namespace_sha256"`
	ID              string `json:"id"`
	TaskID          string `json:"task_id"`
	TenantID        string `json:"tenant_id"`
	InstanceSHA256  string `json:"instance_sha256"`
	TaskIncarnation int64  `json:"task_incarnation"`
	GateEpoch       int64  `json:"gate_epoch"`
	State           string `json:"state"`
	ExitSHA256      string `json:"exit_sha256,omitempty"`
	FlushSHA256     string `json:"flush_sha256,omitempty"`
	CleanupSHA256   string `json:"cleanup_sha256,omitempty"`
}

type SQLiteExecutionGateSnapshot struct {
	Status           string                   `json:"status"`
	TaskID           string                   `json:"task_id"`
	TenantID         string                   `json:"tenant_id"`
	TaskIncarnation  int64                    `json:"task_incarnation"`
	Phase            string                   `json:"phase"`
	GateEpoch        int64                    `json:"gate_epoch"`
	ControlEpoch     int64                    `json:"control_epoch"`
	LeaseState       string                   `json:"lease_state"`
	Attempts         []SQLiteExecutionAttempt `json:"attempts"`
	ReceiptsComplete bool                     `json:"receipts_complete"`
	Gaps             []string                 `json:"gaps"`
}

// NewSQLiteStoreWithExecutionGate creates a NEW dedicated C03 fixture, not a
// production store. Core row versions remain active. Gates cannot be reopened
// for writing, automatically released, or inferred from cancellation/lease TTL.
func NewSQLiteStoreWithExecutionGate(path, namespaceSHA256 string) (*SQLiteStore, error) {
	if sqliteExecutionClockRegistrationError != nil {
		return nil, ErrSQLiteExecutionGate
	}
	s, err := NewSQLiteStoreWithRestoreVersions(path, namespaceSHA256)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	defer tx.Rollback()
	schema := `CREATE TABLE execution_control(singleton INTEGER PRIMARY KEY CHECK(singleton=1),epoch INTEGER NOT NULL CHECK(epoch>=0 AND epoch<=8192));
INSERT INTO execution_control VALUES(1,0);
CREATE TABLE execution_gates(task_id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,task_incarnation INTEGER NOT NULL,phase TEXT NOT NULL CHECK(phase IN ('open','blocked','draining')),epoch INTEGER NOT NULL);
CREATE TABLE execution_attempts(id TEXT PRIMARY KEY,task_id TEXT NOT NULL,tenant_id TEXT NOT NULL,instance_sha256 TEXT NOT NULL,task_incarnation INTEGER NOT NULL,gate_epoch INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN ('running','exited','complete')),exit_sha256 TEXT NOT NULL DEFAULT '',flush_sha256 TEXT NOT NULL DEFAULT '',cleanup_sha256 TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX execution_single_active ON execution_attempts(task_id) WHERE state!='complete';
CREATE TABLE execution_events(epoch INTEGER PRIMARY KEY,task_id TEXT NOT NULL,attempt_id TEXT NOT NULL,operation TEXT NOT NULL);
CREATE TRIGGER execution_no_open AFTER UPDATE OF phase ON execution_gates WHEN OLD.phase!='open' AND NEW.phase='open' BEGIN SELECT RAISE(ABORT,'execution gate cannot auto-open'); END;
CREATE TRIGGER execution_no_gate_delete BEFORE DELETE ON execution_gates BEGIN SELECT RAISE(ABORT,'execution gate history retained'); END;
CREATE TRIGGER execution_no_attempt_delete BEFORE DELETE ON execution_attempts BEGIN SELECT RAISE(ABORT,'execution attempt history retained'); END;
CREATE TRIGGER execution_no_live_task_delete BEFORE DELETE ON tasks WHEN EXISTS(SELECT 1 FROM execution_attempts WHERE task_id=OLD.id AND state!='complete') BEGIN SELECT RAISE(ABORT,'execution attempt unresolved'); END;
CREATE TRIGGER execution_lease_delete BEFORE DELETE ON task_leases WHEN EXISTS(SELECT 1 FROM execution_attempts WHERE task_id=OLD.task_id AND state!='complete') BEGIN SELECT RAISE(ABORT,'execution receipts incomplete'); END;`
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	for _, operation := range []string{"INSERT", "UPDATE"} {
		body := `SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM execution_gates g JOIN tasks t ON t.id=g.task_id JOIN restore_row_versions v ON v.kind='task' AND v.id=t.id JOIN execution_attempts a ON a.task_id=g.task_id WHERE g.task_id=NEW.task_id AND g.tenant_id=t.tenant_id AND g.task_incarnation=v.incarnation AND v.deleted=0 AND a.task_incarnation=g.task_incarnation AND a.tenant_id=g.tenant_id AND a.state IN ('running','exited') AND NEW.owner='execution-attempt:'||a.id) THEN RAISE(ABORT,'execution lease requires registered attempt') END;`
		if operation == "INSERT" {
			body += `SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM execution_gates WHERE task_id=NEW.task_id AND phase='open') THEN RAISE(ABORT,'execution dispatch blocked') END;`
		} else {
			body += `SELECT CASE WHEN NEW.owner IS NOT OLD.owner OR (NEW.expires_at IS NOT OLD.expires_at AND OLD.expires_at<=ai_agent_execution_now_ns_v1()) THEN RAISE(ABORT,'execution ownership cannot revive') END;`
		}
		if _, err := tx.ExecContext(ctx, `CREATE TRIGGER execution_lease_`+operation+` BEFORE `+operation+` ON task_leases BEGIN `+body+` END;`); err != nil {
			return nil, ErrSQLiteExecutionGate
		}
	}
	s.restoreVersions.schema, err = sqliteRestoreVersionSchema(ctx, tx)
	if err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	if err := tx.Commit(); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	s.executionGate = &sqliteExecutionGate{}
	ok = true
	return s, nil
}

func (s *SQLiteStore) executionGateTx(ctx context.Context) (*sql.Tx, error) {
	if s.executionGate == nil || s.restoreVersions == nil {
		return nil, ErrSQLiteExecutionGate
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.restoreVersions.checkFile(); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, executionGateError(ctx, err)
	}
	good := false
	defer func() {
		if !good {
			tx.Rollback()
		}
	}()
	// Serialize control and business lease changes before checking the gate/time.
	result, err := tx.ExecContext(ctx, `UPDATE execution_control SET epoch=epoch WHERE singleton=1`)
	if err != nil {
		return nil, executionGateError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return nil, ErrSQLiteExecutionGate
	}
	if err := executionControlValidate(ctx, tx); err != nil {
		return nil, executionGateError(ctx, err)
	}
	if _, err := s.restoreVersions.read(ctx, tx); err != nil {
		return nil, executionGateError(ctx, err)
	}
	good = true
	return tx, nil
}
func executionGateError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrSQLiteExecutionBlocked) || errors.Is(err, ErrSQLiteExecutionConflict) {
		return err
	}
	return ErrSQLiteExecutionGate
}
func executionGateScope(taskID, tenantID string) bool {
	return validObservationID(taskID) && validObservationID(tenantID)
}
func executionControlEvent(ctx context.Context, tx *sql.Tx, taskID, attemptID, operation string) (int64, error) {
	var epoch int64
	if err := tx.QueryRowContext(ctx, `UPDATE execution_control SET epoch=epoch+1 WHERE singleton=1 RETURNING epoch`).Scan(&epoch); err != nil {
		return 0, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO execution_events VALUES(?,?,?,?)`, epoch, taskID, attemptID, operation)
	return epoch, err
}

func (s *SQLiteStore) EnrollSQLiteExecutionGate(ctx context.Context, taskID, tenantID string) (int64, error) {
	if !executionGateScope(taskID, tenantID) {
		return 0, ErrSQLiteExecutionGate
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.executionGateTx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var incarnation int64
	if err := tx.QueryRowContext(ctx, `SELECT v.incarnation FROM tasks t JOIN restore_row_versions v ON v.kind='task' AND v.id=t.id WHERE t.id=? AND t.tenant_id=? AND v.deleted=0`, taskID, tenantID).Scan(&incarnation); err != nil {
		return 0, executionGateError(ctx, err)
	}
	var existing, leases, attempts int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM execution_gates WHERE task_id=?),(SELECT count(*) FROM task_leases WHERE task_id=?),(SELECT count(*) FROM execution_attempts WHERE task_id=?)`, taskID, taskID, taskID).Scan(&existing, &leases, &attempts); err != nil {
		return 0, ErrSQLiteExecutionGate
	}
	if existing != 0 || leases != 0 || attempts != 0 {
		return 0, ErrSQLiteExecutionConflict
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_gates`).Scan(&count); err != nil || count >= 256 {
		return 0, ErrSQLiteExecutionGate
	}
	epoch, err := executionControlEvent(ctx, tx, taskID, "", "enroll")
	if err != nil {
		return 0, ErrSQLiteExecutionGate
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_gates VALUES(?,?,?,'open',?)`, taskID, tenantID, incarnation, epoch); err != nil {
		return 0, ErrSQLiteExecutionGate
	}
	if err := tx.Commit(); err != nil {
		return 0, executionGateError(ctx, err)
	}
	return epoch, nil
}

// StartSQLiteExecutionAttempt atomically checks an enrolled open gate, registers
// the unique attempt and acquires its lease. Used IDs are permanently rejected;
// reading an existing attempt after an ambiguous response is not permission to
// execute it again. TTL expiry never retires a prior unresolved attempt.
func (s *SQLiteStore) StartSQLiteExecutionAttempt(ctx context.Context, taskID, tenantID, id, instanceSHA256 string, expectedGateEpoch int64, ttl time.Duration) (*SQLiteExecutionAttempt, error) {
	if !executionGateScope(taskID, tenantID) || !validObservationID(id) || !sqliteRestoreContentValidDigest(instanceSHA256) || expectedGateEpoch < 1 || ttl <= 0 || ttl > time.Hour {
		return nil, ErrSQLiteExecutionGate
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.executionGateTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var phase string
	var incarnation, gateEpoch int64
	err = tx.QueryRowContext(ctx, `SELECT g.phase,g.task_incarnation,g.epoch FROM execution_gates g JOIN tasks t ON t.id=g.task_id JOIN restore_row_versions v ON v.kind='task' AND v.id=t.id WHERE g.task_id=? AND g.tenant_id=? AND t.tenant_id=g.tenant_id AND v.deleted=0 AND v.incarnation=g.task_incarnation`, taskID, tenantID).Scan(&phase, &incarnation, &gateEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSQLiteExecutionBlocked
	}
	if err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	if phase != "open" {
		return nil, ErrSQLiteExecutionBlocked
	}
	if gateEpoch != expectedGateEpoch {
		return nil, ErrSQLiteExecutionConflict
	}
	var conflicts, total int
	err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM execution_attempts WHERE id=? OR (task_id=? AND state!='complete')),(SELECT count(*) FROM execution_attempts)`, id, taskID).Scan(&conflicts, &total)
	if err != nil || total >= 1024 {
		return nil, ErrSQLiteExecutionGate
	}
	if conflicts > 0 {
		return nil, ErrSQLiteExecutionConflict
	}
	var leaseCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM task_leases WHERE task_id=?`, taskID).Scan(&leaseCount); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	if leaseCount != 0 {
		return nil, ErrSQLiteExecutionConflict
	}
	attempt := &SQLiteExecutionAttempt{Origin: s.restoreVersions.origin, NamespaceSHA256: s.restoreVersions.namespace, ID: id, TaskID: taskID, TenantID: tenantID, InstanceSHA256: instanceSHA256, TaskIncarnation: incarnation, GateEpoch: gateEpoch, State: "running"}
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_attempts(id,task_id,tenant_id,instance_sha256,task_incarnation,gate_epoch,state) VALUES(?,?,?,?,?,?,'running')`, id, taskID, tenantID, instanceSHA256, incarnation, gateEpoch); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	// Time is read after the control write lock, so waiting cannot extend an old
	// ownership window. The lease insert is protected even for direct SQL paths.
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_leases VALUES(?,?,?)`, taskID, "execution-attempt:"+id, time.Now().Add(ttl).UnixNano()); err != nil {
		return nil, executionGateError(ctx, err)
	}
	if _, err := executionControlEvent(ctx, tx, taskID, id, "start"); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	if err := tx.Commit(); err != nil {
		return nil, executionGateError(ctx, err)
	}
	return attempt, nil
}

// ExitSQLiteExecutionAttempt records a caller-supplied protocol receipt. This
// fixture does not authenticate the process/host or cancel any running code.
func (s *SQLiteStore) ExitSQLiteExecutionAttempt(ctx context.Context, taskID, tenantID, id, instanceSHA256, exitSHA256 string) error {
	return s.recordSQLiteExecutionReceipt(ctx, taskID, tenantID, id, instanceSHA256, exitSHA256, "", "")
}
func (s *SQLiteStore) FinalizeSQLiteExecutionAttempt(ctx context.Context, taskID, tenantID, id, instanceSHA256, flushSHA256, cleanupSHA256 string) error {
	return s.recordSQLiteExecutionReceipt(ctx, taskID, tenantID, id, instanceSHA256, "", flushSHA256, cleanupSHA256)
}
func (s *SQLiteStore) recordSQLiteExecutionReceipt(ctx context.Context, taskID, tenantID, id, instanceSHA256, exit, flush, cleanup string) error {
	if !executionGateScope(taskID, tenantID) || !validObservationID(id) || !sqliteRestoreContentValidDigest(instanceSHA256) {
		return ErrSQLiteExecutionGate
	}
	exiting := exit != ""
	if exiting {
		if !sqliteRestoreContentValidDigest(exit) || flush != "" || cleanup != "" {
			return ErrSQLiteExecutionGate
		}
	} else if !sqliteRestoreContentValidDigest(flush) || !sqliteRestoreContentValidDigest(cleanup) {
		return ErrSQLiteExecutionGate
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.executionGateTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempt SQLiteExecutionAttempt
	err = tx.QueryRowContext(ctx, `SELECT state,exit_sha256,flush_sha256,cleanup_sha256 FROM execution_attempts WHERE id=? AND task_id=? AND tenant_id=? AND instance_sha256=?`, id, taskID, tenantID, instanceSHA256).Scan(&attempt.State, &attempt.ExitSHA256, &attempt.FlushSHA256, &attempt.CleanupSHA256)
	if err != nil {
		return ErrSQLiteExecutionConflict
	}
	if exiting && attempt.State != "running" {
		if attempt.ExitSHA256 == exit {
			return nil
		}
		return ErrSQLiteExecutionConflict
	}
	if !exiting && attempt.State == "complete" {
		if attempt.FlushSHA256 == flush && attempt.CleanupSHA256 == cleanup {
			return nil
		}
		return ErrSQLiteExecutionConflict
	}
	operation := "exit"
	if exiting {
		_, err = tx.ExecContext(ctx, `UPDATE execution_attempts SET state='exited',exit_sha256=? WHERE id=?`, exit, id)
	} else {
		if attempt.State != "exited" || !sqliteRestoreContentValidDigest(attempt.ExitSHA256) {
			return ErrSQLiteExecutionConflict
		}
		currentOwner, expiresAt, leaseErr := lockTaskLeaseSQL(ctx, tx, taskID, false)
		if leaseErr != nil {
			return executionGateError(ctx, leaseErr)
		}
		if currentOwner != "execution-attempt:"+id || expiresAt <= time.Now().UnixNano() {
			return ErrTaskLeaseLost
		}
		operation = "finalize"
		_, err = tx.ExecContext(ctx, `UPDATE execution_attempts SET state='complete',flush_sha256=?,cleanup_sha256=? WHERE id=?`, flush, cleanup, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM task_leases WHERE task_id=? AND owner=?`, taskID, "execution-attempt:"+id)
		}
	}
	if err != nil {
		return executionGateError(ctx, err)
	}
	if _, err := executionControlEvent(ctx, tx, taskID, id, operation); err != nil {
		return ErrSQLiteExecutionGate
	}
	return executionGateErrorOrNil(ctx, tx.Commit())
}
func executionGateErrorOrNil(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	return executionGateError(ctx, err)
}

// BlockSQLiteExecutionGate is a one-way CAS. "draining" is a protocol state,
// not a cancellation action. Existing attempts remain until receipts complete.
func (s *SQLiteStore) BlockSQLiteExecutionGate(ctx context.Context, taskID, tenantID string, expectedEpoch int64, draining bool) (int64, error) {
	if !executionGateScope(taskID, tenantID) || expectedEpoch < 1 {
		return 0, ErrSQLiteExecutionGate
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.executionGateTx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var phase string
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT phase,epoch FROM execution_gates WHERE task_id=? AND tenant_id=?`, taskID, tenantID).Scan(&phase, &epoch); err != nil {
		return 0, ErrSQLiteExecutionGate
	}
	if epoch != expectedEpoch {
		return 0, ErrSQLiteExecutionConflict
	}
	next := "blocked"
	if draining {
		next = "draining"
	}
	if phase == next {
		return epoch, nil
	}
	if phase == "draining" && next == "blocked" {
		return 0, ErrSQLiteExecutionConflict
	}
	newEpoch, err := executionControlEvent(ctx, tx, taskID, "", next)
	if err != nil {
		return 0, ErrSQLiteExecutionGate
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_gates SET phase=?,epoch=? WHERE task_id=?`, next, newEpoch, taskID); err != nil {
		return 0, ErrSQLiteExecutionGate
	}
	if err := tx.Commit(); err != nil {
		return 0, executionGateError(ctx, err)
	}
	return newEpoch, nil
}

func (s *SQLiteStore) ReadSQLiteExecutionGateSnapshot(ctx context.Context, taskID, tenantID string) (*SQLiteExecutionGateSnapshot, error) {
	if s.executionGate == nil || !executionGateScope(taskID, tenantID) {
		return nil, ErrSQLiteExecutionGate
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.restoreVersions.checkFile(); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, executionGateError(ctx, err)
	}
	defer tx.Rollback()
	if err := executionControlValidate(ctx, tx); err != nil {
		return nil, executionGateError(ctx, err)
	}
	if _, err := s.restoreVersions.read(ctx, tx); err != nil {
		return nil, executionGateError(ctx, err)
	}
	out := &SQLiteExecutionGateSnapshot{Status: "unverified", TaskID: taskID, TenantID: tenantID, Attempts: []SQLiteExecutionAttempt{}, Gaps: []string{"single_task_fixture_only", "runtime_entrypoints_not_integrated", "instance_receipts_not_authenticated", "host_cleanup_not_reconciled", "independent_control_floor_unavailable"}}
	err = tx.QueryRowContext(ctx, `SELECT g.task_incarnation,g.phase,g.epoch FROM execution_gates g JOIN tasks t ON t.id=g.task_id JOIN restore_row_versions v ON v.kind='task' AND v.id=t.id WHERE g.task_id=? AND g.tenant_id=? AND t.tenant_id=g.tenant_id AND v.incarnation=g.task_incarnation AND v.deleted=0`, taskID, tenantID).Scan(&out.TaskIncarnation, &out.Phase, &out.GateEpoch)
	if err != nil {
		return nil, executionGateError(ctx, err)
	}
	var count int
	var maxEpoch, minEpoch int64
	if err := tx.QueryRowContext(ctx, `SELECT epoch FROM execution_control WHERE singleton=1`).Scan(&out.ControlEpoch); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(epoch),0),coalesce(max(epoch),0) FROM execution_events`).Scan(&count, &minEpoch, &maxEpoch); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	if out.ControlEpoch < 1 || out.ControlEpoch > 8192 || int64(count) != out.ControlEpoch || minEpoch != 1 || maxEpoch != out.ControlEpoch || out.GateEpoch < 1 || out.GateEpoch > out.ControlEpoch || (out.Phase != "open" && out.Phase != "blocked" && out.Phase != "draining") {
		return nil, ErrSQLiteExecutionGate
	}
	var bad int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_events WHERE operation NOT IN ('enroll','start','exit','finalize','blocked','draining') OR length(CAST(task_id AS BLOB))>128 OR length(CAST(attempt_id AS BLOB))>128`).Scan(&bad); err != nil || bad != 0 {
		return nil, ErrSQLiteExecutionGate
	}
	var maxBytes int
	err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(max(length(CAST(id AS BLOB))+length(CAST(task_id AS BLOB))+length(CAST(tenant_id AS BLOB))+length(instance_sha256)+length(exit_sha256)+length(flush_sha256)+length(cleanup_sha256)),0) FROM execution_attempts`).Scan(&count, &maxBytes)
	if err != nil || count > 1024 || maxBytes > 1024 {
		return nil, ErrSQLiteExecutionGate
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,task_id,tenant_id,instance_sha256,task_incarnation,gate_epoch,state,exit_sha256,flush_sha256,cleanup_sha256 FROM execution_attempts WHERE task_id=? ORDER BY id LIMIT 1025`, taskID)
	if err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	unresolved := 0
	for rows.Next() {
		var a SQLiteExecutionAttempt
		a.Origin = s.restoreVersions.origin
		a.NamespaceSHA256 = s.restoreVersions.namespace
		if err := rows.Scan(&a.ID, &a.TaskID, &a.TenantID, &a.InstanceSHA256, &a.TaskIncarnation, &a.GateEpoch, &a.State, &a.ExitSHA256, &a.FlushSHA256, &a.CleanupSHA256); err != nil {
			rows.Close()
			return nil, ErrSQLiteExecutionGate
		}
		if !validObservationID(a.ID) || a.TenantID != tenantID || a.TaskIncarnation != out.TaskIncarnation || a.GateEpoch < 1 || a.GateEpoch > out.GateEpoch || !sqliteRestoreContentValidDigest(a.InstanceSHA256) {
			rows.Close()
			return nil, ErrSQLiteExecutionGate
		}
		switch a.State {
		case "running":
			if a.ExitSHA256 != "" || a.FlushSHA256 != "" || a.CleanupSHA256 != "" {
				rows.Close()
				return nil, ErrSQLiteExecutionGate
			}
			unresolved++
		case "exited":
			if !sqliteRestoreContentValidDigest(a.ExitSHA256) || a.FlushSHA256 != "" || a.CleanupSHA256 != "" {
				rows.Close()
				return nil, ErrSQLiteExecutionGate
			}
			unresolved++
		case "complete":
			if !sqliteRestoreContentValidDigest(a.ExitSHA256) || !sqliteRestoreContentValidDigest(a.FlushSHA256) || !sqliteRestoreContentValidDigest(a.CleanupSHA256) {
				rows.Close()
				return nil, ErrSQLiteExecutionGate
			}
		default:
			rows.Close()
			return nil, ErrSQLiteExecutionGate
		}
		out.Attempts = append(out.Attempts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || unresolved > 1 {
		return nil, ErrSQLiteExecutionGate
	}
	for _, a := range out.Attempts {
		var starts int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_events WHERE operation='start' AND task_id=? AND attempt_id=?`, taskID, a.ID).Scan(&starts); err != nil || starts != 1 {
			return nil, ErrSQLiteExecutionGate
		}
	}
	var owner string
	var expiry int64
	err = tx.QueryRowContext(ctx, `SELECT owner,expires_at FROM task_leases WHERE task_id=?`, taskID).Scan(&owner, &expiry)
	out.LeaseState = "absent"
	if err == nil {
		validOwner := false
		for _, a := range out.Attempts {
			if a.State != "complete" && owner == "execution-attempt:"+a.ID {
				validOwner = true
			}
		}
		if !validOwner {
			return nil, ErrSQLiteExecutionGate
		}
		out.LeaseState = "active-unresolved"
		if expiry <= time.Now().UnixNano() {
			out.LeaseState = "expired-unresolved"
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSQLiteExecutionGate
	}
	out.ReceiptsComplete = out.Phase != "open" && unresolved == 0 && out.LeaseState == "absent"
	if err := tx.Commit(); err != nil {
		return nil, executionGateError(ctx, err)
	}
	if err := s.restoreVersions.checkFile(); err != nil {
		return nil, ErrSQLiteExecutionGate
	}
	return out, nil
}

type SQLiteExecutionGateReader struct{ reader *SQLiteRestoreVersionReader }

func OpenSQLiteExecutionGateReader(path string, pin SQLiteRestoreVersionPin) (*SQLiteExecutionGateReader, error) {
	reader, err := OpenSQLiteRestoreVersionReader(path, pin)
	if err != nil {
		return nil, err
	}
	reader.store.executionGate = &sqliteExecutionGate{}
	// Require actual control tables in the immutable schema; scope-specific
	// content checks are performed by each subsequent snapshot.
	var count int
	if err := reader.store.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('execution_control','execution_gates','execution_attempts','execution_events')`).Scan(&count); err != nil || count != 4 {
		reader.Close()
		return nil, ErrSQLiteExecutionGate
	}
	return &SQLiteExecutionGateReader{reader: reader}, nil
}
func (r *SQLiteExecutionGateReader) Close() error { return r.reader.Close() }
func (r *SQLiteExecutionGateReader) ReadSQLiteExecutionGateSnapshot(ctx context.Context, taskID, tenantID string) (*SQLiteExecutionGateSnapshot, error) {
	if _, err := r.reader.ReadSQLiteRestoreVersions(ctx); err != nil {
		return nil, err
	}
	return r.reader.store.ReadSQLiteExecutionGateSnapshot(ctx, taskID, tenantID)
}

func executionControlValidate(ctx context.Context, tx *sql.Tx) error {
	var rows, count int
	var epoch, minimum, maximum int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(max(epoch),0) FROM execution_control WHERE singleton=1`).Scan(&rows, &epoch); err != nil || rows != 1 || epoch < 0 || epoch > 8192 {
		return ErrSQLiteExecutionGate
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(epoch),0),coalesce(max(epoch),0) FROM execution_events`).Scan(&count, &minimum, &maximum); err != nil || int64(count) != epoch || maximum != epoch || (epoch > 0 && minimum != 1) {
		return ErrSQLiteExecutionGate
	}
	var bad int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_events WHERE operation NOT IN ('enroll','start','exit','finalize','blocked','draining') OR length(CAST(task_id AS BLOB))>128 OR length(CAST(attempt_id AS BLOB))>128`).Scan(&bad); err != nil || bad != 0 {
		return ErrSQLiteExecutionGate
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_gates`).Scan(&count); err != nil || count > 256 {
		return ErrSQLiteExecutionGate
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_attempts`).Scan(&count); err != nil || count > 1024 {
		return ErrSQLiteExecutionGate
	}
	// Materialized phases/states must correspond to recorded control actions.
	// This is local integrity only, not authentication of those receipts.
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_gates g WHERE NOT EXISTS(SELECT 1 FROM execution_events e WHERE e.epoch=g.epoch AND e.task_id=g.task_id AND e.attempt_id='' AND e.operation=CASE WHEN g.phase='open' THEN 'enroll' ELSE g.phase END)`).Scan(&bad); err != nil || bad != 0 {
		return ErrSQLiteExecutionGate
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_attempts a WHERE NOT EXISTS(SELECT 1 FROM execution_events e WHERE e.task_id=a.task_id AND e.attempt_id=a.id AND e.epoch=(SELECT max(epoch) FROM execution_events WHERE attempt_id=a.id) AND e.operation=CASE a.state WHEN 'running' THEN 'start' WHEN 'exited' THEN 'exit' WHEN 'complete' THEN 'finalize' ELSE '' END)`).Scan(&bad); err != nil || bad != 0 {
		return ErrSQLiteExecutionGate
	}
	return nil
}
