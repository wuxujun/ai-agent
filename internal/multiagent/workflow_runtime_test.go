package multiagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestWorkflowRuntime_ExecutesSerialGraphAndPassesDependencyResults(t *testing.T) {
	graph, err := BuildWorkflowGraph(WorkflowResearch)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	runtime := WorkflowRuntime{MaxConcurrency: 3}
	execution, err := runtime.Run(context.Background(), graph, WorkflowResearch, nil, func(_ context.Context, node WorkflowGraphNode, dependencies map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
		order = append(order, node.ID)
		if len(node.DependsOn) > 0 {
			dependency := node.DependsOn[0]
			if string(dependencies[dependency].Data) != fmt.Sprintf(`{"node":%q}`, dependency) {
				t.Fatalf("node %q dependencies = %+v", node.ID, dependencies)
			}
		}
		return WorkflowNodeResult{Data: json.RawMessage(fmt.Sprintf(`{"node":%q}`, node.ID))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "plan,research,write" {
		t.Fatalf("execution order = %v", order)
	}
	for _, node := range graph.Nodes {
		if execution.States[node.ID] != WorkflowNodeSucceeded {
			t.Fatalf("node %q state = %q", node.ID, execution.States[node.ID])
		}
	}
}

func TestWorkflowRuntime_ActivatesAdaptiveRouteBranches(t *testing.T) {
	graph, err := BuildWorkflowGraph(WorkflowAdaptive)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		route       Workflow
		wantCalls   string
		wantSkipped []string
	}{
		{"research", WorkflowResearch, "plan,research,write", []string{"critique", "execute", "verify"}},
		{"reviewed", WorkflowReviewed, "plan,critique,execute,verify", []string{"research", "write"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			runtime := WorkflowRuntime{MaxConcurrency: 2}
			execution, runErr := runtime.Run(context.Background(), graph, tt.route, nil, func(_ context.Context, node WorkflowGraphNode, _ map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
				mu.Lock()
				calls = append(calls, node.ID)
				mu.Unlock()
				result := WorkflowNodeResult{}
				if node.ID == "critique" {
					approved := true
					result.Approved = &approved
				}
				return result, nil
			})
			if runErr != nil {
				t.Fatal(runErr)
			}
			if strings.Join(calls, ",") != tt.wantCalls {
				t.Fatalf("calls = %v, want %s", calls, tt.wantCalls)
			}
			for _, id := range tt.wantSkipped {
				if execution.States[id] != WorkflowNodeSkipped {
					t.Fatalf("node %q state = %q", id, execution.States[id])
				}
			}
		})
	}
}

func TestWorkflowRuntime_ApprovedConditionRequiresControlResult(t *testing.T) {
	graph, err := BuildWorkflowGraph(WorkflowReviewed)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := (WorkflowRuntime{}).Run(context.Background(), graph, WorkflowReviewed, nil, func(_ context.Context, node WorkflowGraphNode, _ map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
		return WorkflowNodeResult{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "no dependency approval result") {
		t.Fatalf("error = %v", err)
	}
	if execution.States["execute"] != WorkflowNodeFailed || execution.States["verify"] != WorkflowNodePending {
		t.Fatalf("states = %+v", execution.States)
	}
}

func TestWorkflowRuntime_ApprovedFalseSkipsDownstream(t *testing.T) {
	graph, err := BuildWorkflowGraph(WorkflowReviewed)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := (WorkflowRuntime{}).Run(context.Background(), graph, WorkflowReviewed, nil, func(_ context.Context, node WorkflowGraphNode, _ map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
		result := WorkflowNodeResult{}
		if node.ID == "critique" {
			approved := false
			result.Approved = &approved
		}
		return result, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if execution.States["execute"] != WorkflowNodeSkipped || execution.States["verify"] != WorkflowNodeSkipped {
		t.Fatalf("states = %+v", execution.States)
	}
}

func TestWorkflowRuntime_RespectsConcurrencyLimit(t *testing.T) {
	graph := WorkflowGraph{Workflow: WorkflowResearch, Nodes: []WorkflowGraphNode{
		{ID: "root", Role: RolePlanner, Condition: WorkflowConditionAlways},
		{ID: "left", Role: RoleResearcher, DependsOn: []string{"root"}, Condition: WorkflowConditionAlways},
		{ID: "middle", Role: RoleResearcher, DependsOn: []string{"root"}, Condition: WorkflowConditionAlways},
		{ID: "right", Role: RoleResearcher, DependsOn: []string{"root"}, Condition: WorkflowConditionAlways},
	}}
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	done := make(chan error, 1)
	var current atomic.Int32
	var maximum atomic.Int32
	go func() {
		_, err := (WorkflowRuntime{MaxConcurrency: 2}).Run(context.Background(), graph, WorkflowResearch, nil, func(_ context.Context, node WorkflowGraphNode, _ map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
			if node.ID == "root" {
				return WorkflowNodeResult{}, nil
			}
			running := current.Add(1)
			defer current.Add(-1)
			for {
				observed := maximum.Load()
				if running <= observed || maximum.CompareAndSwap(observed, running) {
					break
				}
			}
			started <- struct{}{}
			<-release
			return WorkflowNodeResult{}, nil
		})
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for concurrent nodes")
		}
	}
	select {
	case <-started:
		t.Fatal("third node started before a concurrency slot was released")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d", maximum.Load())
	}
}

func TestWorkflowRuntime_FailureCancelsSiblingAndLeavesDescendantsPending(t *testing.T) {
	graph := WorkflowGraph{Workflow: WorkflowResearch, Nodes: []WorkflowGraphNode{
		{ID: "root", Role: RolePlanner, Condition: WorkflowConditionAlways},
		{ID: "fail", Role: RoleResearcher, DependsOn: []string{"root"}, Condition: WorkflowConditionAlways},
		{ID: "wait", Role: RoleResearcher, DependsOn: []string{"root"}, Condition: WorkflowConditionAlways},
		{ID: "join", Role: RoleWriter, DependsOn: []string{"fail", "wait"}, Condition: WorkflowConditionAlways},
	}}
	waitStarted := make(chan struct{})
	execution, err := (WorkflowRuntime{MaxConcurrency: 2}).Run(context.Background(), graph, WorkflowResearch, nil, func(ctx context.Context, node WorkflowGraphNode, _ map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
		switch node.ID {
		case "wait":
			close(waitStarted)
			<-ctx.Done()
			return WorkflowNodeResult{}, ctx.Err()
		case "fail":
			<-waitStarted
			return WorkflowNodeResult{}, errors.New("hard failure")
		default:
			return WorkflowNodeResult{}, nil
		}
	})
	if err == nil || !strings.Contains(err.Error(), "hard failure") {
		t.Fatalf("error = %v", err)
	}
	if execution.States["fail"] != WorkflowNodeFailed || execution.States["wait"] != WorkflowNodeFailed || execution.States["join"] != WorkflowNodePending {
		t.Fatalf("states = %+v", execution.States)
	}
}

func TestWorkflowRuntime_ResumesCompletedNodesAndRetriesFailure(t *testing.T) {
	graph, err := BuildWorkflowGraph(WorkflowResearch)
	if err != nil {
		t.Fatal(err)
	}
	var latest WorkflowRuntimeCheckpoint
	firstCalls := make(map[string]int)
	runtime := WorkflowRuntime{SaveCheckpoint: func(checkpoint WorkflowRuntimeCheckpoint) error {
		latest = checkpoint
		return nil
	}}
	_, err = runtime.Run(context.Background(), graph, WorkflowResearch, nil, func(_ context.Context, node WorkflowGraphNode, _ map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
		firstCalls[node.ID]++
		if node.ID == "research" {
			return WorkflowNodeResult{}, errors.New("temporary")
		}
		return WorkflowNodeResult{Data: json.RawMessage(`{"saved":true}`)}, nil
	})
	if err == nil {
		t.Fatal("expected first run to fail")
	}
	secondCalls := make(map[string]int)
	execution, err := runtime.Run(context.Background(), graph, WorkflowResearch, &latest, func(_ context.Context, node WorkflowGraphNode, dependencies map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
		secondCalls[node.ID]++
		if node.ID == "research" && string(dependencies["plan"].Data) != `{"saved":true}` {
			t.Fatalf("restored dependency result = %s", dependencies["plan"].Data)
		}
		return WorkflowNodeResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstCalls["plan"] != 1 || secondCalls["plan"] != 0 || secondCalls["research"] != 1 || secondCalls["write"] != 1 {
		t.Fatalf("first calls = %+v, second calls = %+v", firstCalls, secondCalls)
	}
	if execution.States["write"] != WorkflowNodeSucceeded {
		t.Fatalf("states = %+v", execution.States)
	}
}

func TestWorkflowRuntime_RejectsMismatchedOrCorruptCheckpoint(t *testing.T) {
	graph, err := BuildWorkflowGraph(WorkflowResearch)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := graph.Summary()
	if err != nil {
		t.Fatal(err)
	}
	valid := WorkflowRuntimeCheckpoint{
		Version: workflowRuntimeCheckpointVersion, Workflow: WorkflowResearch, Route: WorkflowResearch,
		GraphDigest: summary.Digest, States: map[string]WorkflowNodeState{"plan": WorkflowNodeSucceeded},
	}
	tests := []struct {
		name       string
		checkpoint WorkflowRuntimeCheckpoint
	}{
		{"digest", func() WorkflowRuntimeCheckpoint { value := valid; value.GraphDigest = "changed"; return value }()},
		{"unknown node", func() WorkflowRuntimeCheckpoint {
			value := valid
			value.States = map[string]WorkflowNodeState{"unknown": WorkflowNodeSucceeded}
			return value
		}()},
		{"invalid state", func() WorkflowRuntimeCheckpoint {
			value := valid
			value.States = map[string]WorkflowNodeState{"plan": "invalid"}
			return value
		}()},
		{"incomplete dependency", func() WorkflowRuntimeCheckpoint {
			value := valid
			value.States = map[string]WorkflowNodeState{"research": WorkflowNodeSucceeded}
			return value
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, runErr := (WorkflowRuntime{}).Run(context.Background(), graph, WorkflowResearch, &tt.checkpoint, func(context.Context, WorkflowGraphNode, map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
				return WorkflowNodeResult{}, nil
			}); runErr == nil {
				t.Fatal("expected checkpoint validation error")
			}
		})
	}
}

func TestWorkflowRuntime_RejectsInvalidNodeResult(t *testing.T) {
	graph, err := BuildWorkflowGraph(WorkflowResearch)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := (WorkflowRuntime{}).Run(context.Background(), graph, WorkflowResearch, nil, func(_ context.Context, node WorkflowGraphNode, _ map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
		if node.ID == "plan" {
			return WorkflowNodeResult{Data: json.RawMessage(`not-json`)}, nil
		}
		return WorkflowNodeResult{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("error = %v", err)
	}
	if execution.States["plan"] != WorkflowNodeFailed || execution.States["research"] != WorkflowNodePending {
		t.Fatalf("states = %+v", execution.States)
	}
}

func TestWorkflowRuntimeCheckpointTrace_UpsertsAndStaysOutOfReplanner(t *testing.T) {
	task := &types.Task{ID: "runtime-checkpoint", Goal: "test", StepCount: 3}
	checkpoint := WorkflowRuntimeCheckpoint{
		Version: workflowRuntimeCheckpointVersion, Workflow: WorkflowAdaptive, Route: WorkflowResearch, GraphDigest: "digest-a",
		States: map[string]WorkflowNodeState{"plan": WorkflowNodeRunning},
	}
	if err := upsertWorkflowRuntimeCheckpoint(task, checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.States["plan"] = WorkflowNodeSucceeded
	checkpoint.Results = map[string]WorkflowNodeResult{"plan": {Data: json.RawMessage(`{"private":"result"}`)}}
	if err := upsertWorkflowRuntimeCheckpoint(task, checkpoint); err != nil {
		t.Fatal(err)
	}
	if len(task.Trace) != 1 || task.StepCount != 4 {
		t.Fatalf("trace count = %d, step count = %d", len(task.Trace), task.StepCount)
	}
	persisted, ok := persistedWorkflowRuntimeCheckpoint(task, "digest-a", WorkflowResearch)
	if !ok || persisted.States["plan"] != WorkflowNodeSucceeded || string(persisted.Results["plan"].Data) != `{"private":"result"}` {
		t.Fatalf("persisted checkpoint = %+v, ok=%v", persisted, ok)
	}
	formatted := formatTracesForReplanner(task.Trace)
	if strings.Contains(formatted, WorkflowRuntimeCheckpointTraceAction) || strings.Contains(formatted, "private") {
		t.Fatalf("checkpoint leaked into replanner prompt: %s", formatted)
	}
}

func TestWorkflowRuntimeCheckpointTrace_ScopesResumeToTeamDigest(t *testing.T) {
	task := &types.Task{ID: "runtime-team-scope"}
	checkpoint := WorkflowRuntimeCheckpoint{
		Version: workflowRuntimeCheckpointVersion, Workflow: WorkflowResearch, Route: WorkflowResearch,
		GraphDigest: "same-graph", ActiveTeam: "old-team", TeamDigest: "old-digest",
		States: map[string]WorkflowNodeState{"plan": WorkflowNodeSucceeded},
	}
	if err := upsertWorkflowRuntimeCheckpoint(task, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, ok := persistedWorkflowRuntimeCheckpoint(task, "same-graph", WorkflowResearch, "old-digest"); !ok {
		t.Fatal("checkpoint was not found for matching team digest")
	}
	if _, ok := persistedWorkflowRuntimeCheckpoint(task, "same-graph", WorkflowResearch, "new-digest"); ok {
		t.Fatal("checkpoint from old team digest was reused")
	}
}
