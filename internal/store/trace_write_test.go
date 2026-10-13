package store

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSQLitePreparedTraceSnapshotRollbackPreservesTaskAndTraces(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := &types.Task{ID: "trace-rollback", Goal: "original", Status: types.StatusRunning, StepCount: 7, Trace: []types.StepTrace{{Step: 1, Action: "one"}, {Step: 1, Action: "two"}, {Step: 2, Action: "three"}, {Step: 2, Action: "four"}, {Step: 3, Action: "five"}}}
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	times, err := st.ListTaskTraces(t.Context(), task.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.db.ExecContext(t.Context(), `CREATE TRIGGER fail_trace_row BEFORE INSERT ON traces WHEN NEW.action='force-error' BEGIN SELECT RAISE(ABORT,'fixture row failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	changed := types.CloneTask(before)
	changed.Goal = "must roll back"
	changed.Status = types.StatusPaused
	changed.Trace = []types.StepTrace{{Step: 8, Action: "changed first"}, {Step: 9, Action: "force-error"}, {Step: 10, Action: "never written"}}
	if err := st.SaveFullTask(t.Context(), changed); err == nil {
		t.Fatal("injected Trace failure did not abort save")
	}
	after, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterTimes, err := st.ListTaskTraces(t.Context(), task.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(times, afterTimes) {
		t.Fatal("failed snapshot committed metadata, first row or truncation")
	}
	// Statement failure in the independent replacement path must roll back too.
	if err := st.ReplaceTraces(t.Context(), task.ID, changed.Trace); err == nil {
		t.Fatal("injected replacement failure did not abort")
	}
	after, err = st.GetTask(t.Context(), task.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed replacement changed task lifecycle")
	}
	afterTimes, err = st.ListTaskTraces(t.Context(), task.ID, 0, 10)
	if err != nil || !reflect.DeepEqual(times, afterTimes) {
		t.Fatal("failed replacement committed a partial snapshot")
	}
	// One prepared statement must be released after failure; a later valid save works.
	changed.Trace = []types.StepTrace{{Step: 9, Action: "valid"}}
	if err := st.SaveFullTask(t.Context(), changed); err != nil {
		t.Fatal(err)
	}
	after, err = st.GetTask(t.Context(), task.ID)
	if err != nil || !reflect.DeepEqual(after.Trace, changed.Trace) || after.Goal != changed.Goal {
		t.Fatal("store could not recover after statement failure")
	}
}
func TestSQLitePreparedTraceSnapshotKeepsRecordedAtAndPhysicalOrder(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "times.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := &types.Task{ID: "trace-times", Status: types.StatusRunning, StepCount: 1, Trace: []types.StepTrace{{Step: 1, Action: "one"}, {Step: 1, Action: "two"}, {Step: 1, Action: "three"}}}
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	before, err := st.ListTaskTraces(t.Context(), task.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range before {
		if row.RecordedAt == nil {
			t.Fatal("new Trace lacks recorded_at")
		}
	}
	for round := 0; round < 3; round++ {
		if round == 1 {
			task.Trace[0], task.Trace[2] = task.Trace[2], task.Trace[0]
		}
		if round == 2 {
			task.Trace = task.Trace[:2]
		}
		if err := st.SaveFullTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
		rows, err := st.ListTaskTraces(t.Context(), task.ID, 0, 10)
		if err != nil || len(rows) != len(task.Trace) {
			t.Fatal("snapshot length mismatch")
		}
		for i, row := range rows {
			if row.Sequence != int64(i+1) || !reflect.DeepEqual(row.Trace, task.Trace[i]) || !reflect.DeepEqual(row.RecordedAt, before[i].RecordedAt) {
				t.Fatal("snapshot rewrite changed position, logical step or recorded_at")
			}
		}
	}
	if err := st.ReplaceTraces(t.Context(), task.ID, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListTaskTraces(t.Context(), task.ID, 0, 10)
	if err != nil || len(rows) != 0 {
		t.Fatal("empty snapshot failed to clear traces")
	}
}
