package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sync"

	_ "modernc.org/sqlite"
)

var ErrTaskRestoreInvalid = errors.New("invalid joint restore artifact")
var ErrTaskRestoreConflict = errors.New("isolated restore already exists")
var ErrTaskRestoreMismatch = errors.New("isolated restore verification failed")
var ErrTaskRestoreReadOnly = errors.New("reopened restore sandbox is read-only")

const taskRestoreApplicationID = 0x41525354
const taskRestoreSchemaVersion = 1

// TaskRestoreSandbox is a dedicated, inert SQLite database. It deliberately
// does not implement Store: nothing here can acquire execution leases, index
// memories, resolve approvals, or overwrite online records. Each instance is
// owns its connection. Persistent creation allocates a new private directory;
// reopening is read-only and requires the dedicated database identity.
type TaskRestoreSandbox struct {
	mu         sync.Mutex
	db         *sql.DB
	persistent bool
	readOnly   bool
}

type TaskRestoreReport struct {
	TaskID          string `json:"task_id"`
	TenantID        string `json:"tenant_id"`
	Verified        bool   `json:"verified"`
	Target          string `json:"target"`
	SourcePreserved bool   `json:"source_preserved"`
	RestoreReady    bool   `json:"restore_ready"`
	PayloadSHA256   string `json:"payload_sha256"`
	TraceCount      int    `json:"trace_count"`
	ApprovalCount   int    `json:"approval_count"`
	MemoryCount     int    `json:"memory_count"`
	SourceBackend   string `json:"source_backend,omitempty"`
	Reopened        bool   `json:"reopened,omitempty"`
}

func NewTaskRestoreSandbox(ctx context.Context) (*TaskRestoreSandbox, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	err = initializeTaskRestoreDB(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &TaskRestoreSandbox{db: db}, nil
}
func (s *TaskRestoreSandbox) Close() error { return s.db.Close() }

func initializeTaskRestoreDB(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `PRAGMA application_id=0x41525354; PRAGMA user_version=1;
 CREATE TABLE restore_runs (task_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, expected BLOB NOT NULL, payload_hash TEXT NOT NULL, artifact BLOB NOT NULL);
 CREATE TABLE restore_parts (task_id TEXT NOT NULL, kind TEXT NOT NULL, position INTEGER NOT NULL, body BLOB NOT NULL, PRIMARY KEY(task_id,kind,position));`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// strictRestoreJSON rejects unknown fields, duplicate keys and trailing JSON,
// and keeps arbitrary approval parameter integers at their original precision.
func strictRestoreJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrTaskRestoreInvalid
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return ErrTaskRestoreInvalid
		}
		_, err = d.Token()
		return err
	}
	if err := walk(); err != nil {
		return ErrTaskRestoreInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrTaskRestoreInvalid
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrTaskRestoreInvalid
	}
	return nil
}

func canonicalRestoreJSON(data []byte) ([]byte, error) {
	var value any
	if err := strictRestoreJSON(data, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func decodeJointRestore(ctx context.Context, data []byte, tenant string, budget int) (*TaskExportArtifact, *TaskExportSnapshot, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if tenant == "" || budget < 4096 || budget > MaxTaskExportBytes {
		return nil, nil, nil, ErrTaskRestoreInvalid
	}
	if len(data) > budget {
		return nil, nil, nil, ErrTaskExportTooLarge
	}
	var a TaskExportArtifact
	if err := strictRestoreJSON(data, &a); err != nil {
		return nil, nil, nil, err
	}
	manifestJSON, err := json.Marshal(&a)
	if err != nil {
		return nil, nil, nil, ErrTaskRestoreInvalid
	}
	manifest, err := canonicalRestoreJSON(manifestJSON)
	original, originalErr := canonicalRestoreJSON(data)
	if err != nil || originalErr != nil || !bytes.Equal(manifest, original) {
		return nil, nil, nil, ErrTaskRestoreInvalid
	}
	hash := sha256.Sum256(a.Payload)
	if a.Schema != "task-joint-export-v2" || !a.SourcePreserved || a.RestoreReady || a.SourceBackend == "" || a.ExportedAt.IsZero() || a.PayloadBytes != len(a.Payload) || a.PayloadSHA256 != hex.EncodeToString(hash[:]) {
		return nil, nil, nil, ErrTaskRestoreInvalid
	}
	included := []string{"task", "trace_events", "task_embedded_memories", "answer_audit", "durable_approvals_ciphertext", "task_owned_memories_and_embeddings", "session_record_and_sequence_high_watermark"}
	excluded := []string{"execution_leases", "external_audit_records", "workspace_files", "encryption_keys", "other_tasks_in_session", "unrelated_memories"}
	if !reflect.DeepEqual(a.Included, included) || !reflect.DeepEqual(a.Excluded, excluded) {
		return nil, nil, nil, ErrTaskRestoreInvalid
	}
	var snap TaskExportSnapshot
	if err := strictRestoreJSON(a.Payload, &snap); err != nil {
		return nil, nil, nil, err
	}
	if snap.Task == nil || snap.Task.ID == "" || len(snap.Task.ID) > 256 || snap.Task.TenantID != tenant || len(snap.Task.Trace) != 0 || snap.Dependencies == nil || snap.Task.SequenceNo < 0 {
		return nil, nil, nil, ErrTaskRestoreInvalid
	}
	if len(snap.Traces) > MaxTaskExportTraces {
		return nil, nil, nil, ErrTaskExportTooLarge
	}
	if err := validateExportDependencies(snap.Task, snap.Dependencies); err != nil {
		return nil, nil, nil, err
	}
	for i, tr := range snap.Traces {
		if tr.Sequence != int64(i+1) {
			return nil, nil, nil, ErrTaskRestoreInvalid
		}
	}
	for _, m := range snap.Dependencies.Memories {
		if m.SessionID != snap.Task.SessionID {
			return nil, nil, nil, ErrTaskRestoreInvalid
		}
	}
	for _, approval := range snap.Dependencies.Approvals {
		if approval.Status == "" || approval.Validate() != nil {
			return nil, nil, nil, ErrTaskRestoreInvalid
		}
	}
	if a.TraceCount != len(snap.Traces) || a.ApprovalCount == nil || *a.ApprovalCount != len(snap.Dependencies.Approvals) || a.MemoryCount == nil || *a.MemoryCount != len(snap.Dependencies.Memories) {
		return nil, nil, nil, ErrTaskRestoreInvalid
	}
	// Refuse fields/aliases or representations that typed restoration would lose.
	expected, err := canonicalRestoreJSON(a.Payload)
	if err != nil {
		return nil, nil, nil, err
	}
	encoded, err := json.Marshal(&snap)
	if err != nil {
		return nil, nil, nil, ErrTaskRestoreInvalid
	}
	actual, err := canonicalRestoreJSON(encoded)
	if err != nil || !bytes.Equal(expected, actual) {
		return nil, nil, nil, ErrTaskRestoreInvalid
	}
	return &a, &snap, expected, ctx.Err()
}

// ImportJoint validates before atomically inserting distinct restored records.
// A failed import leaves no records. Existing task IDs are never overwritten.
func (s *TaskRestoreSandbox) ImportJoint(ctx context.Context, data []byte, tenant string, budget int) error {
	if s.readOnly {
		return ErrTaskRestoreReadOnly
	}
	a, snap, expected, err := decodeJointRestore(ctx, data, tenant, budget)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM restore_runs WHERE task_id=?", snap.Task.ID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return ErrTaskRestoreConflict
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO restore_runs VALUES (?,?,?,?,?)", snap.Task.ID, tenant, expected, a.PayloadSHA256, data); err != nil {
		return err
	}
	put := func(kind string, pos int, value any) error {
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO restore_parts VALUES (?,?,?,?)", snap.Task.ID, kind, pos, body)
		return err
	}
	if err := put("task", 0, snap.Task); err != nil {
		return err
	}
	if err := put("session", 0, snap.Dependencies.Session); err != nil {
		return err
	}
	for i, tr := range snap.Traces {
		if err := put("trace", i, tr); err != nil {
			return err
		}
	}
	for i, a := range snap.Dependencies.Approvals {
		if err := put("approval", i, a); err != nil {
			return err
		}
	}
	for i, m := range snap.Dependencies.Memories {
		if err := put("memory", i, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Verify reconstructs from the isolated records, rather than returning the
// uploaded snapshot. Original times, ciphertext, versions, sequence and all
// bodies participate in comparison. This is not authorization to resume work.
func (s *TaskRestoreSandbox) Verify(ctx context.Context, id, tenant string) (*TaskRestoreReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var expected []byte
	var hash string
	var artifact []byte
	var size int
	if err := tx.QueryRowContext(ctx, "SELECT length(expected)+length(artifact) FROM restore_runs WHERE task_id=? AND tenant_id=?", id, tenant).Scan(&size); err != nil {
		return nil, err
	}
	if size > 2*MaxTaskExportBytes {
		return nil, ErrTaskRestoreMismatch
	}
	if err := tx.QueryRowContext(ctx, "SELECT expected,payload_hash,artifact FROM restore_runs WHERE task_id=? AND tenant_id=?", id, tenant).Scan(&expected, &hash, &artifact); err != nil {
		return nil, err
	}
	a, restored, canonical, err := decodeJointRestore(ctx, artifact, tenant, MaxTaskExportBytes)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTaskRestoreMismatch
	}
	if restored.Task.ID != id || a.PayloadSHA256 != hash || !bytes.Equal(expected, canonical) {
		return nil, ErrTaskRestoreMismatch
	}
	snap := *restored
	// Preserve nil/empty collection shape while replacing every record from SQL.
	wantTraces, wantApprovals, wantMemories := len(snap.Traces), len(snap.Dependencies.Approvals), len(snap.Dependencies.Memories)
	snap.Task = nil
	snap.Dependencies.Session = nil
	snap.Traces = snap.Traces[:0]
	snap.Dependencies.Approvals = snap.Dependencies.Approvals[:0]
	snap.Dependencies.Memories = snap.Dependencies.Memories[:0]
	var partBytes, partCount int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(body)),0),COUNT(*) FROM restore_parts WHERE task_id=?", id).Scan(&partBytes, &partCount); err != nil {
		return nil, err
	}
	if partBytes > MaxTaskExportBytes || partCount != 2+wantTraces+wantApprovals+wantMemories {
		return nil, ErrTaskRestoreMismatch
	}
	rows, err := tx.QueryContext(ctx, "SELECT kind,position,body FROM restore_parts WHERE task_id=? ORDER BY kind,position", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	limits := map[string]int{"task": 1, "session": 1, "trace": wantTraces, "approval": wantApprovals, "memory": wantMemories}
	remaining := MaxTaskExportBytes
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var kind string
		var pos int
		var body []byte
		if err := rows.Scan(&kind, &pos, &body); err != nil {
			return nil, err
		}
		remaining -= len(body)
		if remaining < 0 {
			return nil, ErrTaskRestoreMismatch
		}
		if pos != counts[kind] {
			return nil, ErrTaskRestoreMismatch
		}
		counts[kind]++
		if counts[kind] > limits[kind] {
			return nil, ErrTaskRestoreMismatch
		}
		var target any
		switch kind {
		case "task":
			target = &snap.Task
		case "session":
			target = &snap.Dependencies.Session
		case "trace":
			snap.Traces = append(snap.Traces, TaskTraceEvent{})
			target = &snap.Traces[len(snap.Traces)-1]
		case "approval":
			snap.Dependencies.Approvals = append(snap.Dependencies.Approvals, nil)
			target = &snap.Dependencies.Approvals[len(snap.Dependencies.Approvals)-1]
		case "memory":
			snap.Dependencies.Memories = append(snap.Dependencies.Memories, nil)
			target = &snap.Dependencies.Memories[len(snap.Dependencies.Memories)-1]
		default:
			return nil, ErrTaskRestoreMismatch
		}
		if err := strictRestoreJSON(body, target); err != nil {
			return nil, ErrTaskRestoreMismatch
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if counts["task"] != 1 || counts["session"] != 1 || counts["trace"] != wantTraces || counts["approval"] != wantApprovals || counts["memory"] != wantMemories {
		return nil, ErrTaskRestoreMismatch
	}
	encoded, err := json.Marshal(&snap)
	if err != nil {
		return nil, ErrTaskRestoreMismatch
	}
	actual, err := canonicalRestoreJSON(encoded)
	if err != nil || !bytes.Equal(expected, actual) {
		return nil, ErrTaskRestoreMismatch
	}
	if snap.Dependencies.Session != nil {
		snap.Dependencies.Session.Session.NextSequence = snap.Dependencies.Session.NextSequence
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	target := "ephemeral_isolated_sqlite"
	if s.persistent {
		target = "persistent_isolated_sqlite"
	}
	return &TaskRestoreReport{TaskID: id, TenantID: tenant, Verified: true, Target: target, SourcePreserved: true, RestoreReady: false, PayloadSHA256: hash, TraceCount: wantTraces, ApprovalCount: wantApprovals, MemoryCount: wantMemories, SourceBackend: a.SourceBackend, Reopened: s.readOnly}, nil
}

// VerifyIsolatedTaskRestore performs a complete ephemeral import/readback drill.
// No source Store, filesystem path, decryption key or executor is accepted.
func VerifyIsolatedTaskRestore(ctx context.Context, data []byte, tenant string, budget int) (*TaskRestoreReport, error) {
	_, snap, _, err := decodeJointRestore(ctx, data, tenant, budget)
	if err != nil {
		return nil, err
	}
	sandbox, err := NewTaskRestoreSandbox(ctx)
	if err != nil {
		return nil, err
	}
	defer sandbox.Close()
	if err := sandbox.ImportJoint(ctx, data, tenant, budget); err != nil {
		return nil, err
	}
	return sandbox.Verify(ctx, snap.Task.ID, tenant)
}
