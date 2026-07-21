package multiagent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

const WorkflowRuntimeCheckpointTraceAction = "multiagent_workflow_checkpoint"

// upsertWorkflowRuntimeCheckpoint keeps one latest checkpoint per graph and
// route. It intentionally does not add a new task step for every node state
// transition.
func upsertWorkflowRuntimeCheckpoint(task *types.Task, checkpoint WorkflowRuntimeCheckpoint) error {
	if task == nil {
		return fmt.Errorf("task is nil")
	}
	if checkpoint.Version != workflowRuntimeCheckpointVersion || strings.TrimSpace(checkpoint.GraphDigest) == "" {
		return fmt.Errorf("workflow checkpoint metadata is invalid")
	}
	for id, result := range checkpoint.Results {
		if err := validateWorkflowNodeResult(result); err != nil {
			return fmt.Errorf("workflow checkpoint node %q result: %w", id, err)
		}
	}
	if workflowResultsSize(checkpoint.Results) > workflowResultsMaxBytes {
		return fmt.Errorf("workflow checkpoint results exceed %d bytes", workflowResultsMaxBytes)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("marshal workflow checkpoint: %w", err)
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := &task.Trace[i]
		if trace.Action != WorkflowRuntimeCheckpointTraceAction {
			continue
		}
		var persisted WorkflowRuntimeCheckpoint
		if json.Unmarshal([]byte(trace.Observation), &persisted) == nil && persisted.GraphDigest == checkpoint.GraphDigest && persisted.Route == checkpoint.Route {
			trace.Observation = string(encoded)
			trace.Query = string(checkpoint.Workflow)
			trace.Evidence = workflowCheckpointEvidence(checkpoint)
			return nil
		}
	}
	task.Trace = append(task.Trace, types.StepTrace{
		Step: task.StepCount, Goal: task.Goal, Action: WorkflowRuntimeCheckpointTraceAction,
		Query: string(checkpoint.Workflow), Observation: string(encoded), AgentRole: RolePlanner,
		Evidence: workflowCheckpointEvidence(checkpoint),
	})
	task.StepCount++
	return nil
}

func persistedWorkflowRuntimeCheckpoint(task *types.Task, graphDigest string, route Workflow, teamDigest ...string) (WorkflowRuntimeCheckpoint, bool) {
	if task == nil || strings.TrimSpace(graphDigest) == "" {
		return WorkflowRuntimeCheckpoint{}, false
	}
	expectedTeamDigest := ""
	if len(teamDigest) > 0 {
		expectedTeamDigest = strings.TrimSpace(teamDigest[0])
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action != WorkflowRuntimeCheckpointTraceAction {
			continue
		}
		var checkpoint WorkflowRuntimeCheckpoint
		if json.Unmarshal([]byte(trace.Observation), &checkpoint) != nil {
			continue
		}
		if checkpoint.Version == workflowRuntimeCheckpointVersion && checkpoint.GraphDigest == graphDigest && checkpoint.Route == route && (expectedTeamDigest == "" || checkpoint.TeamDigest == expectedTeamDigest) {
			return checkpoint, true
		}
	}
	return WorkflowRuntimeCheckpoint{}, false
}

func workflowCheckpointEvidence(checkpoint WorkflowRuntimeCheckpoint) []types.Evidence {
	evidence := []types.Evidence{{
		Path: "workflow_graph", Query: string(checkpoint.Route),
		Lines: []string{"digest:" + checkpoint.GraphDigest, fmt.Sprintf("version:%d", checkpoint.Version)},
	}}
	if strings.TrimSpace(checkpoint.TeamDigest) != "" {
		evidence = append(evidence, types.Evidence{
			Path: "team_config", Query: checkpoint.ActiveTeam,
			Lines: []string{"digest:" + checkpoint.TeamDigest},
		})
	}
	return evidence
}
