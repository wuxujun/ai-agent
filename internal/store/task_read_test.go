package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskReadPages_StableOrderAndIndependentTraceSequence(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) Store
	}{
		{"memory", func(*testing.T) Store { return NewMemoryStore() }},
		{"sqlite", func(t *testing.T) Store {
			st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "task-read.db"))
			if err != nil {
				t.Fatal(err)
			}
			return st
		}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			st := backend.open(t)
			t.Cleanup(func() { _ = st.Close() })
			created := time.Now().UTC().Truncate(time.Microsecond)
			occurred := created.Add(-2 * time.Second)
			duration := int64(37)
			for _, fixture := range []struct {
				id, tenant, session string
				at                  time.Time
			}{
				{"a-1", "tenant-a", "s1", created},
				{"a-2", "tenant-a", "s1", created},
				{"a-3", "tenant-a", "s2", created.Add(-time.Hour)},
				{"b-1", "tenant-b", "s1", created.Add(time.Hour)},
			} {
				task := &types.Task{ID: fixture.id, TenantID: fixture.tenant, SessionID: fixture.session,
					CreatedAt: fixture.at, Goal: "long goal", Status: types.StatusRunning,
					ExecutionTraceID: "0123456789abcdef0123456789abcdef",
					Trace:            []types.StepTrace{{Step: 1, Action: "first", OccurredAt: &occurred, DurationMS: &duration}, {Step: 1, Action: "second"}, {Step: 2, Action: "third"}}}
				if err := st.(TaskCreationStore).CreateTask(t.Context(), task); err != nil {
					t.Fatal(err)
				}
			}
			reader := st.(TaskSummaryStore)
			first, err := reader.ListTaskSummaries(t.Context(), TaskSummaryFilter{TenantID: "tenant-a", Limit: 1})
			if err != nil || len(first) != 1 || first[0].ID != "a-2" {
				t.Fatalf("first = %+v, %v", first, err)
			}
			second, err := reader.ListTaskSummaries(t.Context(), TaskSummaryFilter{
				TenantID: "tenant-a", BeforeCreatedAt: first[0].CreatedAt, BeforeID: first[0].ID, Limit: 2,
			})
			if err != nil || len(second) != 2 || second[0].ID != "a-1" || second[1].ID != "a-3" {
				t.Fatalf("second = %+v, %v", second, err)
			}
			bySession, err := reader.ListTaskSummaries(t.Context(), TaskSummaryFilter{TenantID: "tenant-a", SessionID: "s1", Limit: 10})
			if err != nil || len(bySession) != 2 {
				t.Fatalf("session page = %+v, %v", bySession, err)
			}
			metadata, err := reader.GetTaskWithoutTrace(t.Context(), "a-1")
			if err != nil || len(metadata.Trace) != 0 || metadata.Goal != "long goal" || metadata.ExecutionTraceID != "0123456789abcdef0123456789abcdef" {
				t.Fatalf("metadata = %+v, %v", metadata, err)
			}
			traces := st.(TaskTraceStore)
			page1, err := traces.ListTaskTraces(t.Context(), "a-1", 0, 2)
			if err != nil || len(page1) != 2 || page1[0].Sequence != 1 || page1[1].Sequence != 2 || page1[1].Trace.Step != 1 {
				t.Fatalf("trace page1 = %+v, %v", page1, err)
			}
			if page1[0].RecordedAt == nil || page1[1].RecordedAt == nil {
				t.Fatalf("new trace events have no persisted timestamp: %+v", page1)
			}
			if page1[0].Trace.OccurredAt == nil || !page1[0].Trace.OccurredAt.Equal(occurred) ||
				page1[0].Trace.DurationMS == nil || *page1[0].Trace.DurationMS != 37 ||
				page1[1].Trace.OccurredAt != nil || page1[1].Trace.DurationMS != nil {
				t.Fatalf("measured timing not preserved: %+v", page1)
			}
			*page1[0].Trace.DurationMS = 99
			reloaded, err := traces.ListTaskTraces(t.Context(), "a-1", 0, 1)
			if err != nil || len(reloaded) != 1 || reloaded[0].Trace.DurationMS == nil || *reloaded[0].Trace.DurationMS != 37 {
				t.Fatalf("trace timing shared with store: %+v, %v", reloaded, err)
			}
			page2, err := traces.ListTaskTraces(t.Context(), "a-1", page1[1].Sequence, 2)
			if err != nil || len(page2) != 1 || page2[0].Sequence != 3 || page2[0].Trace.Action != "third" {
				t.Fatalf("trace page2 = %+v, %v", page2, err)
			}
			full, err := st.GetTask(t.Context(), "a-1")
			if err != nil {
				t.Fatal(err)
			}
			if full.Trace[0].OccurredAt == nil || !full.Trace[0].OccurredAt.Equal(occurred) ||
				full.Trace[0].DurationMS == nil || *full.Trace[0].DurationMS != 37 {
				t.Fatalf("full task lost action timing: %+v", full.Trace[0])
			}
			full.Trace[0].Observation = "updated snapshot"
			full.Trace = append(full.Trace, types.StepTrace{Step: 2, Action: "fourth"})
			if err := st.SaveFullTask(t.Context(), full); err != nil {
				t.Fatal(err)
			}
			updated, err := traces.ListTaskTraces(t.Context(), "a-1", 0, 5)
			if err != nil || len(updated) != 4 || updated[0].RecordedAt == nil ||
				!updated[0].RecordedAt.Equal(*page1[0].RecordedAt) || updated[3].RecordedAt == nil {
				t.Fatalf("trace timestamp changed on snapshot overwrite: %+v, %v", updated, err)
			}
		})
	}
}

func TestSQLiteLegacyTraceHasNoRecordedAt(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "legacy-trace-time.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateTask(t.Context(), &types.Task{ID: "legacy-trace-time", Status: types.StatusCreated}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(t.Context(), `INSERT INTO traces(task_id, step, goal, action, query, observation, evidence_json, recorded_at) VALUES(?, 1, '', 'legacy', '', '', 'null', NULL)`, "legacy-trace-time"); err != nil {
		t.Fatal(err)
	}
	page, err := st.ListTaskTraces(t.Context(), "legacy-trace-time", 0, 2)
	if err != nil || len(page) != 1 || page[0].RecordedAt != nil ||
		page[0].Trace.OccurredAt != nil || page[0].Trace.DurationMS != nil {
		t.Fatalf("legacy trace timing = %+v, %v", page, err)
	}
}

func TestEncodeRedisTaskReadModel_EmptyTraceIsLuaArray(t *testing.T) {
	_, _, traces, _, err := encodeRedisTaskReadModel(&types.Task{ID: "empty-trace", Status: types.StatusCreated})
	if err != nil || string(traces) != "[]" {
		t.Fatalf("empty trace encoding = %q, %v", traces, err)
	}
}
