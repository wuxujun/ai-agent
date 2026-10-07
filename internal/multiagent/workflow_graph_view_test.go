package multiagent

import (
	"encoding/json"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestWorkflowGraphViewFromTask_VerifiedCheckpoint(t *testing.T) {
	graph, err := BuildWorkflowGraph(WorkflowResearch)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := graph.Summary()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := WorkflowRuntimeCheckpoint{
		Version: workflowRuntimeCheckpointVersion, Workflow: WorkflowResearch,
		Route: WorkflowResearch, GraphDigest: summary.Digest,
		States: map[string]WorkflowNodeState{"plan": WorkflowNodeSucceeded, "research": WorkflowNodeRunning},
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	task := &types.Task{Trace: []types.StepTrace{{Action: WorkflowRuntimeCheckpointTraceAction, Observation: string(encoded)}}}
	view, ok := WorkflowGraphViewFromTask(task)
	if !ok || view.Workflow != WorkflowResearch || view.GraphDigest != summary.Digest || len(view.Levels) != 3 {
		t.Fatalf("view = %+v, available = %t", view, ok)
	}
	if view.Levels[0][0].ID != "plan" || view.Levels[1][0].State != WorkflowNodeRunning ||
		len(view.Levels[1][0].DependsOn) != 1 || view.Levels[1][0].DependsOn[0] != "plan" ||
		view.Levels[2][0].State != WorkflowNodePending {
		t.Fatalf("unexpected node projection: %+v", view.Levels)
	}

	for _, test := range []struct {
		name   string
		change func(*WorkflowRuntimeCheckpoint)
	}{
		{"digest mismatch", func(value *WorkflowRuntimeCheckpoint) { value.GraphDigest = "other" }},
		{"invalid state", func(value *WorkflowRuntimeCheckpoint) {
			value.States = map[string]WorkflowNodeState{"plan": "invented"}
		}},
		{"invalid dependency", func(value *WorkflowRuntimeCheckpoint) {
			value.States = map[string]WorkflowNodeState{"research": WorkflowNodeSucceeded}
		}},
		{"unknown version", func(value *WorkflowRuntimeCheckpoint) { value.Version++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := checkpoint
			test.change(&candidate)
			encoded, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			task := &types.Task{Trace: []types.StepTrace{
				{Action: WorkflowRuntimeCheckpointTraceAction, Observation: string(encoded)},
			}}
			if _, ok := WorkflowGraphViewFromTask(task); ok {
				t.Fatal("invalid checkpoint unexpectedly produced a graph")
			}
		})
	}

	task.Trace = append(task.Trace, types.StepTrace{Action: WorkflowRuntimeCheckpointTraceAction, Observation: "not JSON"})
	if _, ok := WorkflowGraphViewFromTask(task); ok {
		t.Fatal("newer malformed checkpoint must suppress the older graph")
	}
	if _, ok := WorkflowGraphViewFromTask(&types.Task{}); ok {
		t.Fatal("legacy task unexpectedly produced a graph")
	}
}
