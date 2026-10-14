package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

const TaskRestoreLocalObservationSchema = "task-restore-local-observation-v1"

var ErrTaskRestoreLocalObservation = errors.New("invalid local restore observation")

// This interface intentionally does not implement either trusted S01 reader.
// It only observes an already-open, operator-selected dedicated fixture Store.
// It cannot prove historical provenance, dependency edges, monotonic revisions,
// target compatibility or old executor isolation. No CLI/HTTP path selects it.
type TaskRestoreLocalObservationReader interface {
	ReadTaskRestoreLocalObservation(context.Context, TaskRestoreLocalObservationRequest) (*TaskRestoreLocalObservation, error)
}
type TaskRestoreLocalObservationRequest struct {
	TaskID, TenantID   string
	MaxBytes, MaxTasks int
}

type TaskRestoreLocalLease struct {
	Present     bool      `json:"present"`
	OwnerSHA256 string    `json:"owner_sha256,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	// Expiry uses the local observation clock; it is never execution exclusion.
	State string `json:"state"`
}
type TaskRestoreLocalMember struct {
	ID          string                `json:"id"`
	TenantID    string                `json:"tenant_id"`
	SessionID   string                `json:"session_id"`
	SequenceNo  int64                 `json:"sequence_no,string"`
	Status      types.TaskStatus      `json:"status"`
	TaskSHA256  string                `json:"task_sha256"`
	TraceSHA256 string                `json:"trace_sha256"`
	TraceCount  int                   `json:"trace_count"`
	Lease       TaskRestoreLocalLease `json:"lease"`
}
type TaskRestoreLocalObservation struct {
	Schema        string                   `json:"schema"`
	Backend       string                   `json:"backend"`
	TaskID        string                   `json:"task_id"`
	TenantID      string                   `json:"tenant_id"`
	SessionID     string                   `json:"session_id"`
	SessionSHA256 string                   `json:"session_sha256,omitempty"`
	SessionStatus types.SessionStatus      `json:"session_status,omitempty"`
	HighWatermark int64                    `json:"high_watermark,string"`
	Members       []TaskRestoreLocalMember `json:"members"`
	// ContentSHA256 only fingerprints this local observation, not change history.
	ContentSHA256 string   `json:"content_sha256"`
	Status        string   `json:"status"`
	Blockers      []string `json:"blockers"`
}

func validateLocalObservationRequest(q TaskRestoreLocalObservationRequest) error {
	if !validObservationID(q.TaskID) || !validObservationID(q.TenantID) || q.MaxBytes < 4096 || q.MaxBytes > MaxTaskRestoreObservationBytes || q.MaxTasks < 1 || q.MaxTasks > MaxTaskRestoreObservationTasks {
		return ErrTaskRestoreLocalObservation
	}
	return nil
}
func sanitizeLocalObservationError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrTaskExportTooLarge) {
		return err
	}
	return ErrTaskRestoreLocalObservation
}
func newLocalObservation(backend string, q TaskRestoreLocalObservationRequest, session *types.Session) (*TaskRestoreLocalObservation, error) {
	o := &TaskRestoreLocalObservation{Schema: TaskRestoreLocalObservationSchema, Backend: backend, TaskID: q.TaskID, TenantID: q.TenantID, Members: []TaskRestoreLocalMember{}, Status: "unverified", Blockers: []string{"historical_baseline_unavailable", "dependency_graph_unavailable", "monotonic_revision_unavailable", "executor_isolation_unavailable", "target_compatibility_unverified"}}
	if session != nil {
		o.SessionID = session.ID
		o.SessionStatus = session.Status
		o.HighWatermark = session.NextSequence
		raw, err := json.Marshal(TaskExportSession{Session: session, NextSequence: session.NextSequence})
		if err != nil {
			return nil, ErrTaskRestoreLocalObservation
		}
		o.SessionSHA256 = restoreDigest(raw)
	}
	return o, nil
}
func localLease(owner string, expiry time.Time, present bool, now time.Time) (TaskRestoreLocalLease, error) {
	lease := TaskRestoreLocalLease{Present: present, State: "absent"}
	if present {
		if !validObservationID(owner) || expiry.IsZero() {
			return lease, ErrTaskRestoreLocalObservation
		}
		lease.OwnerSHA256 = restoreDigest([]byte(owner))
		lease.ExpiresAt = expiry.UTC()
		lease.State = "expired"
		if expiry.After(now) {
			lease.State = "active"
		}
	}
	return lease, nil
}
func appendLocalMember(ctx context.Context, o *TaskRestoreLocalObservation, snap *TaskExportSnapshot, lease TaskRestoreLocalLease, remaining *int, totalTraces *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if snap == nil || snap.Task == nil || len(snap.Task.Trace) != 0 || !exportCopyFits(snap, *remaining) {
		return ErrTaskExportTooLarge
	}
	task := snap.Task
	if !validObservationID(task.ID) || task.TenantID != o.TenantID || task.SessionID != o.SessionID {
		return ErrTaskRestoreLocalObservation
	}
	switch task.Status {
	case types.StatusCreated, types.StatusRunning, types.StatusAwaitingApproval, types.StatusPaused, types.StatusCompleted, types.StatusPartial, types.StatusFailed:
	default:
		return ErrTaskRestoreLocalObservation
	}
	if (o.SessionID != "" && (task.SequenceNo <= 0 || task.SequenceNo > o.HighWatermark)) || (o.SessionID == "" && task.SequenceNo != 0) {
		return ErrTaskRestoreLocalObservation
	}
	*totalTraces += len(snap.Traces)
	if *totalTraces > MaxTaskExportTraces {
		return ErrTaskExportTooLarge
	}
	for i, tr := range snap.Traces {
		if tr.Sequence != int64(i+1) {
			return ErrTaskRestoreLocalObservation
		}
	}
	taskBytes, err := json.Marshal(task)
	if err != nil {
		return ErrTaskRestoreLocalObservation
	}
	traceBytes, err := json.Marshal(snap.Traces)
	if err != nil {
		return ErrTaskRestoreLocalObservation
	}
	if len(taskBytes)+len(traceBytes) > *remaining {
		return ErrTaskExportTooLarge
	}
	*remaining -= len(taskBytes) + len(traceBytes)
	o.Members = append(o.Members, TaskRestoreLocalMember{ID: task.ID, TenantID: task.TenantID, SessionID: task.SessionID, SequenceNo: task.SequenceNo, Status: task.Status, TaskSHA256: restoreDigest(taskBytes), TraceSHA256: restoreDigest(traceBytes), TraceCount: len(snap.Traces), Lease: lease})
	return nil
}
func finishLocalObservation(ctx context.Context, o *TaskRestoreLocalObservation, q TaskRestoreLocalObservationRequest) (*TaskRestoreLocalObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.SessionID != "" && (!validObservationID(o.SessionID) || o.HighWatermark < 0 || (o.SessionStatus != types.SessionStatusActive && o.SessionStatus != types.SessionStatusArchived)) {
		return nil, ErrTaskRestoreLocalObservation
	}
	sort.Slice(o.Members, func(i, j int) bool { return o.Members[i].ID < o.Members[j].ID })
	seen := map[int64]bool{}
	found := false
	for i, m := range o.Members {
		if i > 0 && m.ID == o.Members[i-1].ID {
			return nil, ErrTaskRestoreLocalObservation
		}
		if o.SessionID != "" {
			if seen[m.SequenceNo] {
				return nil, ErrTaskRestoreLocalObservation
			}
			seen[m.SequenceNo] = true
		}
		if m.ID == q.TaskID {
			found = true
		}
	}
	if !found {
		return nil, sql.ErrNoRows
	}
	raw, err := json.Marshal(o)
	if err != nil || len(raw)+64 > q.MaxBytes {
		return nil, ErrTaskExportTooLarge
	}
	o.ContentSHA256 = restoreDigest(raw)
	return o, nil
}

func (m *MemoryStore) ReadTaskRestoreLocalObservation(ctx context.Context, q TaskRestoreLocalObservationRequest) (observation *TaskRestoreLocalObservation, err error) {
	defer func() { err = sanitizeLocalObservationError(err) }()
	if err = validateLocalObservationRequest(q); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = m.lockRestoreObservation(ctx); err != nil {
		return nil, err
	}
	defer m.mu.RUnlock()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	root := m.tasks[q.TaskID]
	if root == nil || root.TenantID != q.TenantID {
		return nil, sql.ErrNoRows
	}
	var session *types.Session
	if root.SessionID != "" {
		session = m.sessions[root.SessionID]
		if session == nil || session.TenantID != q.TenantID {
			return nil, ErrTaskRestoreLocalObservation
		}
	}
	if session != nil && !exportCopyFits(session, q.MaxBytes) {
		return nil, ErrTaskExportTooLarge
	}
	o, err := newLocalObservation("memory", q, session)
	if err != nil {
		return nil, err
	}
	remaining, totalTraces := q.MaxBytes, 0
	if session != nil {
		raw, _ := json.Marshal(TaskExportSession{Session: session, NextSequence: session.NextSequence})
		remaining -= len(raw)
		if remaining < 0 {
			return nil, ErrTaskExportTooLarge
		}
	}
	// Bound the scan as well as the returned members; no index or page is trusted.
	if len(m.tasks) > 20000 {
		return nil, ErrTaskExportTooLarge
	}
	now := time.Now().UTC()
	for _, task := range m.tasks {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if task == nil {
			return nil, ErrTaskRestoreLocalObservation
		}
		if (root.SessionID != "" && task.SessionID != root.SessionID) || (root.SessionID == "" && task.ID != root.ID) {
			continue
		}
		if len(o.Members) >= q.MaxTasks || len(task.Trace) > MaxTaskExportTraces || !exportCopyFits(task, remaining) {
			return nil, ErrTaskExportTooLarge
		}
		copy := types.CloneTask(task)
		traces := make([]TaskTraceEvent, len(copy.Trace))
		times := m.traceTimes[task.ID]
		if len(times) != len(copy.Trace) {
			return nil, ErrTaskRestoreLocalObservation
		}
		for i, tr := range copy.Trace {
			traces[i] = TaskTraceEvent{Sequence: int64(i + 1), Trace: tr}
			if !times[i].IsZero() {
				at := times[i].UTC()
				traces[i].RecordedAt = &at
			}
		}
		copy.Trace = nil
		storedLease, present := m.leases[task.ID]
		lease, leaseErr := localLease(storedLease.owner, storedLease.expiresAt, present, now)
		if leaseErr != nil {
			return nil, leaseErr
		}
		if err = appendLocalMember(ctx, o, &TaskExportSnapshot{Task: copy, Traces: traces}, lease, &remaining, &totalTraces); err != nil {
			return nil, err
		}
	}
	return finishLocalObservation(ctx, o, q)
}

func (s *SQLiteStore) ReadTaskRestoreLocalObservation(ctx context.Context, q TaskRestoreLocalObservationRequest) (observation *TaskRestoreLocalObservation, err error) {
	defer func() { err = sanitizeLocalObservationError(err) }()
	if err = validateLocalObservationRequest(q); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := readSQLiteLocalObservation(ctx, tx, q, s.getTaskQuery)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func readSQLiteLocalObservation(ctx context.Context, tx *sql.Tx, q TaskRestoreLocalObservationRequest, get func(context.Context, taskSQLReader, string, bool) (*types.Task, error)) (*TaskRestoreLocalObservation, error) {
	var tenant, sessionID string
	err := tx.QueryRowContext(ctx, "SELECT tenant_id,session_id FROM tasks WHERE id=?", q.TaskID).Scan(&tenant, &sessionID)
	if err != nil {
		return nil, err
	}
	if tenant != q.TenantID {
		return nil, sql.ErrNoRows
	}
	var session *types.Session
	if sessionID != "" {
		var size int64
		if err = tx.QueryRowContext(ctx, "SELECT "+exportSQLBytes([]string{"id", "tenant_id", "title", "status"}, false)+" FROM sessions WHERE id=? AND tenant_id=?", sessionID, q.TenantID).Scan(&size); err != nil {
			return nil, err
		}
		if size > int64(q.MaxBytes) {
			return nil, ErrTaskExportTooLarge
		}
		session = &types.Session{}
		if err = tx.QueryRowContext(ctx, "SELECT id,tenant_id,title,status,next_sequence,created_at,updated_at FROM sessions WHERE id=? AND tenant_id=?", sessionID, q.TenantID).Scan(&session.ID, &session.TenantID, &session.Title, &session.Status, &session.NextSequence, &session.CreatedAt, &session.UpdatedAt); err != nil {
			return nil, err
		}
	}
	o, err := newLocalObservation("sqlite", q, session)
	if err != nil {
		return nil, err
	}
	where, args := "id=?", []any{q.TaskID}
	if sessionID != "" {
		where, args = "session_id=?", []any{sessionID}
	}
	var count, maxID int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(length(CAST(id AS BLOB))),0) FROM tasks WHERE "+where, args...).Scan(&count, &maxID); err != nil {
		return nil, err
	}
	if count > q.MaxTasks || maxID > 256 {
		return nil, ErrTaskExportTooLarge
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM tasks WHERE "+where+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	remaining, totalTraces := q.MaxBytes, 0
	if session != nil {
		raw, _ := json.Marshal(TaskExportSession{Session: session, NextSequence: session.NextSequence})
		remaining -= len(raw)
		if remaining < 0 {
			return nil, ErrTaskExportTooLarge
		}
	}
	now := time.Now().UTC()
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		snap, readErr := readSQLTaskExportSnapshot(ctx, tx, id, q.TenantID, remaining, false, get)
		if readErr != nil {
			return nil, readErr
		}
		var owner string
		var expiry int64
		// Bound owner bytes before scanning. Never call the UPDATE-based lease guard.
		var ownerBytes int
		leaseErr := tx.QueryRowContext(ctx, "SELECT length(CAST(owner AS BLOB)) FROM task_leases WHERE task_id=?", id).Scan(&ownerBytes)
		if leaseErr != nil && !errors.Is(leaseErr, sql.ErrNoRows) {
			return nil, leaseErr
		}
		present := leaseErr == nil
		if ownerBytes > 256 {
			return nil, ErrTaskExportTooLarge
		}
		if present {
			if err = tx.QueryRowContext(ctx, "SELECT owner,expires_at FROM task_leases WHERE task_id=?", id).Scan(&owner, &expiry); err != nil {
				return nil, err
			}
		}
		var expires time.Time
		if present {
			expires = time.Unix(0, expiry).UTC()
		}
		lease, leaseErr := localLease(owner, expires, present, now)
		if leaseErr != nil {
			return nil, leaseErr
		}
		if err = appendLocalMember(ctx, o, snap, lease, &remaining, &totalTraces); err != nil {
			return nil, err
		}
	}
	result, err := finishLocalObservation(ctx, o, q)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Unlike an unconditional RLock, contention cannot outlive the observation's
// context. No goroutine waits on a lock after cancellation.
func (m *MemoryStore) lockRestoreObservation(ctx context.Context) error {
	if m.mu.TryRLock() {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if m.mu.TryRLock() {
				return nil
			}
		}
	}
}
