package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

const MaxTaskExportBytes = 8 << 20
const MaxTaskExportTraces = 10000

var ErrTaskExportTooLarge = errors.New("task export exceeds budget")
var ErrTaskExportUnavailable = errors.New("task export snapshot unavailable")

// TaskExportReader returns one read-only observation of Task and physical Trace
// times. It must not repair indexes, acquire leases, or combine separate reads.
type TaskExportReader interface {
	ReadTaskExport(context.Context, string, string, int) (*TaskExportSnapshot, error)
}
type TaskJointExportReader interface {
	ReadJointTaskExport(context.Context, string, string, int) (*TaskExportSnapshot, error)
}

type TaskExportSnapshot struct {
	Dependencies *TaskExportDependencies `json:"dependencies,omitempty"`
	Task         *types.Task             `json:"task"`
	Traces       []TaskTraceEvent        `json:"traces"`
}
type TaskExportArtifact struct {
	Schema          string          `json:"schema"`
	SourceBackend   string          `json:"source_backend"`
	ExportedAt      time.Time       `json:"exported_at"`
	SourcePreserved bool            `json:"source_preserved"`
	RestoreReady    bool            `json:"restore_ready"`
	Included        []string        `json:"included"`
	Excluded        []string        `json:"excluded"`
	PayloadSHA256   string          `json:"payload_sha256"`
	PayloadBytes    int             `json:"payload_bytes"`
	TraceCount      int             `json:"trace_count"`
	ApprovalCount   *int            `json:"approval_count,omitempty"`
	MemoryCount     *int            `json:"memory_count,omitempty"`
	Payload         json.RawMessage `json:"payload"`
}
type taskSQLReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func validateExportRequest(id, tenant string, budget int) error {
	if id == "" || tenant == "" || budget < 4096 || budget > MaxTaskExportBytes {
		return errors.New("invalid export request")
	}
	return nil
}

// A preflight bounds copies before serialization. It is a conservative working
// set guard, not a replacement for the final encoded response budget.
func exportCopyFits(value any, budget int) bool {
	remaining := budget
	var walk func(reflect.Value) bool
	walk = func(v reflect.Value) bool {
		remaining -= 8
		if remaining < 0 {
			return false
		}
		if !v.IsValid() {
			return true
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if !v.IsNil() {
				return walk(v.Elem())
			}
		case reflect.String:
			remaining -= v.Len()
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				if !walk(v.Index(i)) {
					return false
				}
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() && !walk(v.Field(i)) {
					return false
				}
			}
		case reflect.Map:
			it := v.MapRange()
			for it.Next() {
				if !walk(it.Key()) || !walk(it.Value()) {
					return false
				}
			}
		}
		return remaining >= 0
	}
	return walk(reflect.ValueOf(value))
}
func (m *MemoryStore) ReadTaskExport(ctx context.Context, id, tenant string, budget int) (*TaskExportSnapshot, error) {
	return m.readTaskExport(ctx, id, tenant, budget, false)
}
func (m *MemoryStore) ReadJointTaskExport(ctx context.Context, id, tenant string, budget int) (*TaskExportSnapshot, error) {
	return m.readTaskExport(ctx, id, tenant, budget, true)
}
func (m *MemoryStore) readTaskExport(ctx context.Context, id, tenant string, budget int, joint bool) (*TaskExportSnapshot, error) {
	if err := validateExportRequest(id, tenant, budget); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	task := m.tasks[id]
	if task == nil || task.TenantID != tenant {
		return nil, sql.ErrNoRows
	}
	if len(task.Trace) > MaxTaskExportTraces || !exportCopyFits(task, budget) {
		return nil, ErrTaskExportTooLarge
	}
	copy := types.CloneTask(task)
	events := make([]TaskTraceEvent, len(copy.Trace))
	for i, tr := range copy.Trace {
		events[i] = TaskTraceEvent{Sequence: int64(i + 1), Trace: tr}
		if times := m.traceTimes[id]; i < len(times) && !times[i].IsZero() {
			at := times[i].UTC()
			events[i].RecordedAt = &at
		}
	}
	copy.Trace = nil
	result := &TaskExportSnapshot{Task: copy, Traces: events}
	if joint {
		deps, err := m.readExportDependenciesLocked(ctx, task, budget)
		if err != nil {
			return nil, err
		}
		result.Dependencies = deps
	}
	if !exportCopyFits(result, budget) {
		return nil, ErrTaskExportTooLarge
	}
	return result, ctx.Err()
}
func (s *SQLiteStore) ReadTaskExport(ctx context.Context, id, tenant string, budget int) (*TaskExportSnapshot, error) {
	return readSQLTaskExport(ctx, s.db, id, tenant, budget, false, s.getTaskQuery)
}
func (p *PostgresStore) ReadTaskExport(ctx context.Context, id, tenant string, budget int) (*TaskExportSnapshot, error) {
	return readSQLTaskExport(ctx, p.db, id, tenant, budget, true, p.getTaskQuery)
}
func readSQLTaskExport(ctx context.Context, db *sql.DB, id, tenant string, budget int, postgres bool, get func(context.Context, taskSQLReader, string, bool) (*types.Task, error), joint ...bool) (*TaskExportSnapshot, error) {
	if err := validateExportRequest(id, tenant, budget); err != nil {
		return nil, err
	}
	options := &sql.TxOptions{ReadOnly: true}
	if postgres {
		options.Isolation = sql.LevelRepeatableRead
	}
	tx, err := db.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Scope and size checks share the same snapshot as the eventual payload.
	length := "length"
	if postgres {
		length = "octet_length"
	}
	fields := []string{"id", "tenant_id", "session_id", "creator_actor_id", "goal", "workspace", "hypothesis", "unresolved_json", "memories_json", "answer_audit_json", "final_answer", "execution_mode", "requested_team", "team_selection_source", "team_name", "team_config_digest", "brain_project_id", "brain_snapshot_id", "brain_config_digest", "termination_kind", "error_code", "error_message", "execution_trace_id"}
	expression := "0"
	for _, field := range fields {
		operand := field
		if !postgres {
			operand = "CAST(" + field + " AS BLOB)"
		}
		expression += "+COALESCE(" + length + "(" + operand + "),0)"
	}
	var size int64
	err = tx.QueryRowContext(ctx, "SELECT "+expression+" FROM tasks WHERE id="+lifecycleSQLMarker(postgres, 1)+" AND tenant_id="+lifecycleSQLMarker(postgres, 2), id, tenant).Scan(&size)
	if err != nil {
		return nil, err
	}
	if size > int64(budget) {
		return nil, ErrTaskExportTooLarge
	}
	var count int
	var traceBytes int64
	expr := "0"
	for _, field := range []string{"goal", "action", "query", "observation", "evidence_json", "agent_role", "error_text"} {
		operand := field
		if !postgres {
			operand = "CAST(" + field + " AS BLOB)"
		}
		expr += "+COALESCE(" + length + "(" + operand + "),0)"
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM("+expr+"),0) FROM traces WHERE task_id="+lifecycleSQLMarker(postgres, 1), id).Scan(&count, &traceBytes); err != nil {
		return nil, err
	}
	if count > MaxTaskExportTraces || traceBytes+size > int64(budget) {
		return nil, ErrTaskExportTooLarge
	}
	task, err := get(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	events, err := listSQLTaskTraces(ctx, tx, id, 0, MaxTaskExportTraces+1, postgres)
	if err != nil {
		return nil, err
	}
	if len(events) != count {
		return nil, ErrTaskExportUnavailable
	}
	result := &TaskExportSnapshot{Task: task, Traces: events}
	if len(joint) > 0 && joint[0] {
		deps, err := readSQLExportDependencies(ctx, tx, task, budget, postgres)
		if err != nil {
			return nil, err
		}
		result.Dependencies = deps
	}
	if !exportCopyFits(result, budget) {
		return nil, ErrTaskExportTooLarge
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func EncodeTaskExport(ctx context.Context, snapshot *TaskExportSnapshot, backend, id, tenant string, budget int) ([]byte, error) {
	if err := validateExportRequest(id, tenant, budget); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.Task == nil || snapshot.Task.ID != id || snapshot.Task.TenantID != tenant || len(snapshot.Task.Trace) != 0 || len(snapshot.Traces) > MaxTaskExportTraces {
		return nil, ErrTaskExportUnavailable
	}
	for i, event := range snapshot.Traces {
		if event.Sequence != int64(i+1) {
			return nil, ErrTaskExportUnavailable
		}
	}
	if !exportCopyFits(snapshot, budget) {
		return nil, ErrTaskExportTooLarge
	}
	if snapshot.Dependencies != nil {
		if err := validateExportDependencies(snapshot.Task, snapshot.Dependencies); err != nil {
			return nil, err
		}
	}
	// Trace belongs only in the events array; every physical timestamp is hashed.
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode export snapshot: %w", err)
	}
	hash := sha256.Sum256(payload)
	artifact := TaskExportArtifact{Schema: "task-trace-export-v1", SourceBackend: backend, ExportedAt: time.Now().UTC(), SourcePreserved: true, RestoreReady: false, Included: []string{"task", "trace_events", "task_embedded_memories", "answer_audit"}, Excluded: []string{"durable_approvals", "independent_memories_and_embeddings", "session_record_and_sequence_high_watermark", "execution_leases", "external_audit_records", "workspace_files", "encryption_keys"}, PayloadSHA256: hex.EncodeToString(hash[:]), PayloadBytes: len(payload), TraceCount: len(snapshot.Traces), Payload: payload}
	if snapshot.Dependencies != nil {
		artifact.Schema = "task-joint-export-v2"
		approvals, memories := len(snapshot.Dependencies.Approvals), len(snapshot.Dependencies.Memories)
		artifact.ApprovalCount = &approvals
		artifact.MemoryCount = &memories
		artifact.Included = append(artifact.Included, "durable_approvals_ciphertext", "task_owned_memories_and_embeddings", "session_record_and_sequence_high_watermark")
		artifact.Excluded = []string{"execution_leases", "external_audit_records", "workspace_files", "encryption_keys", "other_tasks_in_session", "unrelated_memories"}
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		return nil, err
	}
	if len(data) > budget {
		return nil, ErrTaskExportTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
