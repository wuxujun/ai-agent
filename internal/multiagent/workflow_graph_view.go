package multiagent

import (
	"encoding/json"

	"github.com/wuxujun/ai-agent/internal/types"
)

// WorkflowGraphView contains only verified topology and node states. Opaque
// checkpoint results and errors must not be exposed by the graph endpoint.
type WorkflowGraphView struct {
	Workflow    Workflow                  `json:"workflow"`
	GraphDigest string                    `json:"graph_digest"`
	Levels      [][]WorkflowGraphViewNode `json:"levels"`
}

type WorkflowGraphViewNode struct {
	WorkflowGraphNode
	State WorkflowNodeState `json:"state"`
}

// WorkflowGraphViewFromTask uses the newest persisted DAG checkpoint. A newer
// malformed or unknown checkpoint must not silently fall back to an older graph.
func WorkflowGraphViewFromTask(task *types.Task) (WorkflowGraphView, bool) {
	if task == nil {
		return WorkflowGraphView{}, false
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action != WorkflowRuntimeCheckpointTraceAction {
			continue
		}
		var checkpoint WorkflowRuntimeCheckpoint
		if json.Unmarshal([]byte(trace.Observation), &checkpoint) != nil || checkpoint.Version != workflowRuntimeCheckpointVersion {
			return WorkflowGraphView{}, false
		}
		graph, err := BuildWorkflowGraph(checkpoint.Route)
		if err != nil || checkpoint.Workflow != graph.Workflow {
			return WorkflowGraphView{}, false
		}
		summary, err := graph.Summary()
		if err != nil || checkpoint.GraphDigest != summary.Digest {
			return WorkflowGraphView{}, false
		}
		if _, err := restoreWorkflowExecution(graph, checkpoint.Route, summary.Digest, &checkpoint); err != nil {
			return WorkflowGraphView{}, false
		}
		levels, err := graph.TopologicalLevels()
		if err != nil {
			return WorkflowGraphView{}, false
		}
		view := WorkflowGraphView{
			Workflow: graph.Workflow, GraphDigest: summary.Digest,
			Levels: make([][]WorkflowGraphViewNode, 0, len(levels)),
		}
		for _, level := range levels {
			visible := make([]WorkflowGraphViewNode, 0, len(level))
			for _, node := range level {
				state, ok := checkpoint.States[node.ID]
				if !ok {
					state = WorkflowNodePending
				}
				visible = append(visible, WorkflowGraphViewNode{WorkflowGraphNode: node, State: state})
			}
			view.Levels = append(view.Levels, visible)
		}
		return view, true
	}
	return WorkflowGraphView{}, false
}
