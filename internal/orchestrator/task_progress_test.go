package orchestrator

import (
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestRunAllPreservesUnflushedIncomingTrace(t *testing.T) {
	st, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "progress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := &types.Task{ID: "incoming-progress", Goal: "test", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2, Trace: []types.StepTrace{{Action: "persisted"}}}
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	task.Trace = append(task.Trace, types.StepTrace{Action: "unflushed-recovery"})
	engine := &Engine{Mode: ModeLegacy, Store: st, Planner: &stubPlanner{decision: &planner.PlanDecision{Actions: []planner.ActionCall{{Action: "read_file", Parameters: map[string]any{"path": "file.txt"}}}}}, Executor: &stubExecutor{trace: &types.StepTrace{Action: "read_file", Observation: "ok"}}}
	if err := engine.RunAll(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Trace) < 4 || loaded.Trace[0].Action != "persisted" || loaded.Trace[1].Action != "unflushed-recovery" {
		t.Fatalf("lost incoming trace: %+v", loaded.Trace)
	}
}
