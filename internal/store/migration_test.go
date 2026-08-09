package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

const legacySQLiteSchema = `
CREATE TABLE tasks (id TEXT PRIMARY KEY);
CREATE TABLE traces (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL,
    step INTEGER NOT NULL
);
CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    timestamp DATETIME NOT NULL
);`

func TestSQLiteMigrationUpgradesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := openLegacySQLite(t, path)
	if _, err := db.Exec(`INSERT INTO tasks (id) VALUES ('legacy-task')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore() migration error = %v", err)
	}
	defer store.Close()

	assertSQLiteColumns(t, store.db, "tasks", "tenant_id", "session_id", "sequence_no", "created_at", "updated_at", "token_budget", "llm_call_budget", "llm_cost_budget_usd", "llm_calls", "llm_estimated_cost_usd", "memories_json", "answer_audit_json")
	assertSQLiteColumns(t, store.db, "traces", "agent_role", "error_text", "prompt_tokens", "completion_tokens", "total_tokens")
	assertSQLiteColumns(t, store.db, "memories", "tenant_id", "session_id")

	var createdAt, updatedAt sql.NullTime
	if err := store.db.QueryRow(`SELECT created_at, updated_at FROM tasks WHERE id = 'legacy-task'`).Scan(&createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if !createdAt.Valid || !updatedAt.Valid {
		t.Fatalf("legacy timestamps were not backfilled: created=%v updated=%v", createdAt, updatedAt)
	}
}

func TestSQLiteMigrationRollsBackOnIndexFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	db := openLegacySQLite(t, path)
	if _, err := db.Exec(`INSERT INTO traces (task_id, step) VALUES ('duplicate', 1), ('duplicate', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := NewSQLiteStore(path); err == nil {
		_ = store.Close()
		t.Fatal("NewSQLiteStore() succeeded despite duplicate trace steps")
	} else if !strings.Contains(err.Error(), "create trace step index") {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	columns := sqliteColumns(t, db, "traces")
	if columns["agent_role"] {
		t.Fatal("failed migration left agent_role behind; transaction did not roll back")
	}
}

func openLegacySQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacySQLiteSchema); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db
}

func assertSQLiteColumns(t *testing.T, db *sql.DB, table string, expected ...string) {
	t.Helper()
	columns := sqliteColumns(t, db, table)
	for _, column := range expected {
		if !columns[column] {
			t.Errorf("%s.%s was not migrated", table, column)
		}
	}
}

func sqliteColumns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + quoteSQLiteIdentifier(table) + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}
