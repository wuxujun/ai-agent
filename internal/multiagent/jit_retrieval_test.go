package multiagent

import (
	"context"
	"reflect"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

type wikiOnlySearchStub struct{}

func (wikiOnlySearchStub) Name() string               { return "wiki_search" }
func (wikiOnlySearchStub) Description() string        { return "test Wiki search" }
func (wikiOnlySearchStub) Parameters() map[string]any { return map[string]any{} }
func (wikiOnlySearchStub) RiskLevel() types.RiskLevel { return types.RiskLevelLow }
func (wikiOnlySearchStub) Execute(context.Context, string, map[string]interface{}) (*tools.ToolResult, error) {
	return &tools.ToolResult{}, nil
}

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

func TestPlannerResearchActionsForWikiTeamOnlyAllowsSearch(t *testing.T) {
	tools.Register(wikiOnlySearchStub{})
	t.Cleanup(func() { tools.Unregister("wiki_search") })
	actions := plannerResearchActionsFor(AgentConfig{Tools: []string{"wiki_search"}})
	if !reflect.DeepEqual(actions, []string{"wiki_search"}) {
		t.Fatalf("Wiki team planner actions = %v", actions)
	}
	schema := (&PlannerAgent{}).jsonSchema(actions)
	properties := schema["properties"].(map[string]any)
	steps := properties["steps"].(map[string]any)
	if steps["minItems"] != 1 {
		t.Fatalf("Wiki team minimum plan steps = %v, want 1", steps["minItems"])
	}
}

func TestCoordinatorAutomaticallyFetchesJITCandidates(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "jit"
		cfg.RAG.SearchURL = "https://rag.test/mcp"
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

func TestRetrievalFetchStepsCreatesWikiFetch(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.Wiki.FetchMaxItems = 1 }))
	steps := retrievalFetchSteps([]StepEvidence{{
		StepID: "wiki-step", Action: "wiki_search",
		Observation: `{"count":2,"results":[{"id":"wiki-a"},{"id":"wiki-b"}]}`,
	}})
	if len(steps) != 1 || steps[0].Action != "wiki_fetch" {
		t.Fatalf("steps = %+v", steps)
	}
	ids, _ := steps[0].RepairedParameters["ids"].([]string)
	if !reflect.DeepEqual(ids, []string{"wiki-a"}) {
		t.Fatalf("wiki fetch ids = %v", ids)
	}
}

func TestCoordinatorRewritesDriftingFactualPlanToRAG(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "jit"
		cfg.RAG.SearchURL = "https://rag.test/mcp"
		cfg.RAG.JITFetchMaxItems = 2
	}))
	researcher := &jitResearcher{}
	plannerCalls := 0
	coordinator := &Coordinator{Planner: driftingJITPlanner{calls: &plannerCalls}, Researcher: researcher, Writer: jitWriter{}}
	task := &types.Task{ID: "multiagent-jit-drift", Goal: "数学科学术顾问有哪个人？", Status: types.StatusCreated, MaxSteps: 10, ToolBudget: 10, Workspace: "."}
	if err := coordinator.Run(context.Background(), task); err != nil {
		t.Fatalf("coordinator run: %v", err)
	}
	if !reflect.DeepEqual(researcher.actions, []string{"rag_search", "rag_fetch"}) {
		t.Fatalf("executed actions=%v, want deterministic RAG route", researcher.actions)
	}
	if plannerCalls != 0 {
		t.Fatalf("planner calls=%d, want zero before deterministic retrieval", plannerCalls)
	}
}

func TestEnforceWorkspaceResearchPlanRepairsChineseExternalOnlyPlan(t *testing.T) {
	task := &types.Task{Goal: "检查工作区中的配置文件并总结项目模式"}
	plan := &ResearchPlan{ThoughtSummary: "search knowledge base", Steps: []ResearchStep{{
		ID: "step-1", Action: "rag_search", SearchQuery: task.Goal,
	}}}

	if !enforceWorkspaceResearchPlan(task, plan) {
		t.Fatal("explicit Chinese workspace goal was not repaired")
	}
	if len(plan.Steps) != 2 || plan.Steps[0].Action != "find_files" || plan.Steps[1].Action != "search_text" {
		t.Fatalf("repaired plan = %+v", plan.Steps)
	}
	if plan.Steps[1].SearchQuery != task.Goal {
		t.Fatalf("search query = %q, want goal", plan.Steps[1].SearchQuery)
	}
}

func TestEnforceWorkspaceResearchPlanPreservesMixedPlan(t *testing.T) {
	task := &types.Task{Goal: "Inspect the repository and compare it with external docs"}
	plan := &ResearchPlan{Steps: []ResearchStep{
		{ID: "step-1", Action: "find_files", FileGlob: "*.go"},
		{ID: "step-2", Action: "web_search", SearchQuery: "docs"},
	}}

	if enforceWorkspaceResearchPlan(task, plan) {
		t.Fatalf("mixed workspace plan was unexpectedly rewritten: %+v", plan.Steps)
	}
}

func TestEnsureExplicitWorkspaceFileReads(t *testing.T) {
	task := &types.Task{Goal: "检查工作区中的 requirements.md 并读取 runtime.go", MaxSteps: 6}
	plan := &ResearchPlan{Steps: []ResearchStep{{ID: "step-1", Action: "find_files", FileGlob: "*"}}}
	if !ensureExplicitWorkspaceFileReads(task, plan) {
		t.Fatal("named workspace files were not added")
	}
	if len(plan.Steps) != 3 || plan.Steps[1].FilePath != "requirements.md" || plan.Steps[2].FilePath != "runtime.go" {
		t.Fatalf("plan=%+v", plan.Steps)
	}
	if ensureExplicitWorkspaceFileReads(task, plan) {
		t.Fatalf("duplicate reads added: %+v", plan.Steps)
	}
}

func TestEnsureExplicitWorkspaceFileReadsDoesNotRetryAttemptedFailure(t *testing.T) {
	task := &types.Task{
		Goal: "检查工作区中的 missing.md", MaxSteps: 6,
		Trace: []types.StepTrace{{Action: "read_file", Query: `path="missing.md"`, Observation: "read_file error: file not found"}},
	}
	plan := &ResearchPlan{Steps: []ResearchStep{{ID: "step-1", Action: "find_files", FileGlob: "*"}}}
	if ensureExplicitWorkspaceFileReads(task, plan) {
		t.Fatalf("failed file was retried: %+v", plan.Steps)
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

type driftingJITPlanner struct{ calls *int }

func (p driftingJITPlanner) Plan(context.Context, string, string, []types.Memory) (*ResearchPlan, error) {
	if p.calls != nil {
		*p.calls++
	}
	return &ResearchPlan{ThoughtSummary: "search workspace", Steps: []ResearchStep{
		{ID: "step-1", Action: "find_files", FileGlob: "*.*"},
		{ID: "step-2", Action: "search_text", SearchQuery: "学术顾问"},
		{ID: "step-3", Action: "execute_code", Command: "python3", Args: "parse.py"},
	}}, nil
}

func (driftingJITPlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{}, nil
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
