package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSQLiteTraceEventsHaveIndependentPersistentSequence(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runTraceSequenceContract(t, st)
}

func runTraceSequenceContract(t *testing.T, st Store) {
	t.Helper()
	task := &types.Task{ID: "trace-sequence-contract", Status: types.StatusRunning, StepCount: 1, MaxSteps: 3, Trace: []types.StepTrace{{Step: 1, Action: "read_file", Observation: "source"}}}
	ctx := context.Background()
	if err := st.SaveFullTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	// Execution, V2 audits, multiagent checkpoint and recovery feedback may share
	// an execution step. Their array order is the independent event sequence.
	task.Trace = append(task.Trace,
		types.StepTrace{Step: 1, Action: "citation_verify", TokenUsage: types.TokenUsage{TotalTokens: 6}},
		types.StepTrace{Step: 1, Action: "safety_guard_output", TokenUsage: types.TokenUsage{TotalTokens: 3}},
		types.StepTrace{Step: 1, Action: "multiagent_workflow_checkpoint", Observation: "running"},
		types.StepTrace{Step: 2, Action: "write_file", Observation: "recovery feedback"},
	)
	for n := 0; n < 2; n++ {
		if err := st.SaveFullTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Trace, task.Trace) || got.StepCount != 1 || got.MaxSteps != 3 {
			t.Fatalf("round trip lost events or altered budget: %+v", got)
		}
	}
	task.Trace[3].Observation = "completed"
	if err := st.SaveFullTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(ctx, task.ID)
	if err != nil || !reflect.DeepEqual(got.Trace, task.Trace) {
		t.Fatalf("checkpoint update: %+v %v", got, err)
	}
	task.Trace = task.Trace[:2]
	if err := st.SaveFullTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetTask(ctx, task.ID)
	if err != nil || !reflect.DeepEqual(got.Trace, task.Trace) {
		t.Fatalf("truncation: %+v %v", got, err)
	}
}

func TestSQLiteLegacyTraceStepPreservedDuringConversion(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := &types.Task{ID: "legacy-traces", Status: types.StatusRunning, StepCount: 7}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	for _, step := range []int{0, 7} {
		_, err := st.db.Exec(`INSERT INTO traces(task_id,step,goal,action,query,observation,evidence_json) VALUES(?,?, '','legacy','','','null')`, task.ID, step)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Trace) != 2 || got.Trace[0].Step != 0 || got.Trace[1].Step != 7 {
		t.Fatalf("legacy step changed: %+v", got.Trace)
	}
	got.Trace = append(got.Trace, types.StepTrace{Step: 7, Action: "citation_verify"})
	if err := st.SaveFullTask(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	restored, err := st.GetTask(t.Context(), task.ID)
	if err != nil || !reflect.DeepEqual(restored.Trace, got.Trace) || restored.StepCount != 7 {
		t.Fatalf("conversion lost legacy trace: %+v %v", restored, err)
	}
}
