package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"
)

// SQLiteRestoreBaseline retains a content-bound local export observation. It is
// not an authority envelope, and is never accepted by the restore CLI. Snapshot
// intentionally contains exported business data; callers must protect it.
type SQLiteRestoreBaseline struct {
	Schema           string                       `json:"schema"`
	Status           string                       `json:"status"`
	Pin              SQLiteRestoreVersionPin      `json:"pin"`
	GlobalDataSHA256 string                       `json:"global_data_sha256"`
	Snapshot         *TaskExportSnapshot          `json:"snapshot"`
	Session          *TaskRestoreLocalObservation `json:"session"`
	Versions         []SQLiteRestoreRowVersion    `json:"versions"`
	SHA256           string                       `json:"sha256"`
	Gaps             []string                     `json:"gaps"`
}

// ReadSQLiteRestoreBaseline binds joint export, complete local session members
// and content versions to one transaction. The global epoch intentionally
// invalidates on unrelated core changes too; this conservative scope is not a
// per-tenant authoritative revision. No persistence or write occurs here.
func (s *SQLiteStore) ReadSQLiteRestoreBaseline(ctx context.Context, q TaskRestoreLocalObservationRequest) (*SQLiteRestoreBaseline, error) {
	if s.restoreVersions == nil {
		return nil, ErrSQLiteRestoreVersionsUnavailable
	}
	if err := validateLocalObservationRequest(q); err != nil {
		return nil, err
	}
	if err := s.restoreVersions.checkFile(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	defer tx.Rollback()
	out, err := s.readSQLiteRestoreBaseline(ctx, tx, q)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrTaskExportTooLarge) {
			return nil, err
		}
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	if err := s.restoreVersions.checkFile(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *SQLiteStore) readSQLiteRestoreBaseline(ctx context.Context, tx *sql.Tx, q TaskRestoreLocalObservationRequest) (*SQLiteRestoreBaseline, error) {
	// Scope check precedes any potentially expensive global diagnostic.
	var tenant string
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM tasks WHERE id=?`, q.TaskID).Scan(&tenant); err != nil {
		return nil, err
	}
	if tenant != q.TenantID {
		return nil, sql.ErrNoRows
	}
	ledger, err := s.restoreVersions.read(ctx, tx)
	if err != nil {
		return nil, err
	}
	session, err := readSQLiteLocalObservation(ctx, tx, q, s.getTaskQuery)
	if err != nil {
		return nil, err
	}
	snapshot, err := readSQLTaskExportSnapshot(ctx, tx, q.TaskID, q.TenantID, q.MaxBytes, false, s.getTaskQuery, true)
	if err != nil {
		return nil, err
	}
	// Reuse exact export encodings, including all physical trace timestamps.
	taskBody, _ := json.Marshal(snapshot.Task)
	traceBody, _ := json.Marshal(snapshot.Traces)
	rootFound := false
	selected := map[string]bool{}
	if session.SessionID != "" {
		selected["session\x00"+session.SessionID] = true
	}
	for _, member := range session.Members {
		selected["task\x00"+member.ID] = true
		selected["lease\x00"+member.ID] = true
		if member.ID == q.TaskID {
			rootFound = true
			if member.TaskSHA256 != restoreDigest(taskBody) || member.TraceSHA256 != restoreDigest(traceBody) {
				return nil, ErrSQLiteRestoreVersionsInvalid
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM traces WHERE task_id=? ORDER BY id LIMIT 4097`, member.ID)
		if err != nil {
			return nil, err
		}
		count := 0
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			selected["trace\x00"+strconv.FormatInt(id, 10)] = true
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if count != member.TraceCount {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
	}
	if !rootFound {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if session.SessionID != "" {
		if snapshot.Dependencies == nil || snapshot.Dependencies.Session == nil {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		body, _ := json.Marshal(snapshot.Dependencies.Session)
		if restoreDigest(body) != session.SessionSHA256 {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
	}
	out := &SQLiteRestoreBaseline{Schema: "sqlite-restore-baseline-prototype-v1", Status: "unverified", Pin: SQLiteRestoreVersionPin{
		Origin: ledger.Origin, NamespaceSHA256: ledger.NamespaceSHA256, SchemaSHA256: ledger.SchemaSHA256, MinimumEpoch: ledger.Epoch, PrefixEventsSHA256: ledger.EventsSHA256,
	}, GlobalDataSHA256: ledger.DataSHA256, Snapshot: snapshot, Session: session, Versions: []SQLiteRestoreRowVersion{}, Gaps: []string{
		"independent_trust_floor_unavailable", "dependency_graph_unavailable", "execution_isolation_unverified", "other_store_domains_untracked", "lease_expiry_not_materialized", "writer_permissions_unverified", "historical_payloads_not_retained",
	}}
	for _, version := range ledger.Versions {
		if selected[version.Kind+"\x00"+version.ID] {
			out.Versions = append(out.Versions, version)
		}
	}
	sort.Slice(out.Versions, func(i, j int) bool {
		if out.Versions[i].Kind == out.Versions[j].Kind {
			return out.Versions[i].ID < out.Versions[j].ID
		}
		return out.Versions[i].Kind < out.Versions[j].Kind
	})
	// S02's legacy observation remains accurate as a standalone diagnostic; the
	// baseline supplies the locally bound historical/revision fields separately.
	out.SHA256 = sqliteRestoreVersionHash(out)
	body, err := json.Marshal(out)
	if err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if len(body) > q.MaxBytes {
		return nil, ErrTaskExportTooLarge
	}
	return out, nil
}
