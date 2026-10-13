package store

import (
	"context"
	"fmt"
	"github.com/wuxujun/ai-agent/internal/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMemoryMetadataReadDoesNotCloneTrace(t *testing.T) {
	st := NewMemoryStore()
	task := &types.Task{ID: "metadata-capacity", Unresolved: []string{"original"}, Memories: []types.Memory{{Embedding: []float32{1}}}, Trace: make([]types.StepTrace, 10000)}
	for i := range task.Trace {
		task.Trace[i].Evidence = []types.Evidence{{Lines: []string{"original trace"}}}
	}
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	reader := func() {
		metadata, err := st.GetTaskWithoutTrace(t.Context(), task.ID)
		if err != nil || metadata.Trace != nil {
			t.Fatal("invalid metadata read")
		}
	}
	if allocs := testing.AllocsPerRun(20, reader); allocs > 10 {
		t.Fatalf("metadata allocations scale with Trace: %.0f", allocs)
	}
	metadata, err := st.GetTaskWithoutTrace(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	metadata.Unresolved[0] = "mutated"
	metadata.Memories[0].Embedding[0] = 9
	full, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Trace) != 10000 || full.Unresolved[0] != "original" || full.Memories[0].Embedding[0] != 1 || full.Trace[0].Evidence[0].Lines[0] != "original trace" {
		t.Fatal("metadata read mutated/shared stored state")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := st.GetTaskWithoutTrace(ctx, task.ID); err != context.Canceled {
		t.Fatal("metadata read ignored cancellation")
	}
}
func TestMemorySummaryBoundedSelectionPreservesCursorAndFilters(t *testing.T) {
	st := NewMemoryStore()
	created := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	expected := []string{}
	for i := 0; i < 300; i++ {
		task := &types.Task{ID: fmt.Sprintf("tie-%04d", i), TenantID: "a", SessionID: "s", Status: types.StatusRunning, CreatedAt: created}
		if i%3 == 0 {
			task.TenantID = "b"
		}
		if i%5 == 0 {
			task.SessionID = "other"
		}
		if i%7 == 0 {
			task.Status = types.StatusCompleted
		}
		if task.TenantID == "a" && task.SessionID == "s" && task.Status == types.StatusRunning {
			expected = append(expected, task.ID)
		}
		if err := st.CreateTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}
	slices.Reverse(expected)
	filter := TaskSummaryFilter{TenantID: "a", SessionID: "s", Status: types.StatusRunning, Limit: 13}
	got := []string{}
	for {
		page, err := st.ListTaskSummaries(t.Context(), filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > 13 {
			t.Fatal("page limit exceeded")
		}
		for _, item := range page {
			got = append(got, item.ID)
		}
		last := page[len(page)-1]
		filter.BeforeID = last.ID
		filter.BeforeCreatedAt = last.CreatedAt
	}
	if !slices.Equal(got, expected) {
		t.Fatal("bounded selection changed stable order/filter/cursor coverage")
	}
}

func TestSQLiteSummaryDeepCursorUsesIndexRange(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, status := range []types.TaskStatus{"", types.StatusRunning} {
		filter := TaskSummaryFilter{TenantID: "a", Status: status, BeforeCreatedAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), BeforeID: "task-000020", Limit: 20}
		query, args := taskSummarySQL(filter, false)
		rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		rangeSearch := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			t.Log(detail)
			compact := strings.Join(strings.Fields(detail), "")
			if strings.Contains(compact, "created_at<?") || strings.Contains(compact, "(created_at,id)<") {
				rangeSearch = true
			}
			if strings.Contains(detail, "TEMP B-TREE") {
				rows.Close()
				t.Fatal("summary cursor requires an unbounded sort")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !rangeSearch {
			t.Fatal("deep cursor scans the tenant prefix instead of seeking to its date range")
		}
	}
}
