package tools_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Exercise the planner -> registered middleware -> executor trace contract,
// including the evidence that survives middleware observation truncation.
func TestSQLQueryPlannerExecutorContract(t *testing.T) {
	w := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(w, "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE items(n); INSERT INTO items VALUES(7)"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query string
		wantError   bool
	}{
		{"read", "SELECT n FROM items", false},
		{"large_cell", "SELECT hex(zeroblob(524288)) AS payload", false},
		{"iteration_error", "SELECT 1 UNION ALL SELECT abs(-9223372036854775808)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &planner.PlanDecision{Actions: []planner.ActionCall{{Action: "sql_query", Parameters: map[string]any{"path": "probe.db", "query": tc.query}}}}
			if err := planner.ValidateDecision(d); err != nil {
				t.Fatal(err)
			}
			traces, err := (&executor.DefaultExecutor{}).Execute(context.Background(), &types.Task{ID: "sql-contract", Workspace: w}, d)
			if err != nil || len(traces) != 1 {
				t.Fatalf("traces=%d err=%v", len(traces), err)
			}
			tr := traces[0]
			if (tr.Error != "") != tc.wantError {
				t.Fatalf("trace error: %q", tr.Error)
			}
			if tc.wantError {
				return
			}
			if len(tr.Evidence) != 1 {
				t.Fatal("missing SQL evidence")
			}
			data := strings.Join(tr.Evidence[0].Lines, "\n")
			if len(data) > 1<<20 {
				t.Fatalf("unbounded persisted evidence: %d", len(data))
			}
			if tc.name == "read" && data != "n\n7" {
				t.Fatalf("wrong SQL trace: %q", data)
			}
			if tc.name == "large_cell" && !strings.Contains(data, "[truncated") {
				t.Fatal("large result not marked partial")
			}
		})
	}
	for _, params := range []map[string]any{
		{"path": "probe.db", "query": `SELECT '\'; UPDATE items SET n=8; SELECT 1; --'`},
		{"path": "probe.db?_pragma=query_only(0)", "query": "SELECT 1"},
		{"path": "probe.db", "query": "EXPLAIN PRAGMA query_only=OFF"},
		{"path": "probe.db", "query": "EXPLAIN \ufeffPRAGMA query_only=OFF"},
	} {
		d := &planner.PlanDecision{Actions: []planner.ActionCall{{Action: "sql_query", Parameters: params}}}
		if err := planner.ValidateDecision(d); err == nil {
			t.Fatal("planner accepted unsafe SQL action")
		}
	}
	var n int
	if err := db.QueryRow("SELECT n FROM items").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("read-only tool changed the database: %d", n)
	}
}
