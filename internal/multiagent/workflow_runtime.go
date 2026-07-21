package multiagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

const (
	workflowRuntimeCheckpointVersion = 1
	workflowNodeResultMaxBytes       = 256 * 1024
	workflowResultsMaxBytes          = 1024 * 1024
	workflowErrorMaxRunes            = 2048
)

type WorkflowNodeState string

const (
	WorkflowNodePending   WorkflowNodeState = "pending"
	WorkflowNodeRunning   WorkflowNodeState = "running"
	WorkflowNodeSucceeded WorkflowNodeState = "succeeded"
	WorkflowNodeSkipped   WorkflowNodeState = "skipped"
	WorkflowNodeFailed    WorkflowNodeState = "failed"
)

// WorkflowNodeResult is the persistable output passed to dependent nodes.
// Approved is control metadata used by the built-in approved condition; Data
// remains opaque to the runtime.
type WorkflowNodeResult struct {
	Data     json.RawMessage `json:"data,omitempty"`
	Approved *bool           `json:"approved,omitempty"`
}

type WorkflowRuntimeCheckpoint struct {
	Version     int                           `json:"version"`
	Workflow    Workflow                      `json:"workflow"`
	Route       Workflow                      `json:"route"`
	GraphDigest string                        `json:"graph_digest"`
	ActiveTeam  string                        `json:"active_team,omitempty"`
	TeamDigest  string                        `json:"team_digest,omitempty"`
	States      map[string]WorkflowNodeState  `json:"states"`
	Results     map[string]WorkflowNodeResult `json:"results,omitempty"`
	Errors      map[string]string             `json:"errors,omitempty"`
}

type WorkflowExecution struct {
	Workflow    Workflow
	Route       Workflow
	GraphDigest string
	States      map[string]WorkflowNodeState
	Results     map[string]WorkflowNodeResult
	Errors      map[string]string
}

type WorkflowNodeExecutor func(context.Context, WorkflowGraphNode, map[string]WorkflowNodeResult) (WorkflowNodeResult, error)

type WorkflowRuntime struct {
	MaxConcurrency int
	SaveCheckpoint func(WorkflowRuntimeCheckpoint) error
}

type WorkflowNodeError struct {
	NodeID string
	Err    error
}

func (e *WorkflowNodeError) Error() string {
	return fmt.Sprintf("workflow node %q failed: %v", e.NodeID, e.Err)
}

func (e *WorkflowNodeError) Unwrap() error {
	return e.Err
}

// Run executes each topological level in order. Compatible nodes within a
// level run concurrently up to MaxConcurrency. A failed node cancels its
// siblings and leaves later levels pending for a resumable retry.
func (r WorkflowRuntime) Run(ctx context.Context, graph WorkflowGraph, route Workflow, checkpoint *WorkflowRuntimeCheckpoint, execute WorkflowNodeExecutor) (*WorkflowExecution, error) {
	if execute == nil {
		return nil, fmt.Errorf("workflow node executor is nil")
	}
	levels, err := graph.TopologicalLevels()
	if err != nil {
		return nil, err
	}
	summary, err := graph.Summary()
	if err != nil {
		return nil, err
	}
	execution, err := restoreWorkflowExecution(graph, route, summary.Digest, checkpoint)
	if err != nil {
		return nil, err
	}
	for _, level := range levels {
		if err := ctx.Err(); err != nil {
			return execution, err
		}
		active := make([]WorkflowGraphNode, 0, len(level))
		for _, node := range level {
			state := execution.States[node.ID]
			if state == WorkflowNodeSucceeded || state == WorkflowNodeSkipped {
				continue
			}
			dependenciesReady := true
			for _, dependency := range node.DependsOn {
				dependencyState := execution.States[dependency]
				if dependencyState == WorkflowNodeSkipped {
					execution.States[node.ID] = WorkflowNodeSkipped
					dependenciesReady = false
					break
				}
				if dependencyState != WorkflowNodeSucceeded {
					return execution, fmt.Errorf("workflow node %q dependency %q is %q", node.ID, dependency, dependencyState)
				}
			}
			if !dependenciesReady {
				continue
			}
			activated, conditionErr := evaluateWorkflowNodeCondition(node, route, execution.Results)
			if conditionErr != nil {
				execution.States[node.ID] = WorkflowNodeFailed
				execution.Errors[node.ID] = boundedWorkflowError(conditionErr)
				if saveErr := r.save(execution); saveErr != nil {
					return execution, saveErr
				}
				return execution, &WorkflowNodeError{NodeID: node.ID, Err: conditionErr}
			}
			if !activated {
				execution.States[node.ID] = WorkflowNodeSkipped
				continue
			}
			execution.States[node.ID] = WorkflowNodeRunning
			delete(execution.Errors, node.ID)
			active = append(active, node)
		}
		if len(active) == 0 {
			if err := r.save(execution); err != nil {
				return execution, err
			}
			continue
		}
		if err := r.save(execution); err != nil {
			return execution, err
		}
		slots := r.executeLevel(ctx, active, execution.Results, execute)
		var firstFailure *WorkflowNodeError
		for i, node := range active {
			slot := slots[i]
			if slot.err == nil {
				slot.err = validateWorkflowNodeResult(slot.result)
				if slot.err == nil && workflowResultsSize(execution.Results)+len(slot.result.Data) > workflowResultsMaxBytes {
					slot.err = fmt.Errorf("workflow results exceed %d bytes", workflowResultsMaxBytes)
				}
			}
			if slot.err != nil {
				execution.States[node.ID] = WorkflowNodeFailed
				execution.Errors[node.ID] = boundedWorkflowError(slot.err)
				candidate := &WorkflowNodeError{NodeID: node.ID, Err: slot.err}
				if firstFailure == nil || (isContextError(firstFailure.Err) && !isContextError(slot.err)) {
					firstFailure = candidate
				}
				continue
			}
			execution.States[node.ID] = WorkflowNodeSucceeded
			execution.Results[node.ID] = cloneWorkflowNodeResult(slot.result)
			delete(execution.Errors, node.ID)
		}
		if err := r.save(execution); err != nil {
			return execution, err
		}
		if firstFailure != nil {
			return execution, firstFailure
		}
	}
	return execution, nil
}

type workflowExecutionSlot struct {
	result WorkflowNodeResult
	err    error
}

func (r WorkflowRuntime) executeLevel(ctx context.Context, nodes []WorkflowGraphNode, results map[string]WorkflowNodeResult, execute WorkflowNodeExecutor) []workflowExecutionSlot {
	limit := r.MaxConcurrency
	if limit <= 0 {
		limit = 1
	}
	if limit > len(nodes) {
		limit = len(nodes)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	semaphore := make(chan struct{}, limit)
	slots := make([]workflowExecutionSlot, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		go func(index int, current WorkflowGraphNode) {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-runCtx.Done():
				slots[index].err = runCtx.Err()
				return
			}
			if err := runCtx.Err(); err != nil {
				slots[index].err = err
				return
			}
			dependencyResults := make(map[string]WorkflowNodeResult, len(current.DependsOn))
			for _, dependency := range current.DependsOn {
				dependencyResults[dependency] = cloneWorkflowNodeResult(results[dependency])
			}
			result, err := execute(runCtx, current, dependencyResults)
			slots[index] = workflowExecutionSlot{result: cloneWorkflowNodeResult(result), err: err}
			if err != nil {
				cancel()
			}
		}(i, node)
	}
	wg.Wait()
	return slots
}

func evaluateWorkflowNodeCondition(node WorkflowGraphNode, route Workflow, results map[string]WorkflowNodeResult) (bool, error) {
	switch node.Condition {
	case WorkflowConditionAlways:
		return true, nil
	case WorkflowConditionRouteResearch:
		if route != WorkflowResearch && route != WorkflowReviewed {
			return false, fmt.Errorf("route condition requires an effective workflow, got %q", route)
		}
		return route == WorkflowResearch, nil
	case WorkflowConditionRouteReviewed:
		if route != WorkflowResearch && route != WorkflowReviewed {
			return false, fmt.Errorf("route condition requires an effective workflow, got %q", route)
		}
		return route == WorkflowReviewed, nil
	case WorkflowConditionApproved:
		found := false
		for _, dependency := range node.DependsOn {
			approved := results[dependency].Approved
			if approved == nil {
				continue
			}
			found = true
			if !*approved {
				return false, nil
			}
		}
		if !found {
			return false, fmt.Errorf("approved condition has no dependency approval result")
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported workflow condition %q", node.Condition)
	}
}

func restoreWorkflowExecution(graph WorkflowGraph, route Workflow, digest string, checkpoint *WorkflowRuntimeCheckpoint) (*WorkflowExecution, error) {
	execution := &WorkflowExecution{
		Workflow: graph.Workflow, Route: route, GraphDigest: digest,
		States:  make(map[string]WorkflowNodeState, len(graph.Nodes)),
		Results: make(map[string]WorkflowNodeResult), Errors: make(map[string]string),
	}
	knownNodes := make(map[string]WorkflowGraphNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		knownNodes[node.ID] = node
		execution.States[node.ID] = WorkflowNodePending
	}
	if checkpoint == nil {
		return execution, nil
	}
	if checkpoint.Version != workflowRuntimeCheckpointVersion {
		return nil, fmt.Errorf("unsupported workflow checkpoint version %d", checkpoint.Version)
	}
	if checkpoint.Workflow != graph.Workflow || checkpoint.Route != route || checkpoint.GraphDigest != digest {
		return nil, fmt.Errorf("workflow checkpoint does not match runtime: workflow=%q route=%q digest=%q", checkpoint.Workflow, checkpoint.Route, checkpoint.GraphDigest)
	}
	for id, state := range checkpoint.States {
		if _, ok := knownNodes[id]; !ok {
			return nil, fmt.Errorf("workflow checkpoint contains unknown node %q", id)
		}
		switch state {
		case WorkflowNodePending, WorkflowNodeRunning, WorkflowNodeFailed:
			execution.States[id] = WorkflowNodePending
		case WorkflowNodeSucceeded, WorkflowNodeSkipped:
			execution.States[id] = state
		default:
			return nil, fmt.Errorf("workflow checkpoint node %q has invalid state %q", id, state)
		}
	}
	for id, result := range checkpoint.Results {
		if _, ok := knownNodes[id]; !ok {
			return nil, fmt.Errorf("workflow checkpoint contains result for unknown node %q", id)
		}
		if execution.States[id] != WorkflowNodeSucceeded {
			return nil, fmt.Errorf("workflow checkpoint contains result for node %q in state %q", id, execution.States[id])
		}
		if err := validateWorkflowNodeResult(result); err != nil {
			return nil, fmt.Errorf("workflow checkpoint node %q result: %w", id, err)
		}
		execution.Results[id] = cloneWorkflowNodeResult(result)
	}
	if workflowResultsSize(execution.Results) > workflowResultsMaxBytes {
		return nil, fmt.Errorf("workflow checkpoint results exceed %d bytes", workflowResultsMaxBytes)
	}
	for id, node := range knownNodes {
		if execution.States[id] != WorkflowNodeSucceeded {
			continue
		}
		for _, dependency := range node.DependsOn {
			if execution.States[dependency] != WorkflowNodeSucceeded {
				return nil, fmt.Errorf("workflow checkpoint succeeded node %q has incomplete dependency %q", id, dependency)
			}
		}
	}
	return execution, nil
}

func (r WorkflowRuntime) save(execution *WorkflowExecution) error {
	if r.SaveCheckpoint == nil {
		return nil
	}
	if err := r.SaveCheckpoint(execution.Checkpoint()); err != nil {
		return fmt.Errorf("save workflow checkpoint: %w", err)
	}
	return nil
}

func (e *WorkflowExecution) Checkpoint() WorkflowRuntimeCheckpoint {
	return WorkflowRuntimeCheckpoint{
		Version: workflowRuntimeCheckpointVersion, Workflow: e.Workflow, Route: e.Route, GraphDigest: e.GraphDigest,
		States: cloneWorkflowNodeStates(e.States), Results: cloneWorkflowNodeResults(e.Results), Errors: cloneWorkflowNodeErrors(e.Errors),
	}
}

func cloneWorkflowNodeStates(source map[string]WorkflowNodeState) map[string]WorkflowNodeState {
	cloned := make(map[string]WorkflowNodeState, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func cloneWorkflowNodeResults(source map[string]WorkflowNodeResult) map[string]WorkflowNodeResult {
	cloned := make(map[string]WorkflowNodeResult, len(source))
	for key, value := range source {
		cloned[key] = cloneWorkflowNodeResult(value)
	}
	return cloned
}

func cloneWorkflowNodeResult(source WorkflowNodeResult) WorkflowNodeResult {
	cloned := WorkflowNodeResult{Data: append(json.RawMessage(nil), source.Data...)}
	if source.Approved != nil {
		approved := *source.Approved
		cloned.Approved = &approved
	}
	return cloned
}

func cloneWorkflowNodeErrors(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func validateWorkflowNodeResult(result WorkflowNodeResult) error {
	if len(result.Data) > workflowNodeResultMaxBytes {
		return fmt.Errorf("node result exceeds %d bytes", workflowNodeResultMaxBytes)
	}
	if len(result.Data) > 0 && !json.Valid(result.Data) {
		return fmt.Errorf("node result is not valid JSON")
	}
	return nil
}

func workflowResultsSize(results map[string]WorkflowNodeResult) int {
	total := 0
	for _, result := range results {
		total += len(result.Data)
	}
	return total
}

func boundedWorkflowError(err error) string {
	if err == nil {
		return ""
	}
	runes := []rune(err.Error())
	if len(runes) > workflowErrorMaxRunes {
		runes = runes[:workflowErrorMaxRunes]
	}
	return string(runes)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
