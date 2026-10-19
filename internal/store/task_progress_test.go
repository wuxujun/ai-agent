package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSQLiteTaskProgressPreservesPrefixAndHandlesRewrites(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "progress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := capacityTask(3)
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	before, err := st.ListTaskTraces(t.Context(), task.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := CaptureTaskProgress(st, task)
	if _, err := st.db.ExecContext(t.Context(), `CREATE TRIGGER reject_prefix_insert BEFORE INSERT ON traces WHEN NEW.step<=3 BEGIN SELECT RAISE(ABORT,'old prefix must not be resubmitted'); END`); err != nil {
		t.Fatal(err)
	}
	other, err := NewSQLiteStore(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.SaveTaskProgress(t.Context(), task, checkpoint); !errors.Is(err, ErrTaskProgressConflict) {
		t.Fatalf("foreign store checkpoint: %v", err)
	}
	if err := st.SaveTaskProgress(WithTaskLease(t.Context(), task.ID, "missing"), task, checkpoint); !errors.Is(err, ErrTaskLeaseLost) {
		t.Fatalf("unowned checkpoint: %v", err)
	}
	if ok, err := st.AcquireTaskLease(t.Context(), task.ID, "live", time.Minute); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	if err := st.SaveTaskProgress(WithTaskLease(t.Context(), task.ID, "other"), task, checkpoint); !errors.Is(err, ErrTaskLeaseLost) {
		t.Fatalf("wrong owner: %v", err)
	}
	if err := st.ReleaseTaskLease(t.Context(), task.ID, "live"); err != nil {
		t.Fatal(err)
	}
	task.Trace = append(task.Trace, types.StepTrace{Step: 9, Action: "new"})
	task.StepCount++
	if err := SaveTaskProgress(t.Context(), st, task, checkpoint); err != nil {
		t.Fatal(err)
	}
	after, err := st.ListTaskTraces(t.Context(), task.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 4 || !reflect.DeepEqual(before, after[:3]) || after[3].Sequence != 4 {
		t.Fatal("append changed prefix or order")
	}
	if _, err := st.db.ExecContext(t.Context(), `DROP TRIGGER reject_prefix_insert`); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(){
		func() { task.Trace[0].Evidence[0].Lines[0] = "changed evidence" },
		func() { task.Trace[0], task.Trace[1] = task.Trace[1], task.Trace[0] },
		func() { task.Trace = task.Trace[:1] },
	} {
		checkpoint = CaptureTaskProgress(st, task)
		change()
		if err := SaveTaskProgress(t.Context(), st, task, checkpoint); err != nil {
			t.Fatal(err)
		}
		loaded, err := st.GetTask(t.Context(), task.ID)
		if err != nil || !reflect.DeepEqual(loaded.Trace, task.Trace) {
			t.Fatalf("rewrite lost: %+v %v", loaded, err)
		}
	}
}

func TestSQLiteTaskProgressRejectsStaleVersionAndRollsBack(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "progress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := capacityTask(2)
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	checkpoint := CaptureTaskProgress(st, task)
	peer := types.CloneTask(task)
	peer.Goal = "peer"
	if err := st.SaveFullTask(t.Context(), peer); err != nil {
		t.Fatal(err)
	}
	task.Trace = append(task.Trace, types.StepTrace{Action: "stale"})
	if err := st.SaveTaskProgress(t.Context(), task, checkpoint); !errors.Is(err, ErrTaskProgressConflict) {
		t.Fatalf("stale: %v", err)
	}
	loaded, err := st.GetTask(t.Context(), task.ID)
	if err != nil || loaded.Goal != "peer" || len(loaded.Trace) != 2 {
		t.Fatal("stale checkpoint modified task")
	}
	task = loaded
	checkpoint = CaptureTaskProgress(st, task)
	before, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(t.Context(), `CREATE TRIGGER reject_progress BEFORE INSERT ON traces WHEN NEW.action='reject' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	task.Goal = "failed metadata"
	task.Trace = append(task.Trace, types.StepTrace{Action: "valid"}, types.StepTrace{Action: "reject"})
	if err := st.SaveTaskProgress(t.Context(), task, checkpoint); err == nil {
		t.Fatal("expected rollback")
	}
	loaded, err = st.GetTask(t.Context(), task.ID)
	if err != nil || !reflect.DeepEqual(loaded, before) {
		t.Fatal("failed append changed metadata or trace")
	}
}

func BenchmarkTaskAppendProgress(b *testing.B) {
	for _, rows := range []int{1000, 10000} {
		for _, mode := range []string{"full", "progress"} {
			b.Run(fmt.Sprintf("sqlite/traces_%d/%s", rows, mode), func(b *testing.B) {
				st, _ := capacityStore(b, "sqlite")
				task := capacityTask(rows)
				if err := st.(TaskCreationStore).CreateTask(b.Context(), task); err != nil {
					b.Fatal(err)
				}
				capacityMeasure(b, func() error {
					var checkpoint *TaskProgressCheckpoint
					if mode == "progress" {
						checkpoint = CaptureTaskProgress(st, task)
					}
					task.Trace = append(task.Trace, types.StepTrace{Step: len(task.Trace) + 1, Action: "read_file", Observation: "appended row"})
					task.StepCount++
					if mode == "progress" {
						return SaveTaskProgress(b.Context(), st, task, checkpoint)
					}
					return st.SaveFullTask(b.Context(), task)
				})
			})
		}
	}
}
