package multiagent

import (
	"context"
	"reflect"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestPlannerResearchActionsExposeSearchButNotDetailTools(t *testing.T) {
	actions := plannerResearchActions()
	contains := func(name string) bool {
		for _, action := range actions {
			if action == name {
				return true
			}
		}
		return false
	}
	for _, action := range []string{"rag_search", "memory_search"} {
		if !contains(action) {
			t.Fatalf("planner action enum missing %q", action)
		}
	}
	for _, action := range []string{"rag_fetch", "memory_get"} {
		if contains(action) {
			t.Fatalf("planner must not select detail action %q before candidate IDs exist", action)
		}
	}
}

func TestCoordinatorAutomaticallyFetchesJITCandidates(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "jit"
		cfg.RAG.JITFetchMaxItems = 2
	}))
	researcher := &jitResearcher{}
	coordinator := &Coordinator{
		Planner:    jitPlanner{},
		Researcher: researcher,
		Writer:     jitWriter{},
	}
	task := &types.Task{
		ID:         "multiagent-jit",
		Goal:       "查询教师信息",
		Status:     types.StatusCreated,
		MaxSteps:   10,
		ToolBudget: 10,
		Workspace:  ".",
	}
	if err := coordinator.Run(context.Background(), task); err != nil {
		t.Fatalf("coordinator run: %v", err)
	}
	if !reflect.DeepEqual(researcher.actions, []string{"rag_search", "rag_fetch"}) {
		t.Fatalf("executed actions=%v, want search then fetch", researcher.actions)
	}
	if !reflect.DeepEqual(researcher.fetchIDs, []string{"rag-a", "rag-b"}) {
		t.Fatalf("fetch ids=%v, want first two candidates", researcher.fetchIDs)
	}
	if task.Status != types.StatusCompleted || task.FinalAnswer != "supported answer" {
		t.Fatalf("task status=%s answer=%q", task.Status, task.FinalAnswer)
	}
}

func TestCoordinatorRejectsUnsupportedFactualWriterAnswer(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.ContextMode = "jit" }))
	coordinator := &Coordinator{Planner: jitPlanner{}, Researcher: emptyJITResearcher{}, Writer: fabricatedJITWriter{}}
	task := &types.Task{ID: "multiagent-jit-empty", Goal: "查询教师信息", Status: types.StatusCreated, MaxSteps: 5, ToolBudget: 5, Workspace: "."}
	if err := coordinator.Run(context.Background(), task); err != nil {
		t.Fatalf("coordinator run: %v", err)
	}
	if task.FinalAnswer == "invented teacher profile" || task.FinalAnswer == "" {
		t.Fatalf("unsupported factual answer was not replaced: %q", task.FinalAnswer)
	}
}

type jitPlanner struct{}

func (jitPlanner) Plan(context.Context, string, string, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{ThoughtSummary: "search RAG", Steps: []ResearchStep{{ID: "step-1", Action: "rag_search", SearchQuery: "teacher"}}}, nil
}

func (jitPlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{}, nil
}

type jitResearcher struct {
	actions  []string
	fetchIDs []string
}

func (r *jitResearcher) Research(_ context.Context, _ string, step ResearchStep) (*StepEvidence, error) {
	r.actions = append(r.actions, step.Action)
	if step.Action == "rag_search" {
		return &StepEvidence{StepID: step.ID, Action: step.Action, Observation: `{"count":3,"results":[{"id":"rag-a"},{"id":"rag-b"},{"id":"rag-c"}]}`}, nil
	}
	if ids, ok := step.RepairedParameters["ids"].([]string); ok {
		r.fetchIDs = append([]string(nil), ids...)
	}
	return &StepEvidence{StepID: step.ID, Action: step.Action, Observation: "fetched 2 rag item(s)", Evidence: []types.Evidence{{Path: "rag-a", Lines: []string{"verified fact"}}}}, nil
}

type jitWriter struct{}

func (jitWriter) Write(context.Context, string, []StepEvidence, []types.Memory) (*WriterOutput, error) {
	return &WriterOutput{FinalAnswer: "supported answer", EvidenceSummary: "RAG evidence", Confidence: "high"}, nil
}

type emptyJITResearcher struct{}

func (emptyJITResearcher) Research(_ context.Context, _ string, step ResearchStep) (*StepEvidence, error) {
	return &StepEvidence{StepID: step.ID, Action: step.Action, Observation: `{"count":0,"results":[]}`}, nil
}

type fabricatedJITWriter struct{}

func (fabricatedJITWriter) Write(context.Context, string, []StepEvidence, []types.Memory) (*WriterOutput, error) {
	return &WriterOutput{FinalAnswer: "invented teacher profile", EvidenceSummary: "none", Confidence: "high"}, nil
}
