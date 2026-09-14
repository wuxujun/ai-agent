package tools

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sqlSecurityFixture(t *testing.T, path string, value int) *sql.DB {
	t.Helper()
	// Encode the fixture path independently of the production connection opener.
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE items(n INTEGER); INSERT INTO items VALUES (?)", value); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLSecurityRejectsUnsafeExecution(t *testing.T) {
	w := t.TempDir()
	db := sqlSecurityFixture(t, filepath.Join(w, "probe.db"), 7)
	outside := filepath.Join(t.TempDir(), "outside.db")
	sqlSecurityFixture(t, outside, 99)
	for _, query := range []string{
		`SELECT '\'; UPDATE items SET n=8; SELECT 1; --'`,
		fmt.Sprintf("ATTACH DATABASE '%s' AS ext; SELECT n FROM ext.items", strings.ReplaceAll(outside, "'", "''")),
		"EXPLAIN PRAGMA query_only=OFF",
		"EXPLAIN \ufeffPRAGMA query_only=OFF",
		"SELECT 1; PRAGMA query_only=OFF",
		"UPDATE items SET n=8",
	} {
		t.Run(query, func(t *testing.T) {
			params := map[string]any{"path": "probe.db", "query": query}
			if err := (&SQLQueryTool{}).Validate(params); err == nil {
				t.Error("unsafe query accepted by validation")
			}
			if result, err := (&SQLQueryTool{}).Execute(context.Background(), w, params); err == nil {
				t.Errorf("unsafe query executed: %+v", result)
			}
			var n int
			if err := db.QueryRow("SELECT n FROM items").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 7 {
				t.Fatalf("database changed: n=%d", n)
			}
		})
	}
}

func TestSQLSecurityLiteralFilenameCannotRedirect(t *testing.T) {
	root := t.TempDir()
	w := filepath.Join(root, "workspace")
	if err := os.Mkdir(w, 0700); err != nil {
		t.Fatal(err)
	}
	sqlSecurityFixture(t, filepath.Join(root, "outside.db"), 99)
	name := "%2e%2e%2foutside.db"
	sqlSecurityFixture(t, filepath.Join(w, name), 7)
	result, err := (&SQLQueryTool{}).Execute(context.Background(), w, map[string]any{"path": name, "query": "SELECT n FROM items"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(result.Evidence[0].Lines, "\n"); got != "n\n7" {
		t.Fatalf("opened a different database: %q", got)
	}
}

func TestSQLSecurityMissingAndInvalidPaths(t *testing.T) {
	w := t.TempDir()
	db := sqlSecurityFixture(t, filepath.Join(w, "probe.db"), 7)
	if err := os.Mkdir(filepath.Join(w, "subdir"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing.db", "probe.db?_pragma=" + url.QueryEscape("query_only(0); UPDATE items SET n=8"), "probe.db#fragment", "..", ""} {
		t.Run(name, func(t *testing.T) {
			if result, err := (&SQLQueryTool{}).Execute(context.Background(), w, map[string]any{"path": name, "query": "SELECT 1"}); err == nil {
				t.Fatalf("nonexistent or invalid path executed: %+v", result)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(w, "missing.db")); !os.IsNotExist(err) {
		t.Fatalf("missing database created: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT n FROM items").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("path caused a write: n=%d", n)
	}

	if _, err := (&SQLQueryTool{}).Execute(context.Background(), w, map[string]any{"path": "subdir", "query": "SELECT 1"}); err == nil {
		t.Fatal("directory accepted as database")
	}
	link := filepath.Join(w, "link.db")
	outside := filepath.Join(t.TempDir(), "outside.db")
	sqlSecurityFixture(t, outside, 99)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (&SQLQueryTool{}).Execute(context.Background(), w, map[string]any{"path": "link.db", "query": "SELECT 1"}); err == nil {
		t.Fatal("outside symlink accepted")
	}
}

func TestSQLSecuritySQLiteDialect(t *testing.T) {
	w := t.TempDir()
	sqlSecurityFixture(t, filepath.Join(w, "probe.db"), 7)
	for _, query := range []string{
		"SELECT/**/1", "SELECT-- comment\n1", `SELECT 'path\' AS value`,
		`SELECT 'it''s safe' AS value`, `SELECT 1 AS [x DELETE y]`,
		`SELECT 1 AS "x""UPDATE"`, "WITH c AS (SELECT 1 AS n) SELECT n FROM c",
		"EXPLAIN SELECT 1", "SELECT 1; SELECT 2", "SELECT'UPDATE'", "SELECT\f1",
		"SELECT '\ufeff' AS value", "SELECT /* \ufeffPRAGMA */ 1",
	} {
		t.Run(query, func(t *testing.T) {
			params := map[string]any{"path": "probe.db", "query": query}
			if err := (&SQLQueryTool{}).Validate(params); err != nil {
				t.Fatalf("valid SQLite read rejected: %v", err)
			}
			if _, err := (&SQLQueryTool{}).Execute(context.Background(), w, params); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, query := range []string{"SELECT 1 AS [unterminated", "SELECT 'unterminated", "SELECT 1 /*", "SELECT 1\x00", "SELECT 1; EXPLAIN \ufeffPRAGMA query_only=OFF"} {
		if err := ValidateSQLReadOnly(query); err == nil {
			t.Errorf("malformed query accepted: %q", query)
		}
	}
}

func TestSQLSecurityConnectionEnforcesReadOnly(t *testing.T) {
	w := t.TempDir()
	path := filepath.Join(w, "probe.db")
	fixture := sqlSecurityFixture(t, path, 7)
	db, err := openReadOnlySQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Hold two simultaneous connections so an initialization applied only to
	// the first connection cannot pass this test.
	for range 2 {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var enabled int
		if err := conn.QueryRowContext(context.Background(), "PRAGMA query_only").Scan(&enabled); err != nil {
			t.Fatal(err)
		}
		if enabled != 1 {
			t.Fatal("pool connection lacks query_only")
		}
		if _, err := conn.ExecContext(context.Background(), "CREATE TEMP TABLE writable(n)"); err == nil {
			t.Fatal("query_only allowed a temporary write")
		}
		// Even if query_only is lost, mode=ro must still protect the main DB.
		if _, err := conn.ExecContext(context.Background(), "PRAGMA query_only=OFF"); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), "UPDATE items SET n=8"); err == nil {
			t.Fatal("connection allowed persistent mutation")
		}
	}
	var n int
	if err := fixture.QueryRow("SELECT n FROM items").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("database changed: %d", n)
	}
}

func TestSQLSecurityDefaultPathAndOrdinaryResults(t *testing.T) {
	w := t.TempDir()
	if err := os.Mkdir(filepath.Join(w, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	sqlSecurityFixture(t, filepath.Join(w, "data", "agent.db"), 7)
	result, err := (&SQLQueryTool{}).Execute(context.Background(), w, map[string]any{"query": "SELECT n, NULL AS empty, X'6869' AS blob FROM items"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(result.Evidence[0].Lines, "\n"); got != "n | empty | blob\n7 | NULL | hi" {
		t.Fatalf("result changed: %q", got)
	}
	query := "WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<100) SELECT n FROM c"
	result, err = (&SQLQueryTool{}).Execute(context.Background(), w, map[string]any{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence[0].Lines) != 101 || strings.Contains(result.Observation, "truncated") {
		t.Fatal("exactly 100 rows incorrectly truncated")
	}
}

func TestSQLSecurityIterationErrorAndCancellation(t *testing.T) {
	w := t.TempDir()
	sqlSecurityFixture(t, filepath.Join(w, "probe.db"), 7)
	result, err := (&SQLQueryTool{}).Execute(context.Background(), w, map[string]any{"path": "probe.db", "query": "SELECT 1 AS value UNION ALL SELECT abs(-9223372036854775808)"})
	if err == nil || result != nil {
		t.Fatalf("partial result reported as success: result=%+v err=%v", result, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = (&SQLQueryTool{}).Execute(ctx, w, map[string]any{"path": "probe.db", "query": "WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c) SELECT sum(n) FROM c"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query did not propagate deadline: %v", err)
	}
}

func TestSQLSecurityOutputBudget(t *testing.T) {
	w := t.TempDir()
	sqlSecurityFixture(t, filepath.Join(w, "probe.db"), 7)
	queries := map[string]string{
		"cell":       "SELECT hex(zeroblob(524288)) AS payload",
		"header":     `SELECT 1 AS "` + strings.Repeat("a", (1<<20)+16) + `"`,
		"cumulative": "WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<100) SELECT hex(zeroblob(8192)) AS payload FROM c",
		"row_limit":  "WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<101) SELECT n FROM c",
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			result, err := (&SQLQueryTool{}).Execute(context.Background(), w, map[string]any{"path": "probe.db", "query": query})
			if err != nil {
				t.Fatal(err)
			}
			data := strings.Join(result.Evidence[0].Lines, "\n")
			if len(data) > 1<<20 || len(result.Observation) > 1<<20 {
				t.Fatalf("result exceeded byte budget: evidence=%d observation=%d", len(data), len(result.Observation))
			}
			if !strings.Contains(data, "[truncated") {
				t.Error("partial result lacks truncation marker")
			}
			if name == "row_limit" && strings.Contains(data, "\n101\n") {
				t.Error("returned more than 100 rows")
			}
		})
	}
}
