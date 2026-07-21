package multiagent

import (
	"context"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestBuildWorkflowGraph_Builtins(t *testing.T) {
	tests := []struct {
		workflow          Workflow
		wantLevels        int
		wantWidth         int
		wantConditional   int
		wantNodesPerLevel []int
	}{
		{WorkflowResearch, 3, 1, 0, []int{1, 1, 1}},
		{WorkflowReviewed, 4, 1, 1, []int{1, 1, 1, 1}},
		{WorkflowAdaptive, 4, 2, 3, []int{1, 2, 2, 1}},
	}
	for _, tt := range tests {
		t.Run(string(tt.workflow), func(t *testing.T) {
			graph, err := BuildWorkflowGraph(tt.workflow)
			if err != nil {
				t.Fatal(err)
			}
			levels, err := graph.TopologicalLevels()
			if err != nil {
				t.Fatal(err)
			}
			if len(levels) != tt.wantLevels {
				t.Fatalf("levels = %d, want %d", len(levels), tt.wantLevels)
			}
			for i, want := range tt.wantNodesPerLevel {
				if len(levels[i]) != want {
					t.Fatalf("level %d width = %d, want %d", i, len(levels[i]), want)
				}
			}
			summary, err := graph.Summary()
			if err != nil {
				t.Fatal(err)
			}
			if summary.Digest == "" || summary.LevelCount != tt.wantLevels || summary.MaxLevelWidth != tt.wantWidth || summary.ConditionalNodes != tt.wantConditional {
				t.Fatalf("summary = %+v", summary)
			}
		})
	}
}

func TestWorkflowGraphValidate_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		graph   WorkflowGraph
		wantErr string
	}{
		{"empty", WorkflowGraph{}, "no nodes"},
		{"duplicate node", WorkflowGraph{Nodes: []WorkflowGraphNode{{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways}, {ID: "plan", Role: RoleWriter, DependsOn: []string{"plan"}, Condition: WorkflowConditionAlways}}}, "duplicated"},
		{"multiple roots", WorkflowGraph{Nodes: []WorkflowGraphNode{{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways}, {ID: "write", Role: RoleWriter, Condition: WorkflowConditionAlways}}}, "exactly one root"},
		{"conditional root", WorkflowGraph{Nodes: []WorkflowGraphNode{{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionRouteResearch}}}, "root node"},
		{"unknown role", WorkflowGraph{Nodes: []WorkflowGraphNode{{ID: "plan", Role: "unknown", Condition: WorkflowConditionAlways}}}, "unsupported role"},
		{"unknown condition", WorkflowGraph{Nodes: []WorkflowGraphNode{{ID: "plan", Role: RolePlanner, Condition: "unknown"}}}, "unsupported condition"},
		{"unknown dependency", WorkflowGraph{Nodes: []WorkflowGraphNode{{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways}, {ID: "write", Role: RoleWriter, DependsOn: []string{"missing"}, Condition: WorkflowConditionAlways}}}, "unknown node"},
		{"whitespace dependency", WorkflowGraph{Nodes: []WorkflowGraphNode{{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways}, {ID: "write", Role: RoleWriter, DependsOn: []string{" plan"}, Condition: WorkflowConditionAlways}}}, "surrounding whitespace"},
		{"repeated dependency", WorkflowGraph{Nodes: []WorkflowGraphNode{{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways}, {ID: "write", Role: RoleWriter, DependsOn: []string{"plan", "plan"}, Condition: WorkflowConditionAlways}}}, "repeats dependency"},
		{"cycle", WorkflowGraph{Nodes: []WorkflowGraphNode{{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways}, {ID: "left", Role: RoleResearcher, DependsOn: []string{"plan", "right"}, Condition: WorkflowConditionAlways}, {ID: "right", Role: RoleWriter, DependsOn: []string{"left"}, Condition: WorkflowConditionAlways}}}, "cycle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.graph.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestWorkflowGraphSummary_IsOrderIndependent(t *testing.T) {
	first := WorkflowGraph{Workflow: WorkflowResearch, Nodes: []WorkflowGraphNode{
		{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways},
		{ID: "research", Role: RoleResearcher, DependsOn: []string{"plan"}, Condition: WorkflowConditionAlways},
		{ID: "write", Role: RoleWriter, DependsOn: []string{"research", "plan"}, Condition: WorkflowConditionAlways},
	}}
	second := WorkflowGraph{Workflow: WorkflowResearch, Nodes: []WorkflowGraphNode{
		{ID: "write", Role: RoleWriter, DependsOn: []string{"plan", "research"}, Condition: WorkflowConditionAlways},
		{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways},
		{ID: "research", Role: RoleResearcher, DependsOn: []string{"plan"}, Condition: WorkflowConditionAlways},
	}}
	firstSummary, err := first.Summary()
	if err != nil {
		t.Fatal(err)
	}
	secondSummary, err := second.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if firstSummary.Digest != secondSummary.Digest {
		t.Fatalf("digests differ: %q != %q", firstSummary.Digest, secondSummary.Digest)
	}
}

func TestBuildWorkflowGraph_RejectsUnknownWorkflow(t *testing.T) {
	if _, err := BuildWorkflowGraph("custom"); err == nil {
		t.Fatal("expected unsupported workflow error")
	}
}

func TestAnnotateWorkflowGraph_RecordsTopologyAttributes(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	_, span := provider.Tracer("test").Start(context.Background(), "workflow")
	summary, err := annotateWorkflowGraph(span, "multiagent.workflow.effective_graph", WorkflowReviewed)
	if err != nil {
		t.Fatal(err)
	}
	span.End()

	attributes := make(map[string]any)
	for _, value := range recorder.Ended()[0].Attributes() {
		attributes[string(value.Key)] = value.Value.AsInterface()
	}
	if attributes["multiagent.workflow.effective_graph.digest"] != summary.Digest || attributes["multiagent.workflow.effective_graph.node_count"] != int64(4) || attributes["multiagent.workflow.effective_graph.max_level_width"] != int64(1) {
		t.Fatalf("attributes = %+v", attributes)
	}
}
