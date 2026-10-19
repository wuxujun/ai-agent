package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/wuxujun/ai-agent/internal/types"
)

const MaxTaskExportDependencies = 1000

type TaskExportSession struct {
	*types.Session
	NextSequence int64 `json:"next_sequence,string"`
}
type TaskExportDependencies struct {
	Approvals []*types.DurableApproval `json:"approvals"`
	Memories  []*types.Memory          `json:"task_owned_memories"`
	Session   *TaskExportSession       `json:"session"`
}

func newExportDependencies() *TaskExportDependencies {
	return &TaskExportDependencies{Approvals: []*types.DurableApproval{}, Memories: []*types.Memory{}}
}
func sortExportDependencies(d *TaskExportDependencies) {
	sort.Slice(d.Approvals, func(i, j int) bool { return d.Approvals[i].ID < d.Approvals[j].ID })
	sort.Slice(d.Memories, func(i, j int) bool { return d.Memories[i].ID < d.Memories[j].ID })
}
func validateExportDependencies(task *types.Task, d *TaskExportDependencies) error {
	if len(d.Approvals) > MaxTaskExportDependencies || len(d.Memories) > MaxTaskExportDependencies {
		return ErrTaskExportTooLarge
	}
	seen := map[string]bool{}
	for _, a := range d.Approvals {
		if a == nil || a.ID == "" || seen[a.ID] || a.TaskID != task.ID || a.TenantID != task.TenantID || a.Request.ID != a.ID || a.Request.TaskID != task.ID || a.Version < 1 {
			return ErrTaskExportUnavailable
		}
		seen[a.ID] = true
	}
	seen = map[string]bool{}
	for _, m := range d.Memories {
		if m == nil || m.ID == "" || seen[m.ID] || m.TaskID != task.ID || m.TenantID != task.TenantID {
			return ErrTaskExportUnavailable
		}
		seen[m.ID] = true
	}
	if task.SessionID == "" {
		if d.Session != nil {
			return ErrTaskExportUnavailable
		}
	} else if d.Session == nil || d.Session.Session == nil || d.Session.ID != task.SessionID || d.Session.TenantID != task.TenantID || d.Session.NextSequence < task.SequenceNo || d.Session.NextSequence < 0 {
		return ErrTaskExportUnavailable
	}
	return nil
}
func (m *MemoryStore) readExportDependenciesLocked(ctx context.Context, task *types.Task, budget int) (*TaskExportDependencies, error) {
	d := newExportDependencies()
	for _, a := range m.approvals {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if a.TaskID == task.ID {
			if len(d.Approvals) >= MaxTaskExportDependencies || !exportCopyFits(a, budget) {
				return nil, ErrTaskExportTooLarge
			}
			d.Approvals = append(d.Approvals, a)
		}
	}
	for _, mem := range m.memories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if mem.TaskID == task.ID {
			if len(d.Memories) >= MaxTaskExportDependencies || !exportCopyFits(mem, budget) {
				return nil, ErrTaskExportTooLarge
			}
			d.Memories = append(d.Memories, mem)
		}
	}
	if task.SessionID != "" {
		session := m.sessions[task.SessionID]
		if session == nil {
			return nil, ErrTaskExportUnavailable
		}
		d.Session = &TaskExportSession{Session: session, NextSequence: session.NextSequence}
	}
	if err := validateExportDependencies(task, d); err != nil {
		return nil, err
	}
	if !exportCopyFits(d, budget) {
		return nil, ErrTaskExportTooLarge
	}
	for i, a := range d.Approvals {
		raw, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		copy := &types.DurableApproval{}
		if err := decodeExportApproval(raw, copy); err != nil {
			return nil, err
		}
		d.Approvals[i] = copy
	}
	for i, mem := range d.Memories {
		copy := *mem
		copy.Embedding = append([]float32(nil), mem.Embedding...)
		d.Memories[i] = &copy
	}
	if d.Session != nil {
		copy := *d.Session.Session
		d.Session.Session = &copy
	}
	sortExportDependencies(d)
	return d, nil
}
func (s *SQLiteStore) ReadJointTaskExport(ctx context.Context, id, tenant string, budget int) (*TaskExportSnapshot, error) {
	return readSQLTaskExport(ctx, s.db, id, tenant, budget, false, s.getTaskQuery, true)
}
func (p *PostgresStore) ReadJointTaskExport(ctx context.Context, id, tenant string, budget int) (*TaskExportSnapshot, error) {
	return readSQLTaskExport(ctx, p.db, id, tenant, budget, true, p.getTaskQuery, true)
}

func exportSQLBytes(fields []string, postgres bool) string {
	expression := "0"
	for _, field := range fields {
		operand := field
		fn := "octet_length"
		if !postgres {
			fn = "length"
			operand = "CAST(" + field + " AS BLOB)"
		} else {
			// PostgreSQL octet_length accepts text/bytea, not jsonb. Keep bytea
			// operands unchanged so ciphertext is measured as bytes, not hex text.
			switch field {
			case "answer_audit_json", "request_json", "unresolved_json", "memories_json", "evidence_json":
				operand = "CAST(" + field + " AS TEXT)"
			}
		}
		expression += "+COALESCE(" + fn + "(" + operand + "),0)"
	}
	return expression
}
func readSQLExportDependencies(ctx context.Context, tx *sql.Tx, task *types.Task, budget int, postgres bool) (*TaskExportDependencies, error) {
	d := newExportDependencies()
	mark := lifecycleSQLMarker(postgres, 1)
	for _, group := range []struct {
		table  string
		fields []string
	}{{"approvals", []string{"id", "task_id", "tenant_id", "request_json", "action_payload", "resolution_payload", "owner"}}, {"memories", []string{"id", "task_id", "tenant_id", "session_id", "goal", "final_answer", "key_findings", "embedding_json"}}} {
		var count int
		var size int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM("+exportSQLBytes(group.fields, postgres)+"),0) FROM "+group.table+" WHERE task_id="+mark, task.ID).Scan(&count, &size); err != nil {
			return nil, err
		}
		if count > MaxTaskExportDependencies || size > int64(budget) {
			return nil, ErrTaskExportTooLarge
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+approvalSelectColumns+" FROM approvals WHERE task_id="+mark+" ORDER BY id", task.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		a, err := scanDurableApproval(rows, true)
		if err != nil {
			rows.Close()
			return nil, err
		}
		d.Approvals = append(d.Approvals, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT id,tenant_id,session_id,task_id,goal,final_answer,key_findings,timestamp,embedding_json FROM memories WHERE task_id="+mark+" ORDER BY id", task.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var mem types.Memory
		var embedding string
		if err := rows.Scan(&mem.ID, &mem.TenantID, &mem.SessionID, &mem.TaskID, &mem.Goal, &mem.FinalAnswer, &mem.KeyFindings, &mem.Timestamp, &embedding); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(embedding), &mem.Embedding); err != nil {
			rows.Close()
			return nil, err
		}
		d.Memories = append(d.Memories, &mem)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if task.SessionID != "" {
		var size int64
		err := tx.QueryRowContext(ctx, "SELECT "+exportSQLBytes([]string{"id", "tenant_id", "title", "status"}, postgres)+" FROM sessions WHERE id="+mark+" AND tenant_id="+lifecycleSQLMarker(postgres, 2), task.SessionID, task.TenantID).Scan(&size)
		if err == sql.ErrNoRows {
			return nil, ErrTaskExportUnavailable
		}
		if err != nil {
			return nil, err
		}
		if size > int64(budget) {
			return nil, ErrTaskExportTooLarge
		}
		session := &types.Session{}
		if err := tx.QueryRowContext(ctx, "SELECT id,tenant_id,title,status,next_sequence,created_at,updated_at FROM sessions WHERE id="+mark+" AND tenant_id="+lifecycleSQLMarker(postgres, 2), task.SessionID, task.TenantID).Scan(&session.ID, &session.TenantID, &session.Title, &session.Status, &session.NextSequence, &session.CreatedAt, &session.UpdatedAt); err != nil {
			return nil, err
		}
		d.Session = &TaskExportSession{Session: session, NextSequence: session.NextSequence}
	}
	if err := validateExportDependencies(task, d); err != nil {
		return nil, err
	}
	if !exportCopyFits(d, budget) {
		return nil, ErrTaskExportTooLarge
	}
	return d, nil
}

func decodeExportApproval(raw []byte, value *types.DurableApproval) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(value)
}
