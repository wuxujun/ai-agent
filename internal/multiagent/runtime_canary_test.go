package multiagent

import (
	"strconv"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestResolveTaskRuntimeCanaryUsesStableTaskBucket(t *testing.T) {
	snapshot := newTeamConfigSnapshot("software", TeamConfig{Runtime: RuntimeLegacy, DAGCanaryPercent: 100})
	task := &types.Task{ID: "canary-task", Goal: "inspect"}

	first := resolveTaskRuntime(task, snapshot, RuntimeLegacy)
	if first.Runtime != RuntimeDAG || first.Source != "canary" || first.Bucket < 0 || first.Bucket > 99 || len(task.Trace) != 1 {
		t.Fatalf("first selection = %+v trace=%+v", first, task.Trace)
	}
	if task.Trace[0].Action != RuntimeSelectionTraceAction || task.Trace[0].Query != string(RuntimeDAG) {
		t.Fatalf("selection trace = %+v", task.Trace[0])
	}

	// A resumed task keeps its persisted runtime even if an operator lowers the
	// percentage while using the explicit use-latest resume policy.
	changed := newTeamConfigSnapshot("software", TeamConfig{Runtime: RuntimeLegacy, DAGCanaryPercent: 0})
	resumed := resolveTaskRuntime(task, changed, RuntimeLegacy)
	if resumed.Runtime != RuntimeDAG || resumed.Bucket != first.Bucket || resumed.Source != "persisted_canary" || len(task.Trace) != 1 {
		t.Fatalf("resumed selection = %+v trace_count=%d", resumed, len(task.Trace))
	}
}

func TestResolveTaskRuntimeCanaryLeavesNonSelectedTaskOnLegacy(t *testing.T) {
	taskID := ""
	for i := 0; i < 1000; i++ {
		candidate := "legacy-task-" + strconv.Itoa(i)
		if stableRuntimeBucket("software", candidate) >= 5 {
			taskID = candidate
			break
		}
	}
	if taskID == "" {
		t.Fatal("failed to find deterministic non-canary task ID")
	}
	snapshot := newTeamConfigSnapshot("software", TeamConfig{Runtime: RuntimeLegacy, DAGCanaryPercent: 5})
	selection := resolveTaskRuntime(&types.Task{ID: taskID}, snapshot, RuntimeLegacy)
	if selection.Runtime != RuntimeLegacy || selection.Source != "canary" || selection.Bucket < 5 {
		t.Fatalf("selection = %+v", selection)
	}
}

func TestResolveTaskRuntimeCanaryIsDisabledByDefaultAndExplicitDAGWins(t *testing.T) {
	legacyTask := &types.Task{ID: "legacy-default"}
	legacy := resolveTaskRuntime(legacyTask, newTeamConfigSnapshot("software", TeamConfig{}), RuntimeLegacy)
	if legacy.Runtime != RuntimeLegacy || len(legacyTask.Trace) != 0 {
		t.Fatalf("default selection = %+v trace=%+v", legacy, legacyTask.Trace)
	}

	dagTask := &types.Task{ID: "dag-explicit"}
	dag := resolveTaskRuntime(dagTask, newTeamConfigSnapshot("software", TeamConfig{Runtime: RuntimeDAG, DAGCanaryPercent: 5}), RuntimeDAG)
	if dag.Runtime != RuntimeDAG || dag.Source != "configured" || len(dagTask.Trace) != 0 {
		t.Fatalf("explicit DAG selection = %+v trace=%+v", dag, dagTask.Trace)
	}
}

func TestRuntimeSelectionTraceStaysOutOfReplannerPrompt(t *testing.T) {
	task := &types.Task{ID: "canary-trace", Goal: "inspect"}
	selection := runtimeSelection{Version: runtimeSelectionVersion, ActiveTeam: "software", Runtime: RuntimeDAG, Source: "canary", Bucket: 2, Percent: 5}
	appendRuntimeSelection(task, selection)
	formatted := formatTracesForReplanner(task.Trace)
	if strings.Contains(formatted, RuntimeSelectionTraceAction) || strings.Contains(formatted, `"bucket":2`) {
		t.Fatalf("runtime selection leaked into replanner prompt: %s", formatted)
	}
}
