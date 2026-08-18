package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/api"
	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type mockPlanner struct {
	blockCh   chan struct{}
	startedCh chan struct{}
	runtimeCh chan *llmcore.Runtime
	tokens    []string
}

func (mp *mockPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*planner.PlanDecision, error) {
	if mp.runtimeCh != nil {
		select {
		case mp.runtimeCh <- llmcore.RuntimeFromContext(ctx):
		default:
		}
	}
	for _, tok := range mp.tokens {
		onChunk(tok)
	}
	if mp.blockCh != nil {
		if mp.startedCh != nil {
			select {
			case mp.startedCh <- struct{}{}:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-mp.blockCh:
		}
	}
	return &planner.PlanDecision{
		Stop:        true,
		FinalAnswer: "Mock final answer",
	}, nil
}

type mockExecutor struct{}

type failingWikiReadiness struct{}

func (failingWikiReadiness) Check(context.Context) error { return errors.New("wiki unavailable") }

type wikiHTTPPlanner struct{}

func (wikiHTTPPlanner) Plan(context.Context, string, string, []types.Memory) (*multiagent.ResearchPlan, error) {
	return &multiagent.ResearchPlan{ThoughtSummary: "search Wiki", Steps: []multiagent.ResearchStep{{
		ID: "wiki-search", Description: "Search the course Wiki", Action: "wiki_search", SearchQuery: "PBL 历史旅行指南",
	}}}, nil
}

func (wikiHTTPPlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*multiagent.ResearchPlan, error) {
	return &multiagent.ResearchPlan{}, nil
}

type wikiHTTPWriter struct{}

func (wikiHTTPWriter) Write(_ context.Context, _ string, evidence []multiagent.StepEvidence, _ []types.Memory) (*multiagent.WriterOutput, error) {
	for _, item := range evidence {
		for _, source := range item.Evidence {
			if source.Path == "wiki://local/concepts/pbl-historical-travel-guide-new-york" && strings.Contains(strings.Join(source.Lines, "\n"), "800–1,000") {
				return &multiagent.WriterOutput{
					FinalAnswer:     "课程要求完成 800–1,000 字旅行指南（wiki://local/concepts/pbl-historical-travel-guide-new-york）。",
					EvidenceSummary: "Wiki page fetched", DraftConfidence: "high",
				}, nil
			}
		}
	}
	return nil, errors.New("writer did not receive fetched Wiki page evidence")
}

type wikiGraphHTTPWriter struct{}

func (wikiGraphHTTPWriter) Write(_ context.Context, _ string, evidence []multiagent.StepEvidence, _ []types.Memory) (*multiagent.WriterOutput, error) {
	seen := make(map[string]bool)
	for _, item := range evidence {
		for _, source := range item.Evidence {
			content := strings.Join(source.Lines, "\n")
			if source.Path == "wiki://local/entities/vanessa-ruales" && strings.Contains(content, "Harvard") {
				seen["teacher"] = true
			}
			if source.Path == "wiki://local/sources/pbl-course" && strings.Contains(content, "six live classes") {
				seen["source"] = true
			}
		}
	}
	if !seen["teacher"] || !seen["source"] {
		return nil, fmt.Errorf("writer did not receive graph neighbor evidence: %v", seen)
	}
	return &multiagent.WriterOutput{
		FinalAnswer:     "导师具有 Harvard 背景（wiki://local/entities/vanessa-ruales），课程包含 six live classes（wiki://local/sources/pbl-course）。",
		EvidenceSummary: "Wiki graph neighbors fetched", DraftConfidence: "high",
	}, nil
}

type wikiSuggestHTTPWriter struct{}

func (wikiSuggestHTTPWriter) Write(_ context.Context, _ string, evidence []multiagent.StepEvidence, _ []types.Memory) (*multiagent.WriterOutput, error) {
	seen := make([]string, 0, 2)
	for _, item := range evidence {
		if item.Action != "wiki_suggest" {
			continue
		}
		for _, source := range item.Evidence {
			if strings.HasPrefix(source.Path, "wiki://local/") {
				seen = append(seen, source.Path)
			}
		}
	}
	if len(seen) != 2 {
		return nil, fmt.Errorf("writer did not receive bounded Wiki suggestions: %v", seen)
	}
	return &multiagent.WriterOutput{
		FinalAnswer:     fmt.Sprintf("建议人工审核 %s 和 %s；未应用任何修改。", seen[0], seen[1]),
		EvidenceSummary: "Read-only Wiki suggestions", DraftConfidence: "high",
	}, nil
}

func (m *mockExecutor) Execute(ctx context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
	return nil, nil
}

type apiResumeVerifier struct{ calls int }

func (v *apiResumeVerifier) Draft(context.Context, string, []multiagent.StepEvidence, []types.Memory) (*multiagent.VerificationDraft, error) {
	return nil, fmt.Errorf("draft must not run during resume")
}

func (v *apiResumeVerifier) Verify(context.Context, string, string, []multiagent.StepEvidence) (*multiagent.VerificationResult, error) {
	v.calls++
	return &multiagent.VerificationResult{Supported: true}, nil
}

func (v *apiResumeVerifier) Finalize(context.Context, string, []multiagent.StepEvidence, []types.Memory) (*multiagent.FinalVerificationOutput, error) {
	return nil, fmt.Errorf("finalize must not run during resume")
}

func setupTestRouter(t *testing.T, st store.Store, eng *orchestrator.Engine) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api.RegisterRoutes(r, st, eng, nil)
	return r
}

func waitForTaskStatus(t *testing.T, st store.Store, taskID string, want types.TaskStatus) *types.Task {
	t.Helper()

	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		task, err := st.GetTask(context.Background(), taskID)
		if err != nil {
			t.Fatalf("failed to get task: %v", err)
		}
		if task.Status == want {
			return task
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for task %s status %s; last status %s", taskID, want, task.Status)
		case <-ticker.C:
		}
	}
}

func TestRunAllResumesPartialVerifierCheckpoint(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = false
		cfg.RAG.ContextMode = "prefetch"
	}))
	st := store.NewMemoryStore()
	verifier := &apiResumeVerifier{}
	engine := &orchestrator.Engine{
		Mode:  orchestrator.ModeMultiAgent,
		Store: st,
		Coordinator: &multiagent.Coordinator{
			FinalVerifier: verifier,
		},
	}
	task := &types.Task{
		ID: "api-verifier-resume", Goal: "verify", Status: types.StatusPartial, FinalAnswer: "candidate", StepCount: 1,
		Trace: []types.StepTrace{{
			Step:        0,
			Action:      multiagent.VerifierDraftCheckpointTraceAction,
			Query:       "verifier_draft_checkpoint",
			Observation: `{"version":1,"draft":{"final_answer":"candidate","evidence_summary":"evidence","draft_confidence":"high"},"evidence":[],"execution_complete":true}`,
			AgentRole:   types.AgentRoleVerifier,
		}},
	}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	r := setupTestRouter(t, st, engine)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/api-verifier-resume/run-all", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("run-all status = %d, want 202: %s", w.Code, w.Body.String())
	}
	persisted := waitForTaskStatus(t, st, task.ID, types.StatusCompleted)
	if verifier.calls != 1 || persisted.FinalAnswer != "candidate" || multiagent.HasPendingVerifierDraft(persisted) {
		t.Fatalf("persisted=%+v verifier_calls=%d", persisted, verifier.calls)
	}
}

func TestCancelTask_NotFound(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks/non-existent/cancel", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestReadyReportsConfiguredProbeMode(t *testing.T) {
	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.LLM.Provider = "openai"
		cfg.LLM.Model = "gpt-test"
		cfg.LLM.ReadinessMode = config.LLMReadinessConfigOnly
	})
	t.Cleanup(restore)
	r := setupTestRouter(t, store.NewMemoryStore(), nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/ready", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var response struct {
		Ready         bool   `json:"ready"`
		Verified      bool   `json:"llm_verified"`
		ReadinessMode string `json:"llm_readiness_mode"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if !response.Ready || response.Verified || response.ReadinessMode != config.LLMReadinessConfigOnly {
		t.Fatalf("readiness response = %+v", response)
	}
}

func TestReadyRejectsInvalidTeamRouting(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.LLM.Provider = "openai"
		cfg.LLM.Model = "gpt-test"
		cfg.LLM.ReadinessMode = config.LLMReadinessConfigOnly
		cfg.MultiAgent.Team = "missing-ready-team"
	}))
	mc := metrics.NewCollector()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api.RegisterRoutes(r, store.NewMemoryStore(), nil, mc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/ready", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d, want 503: %s", w.Code, w.Body.String())
	}
	var response struct {
		Ready bool `json:"ready"`
		Teams struct {
			Configured            bool   `json:"configured"`
			Healthy               bool   `json:"healthy"`
			ActiveTeam            string `json:"active_team"`
			InvalidReferenceCount int    `json:"invalid_reference_count"`
			Error                 string `json:"error"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Ready || !response.Teams.Configured || response.Teams.Healthy || response.Teams.ActiveTeam != "missing-ready-team" || response.Teams.InvalidReferenceCount != 1 || response.Teams.Error == "" {
		t.Fatalf("team readiness response = %+v", response)
	}
	if got := mc.Snapshot().MultiAgentTeamReadinessFailures; got != 1 {
		t.Fatalf("team readiness failure metric = %d", got)
	}
}

func TestConfigReloadRejectsCrossConfigValidation(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = "reload-validation-key"
	}))
	unregister := config.RegisterReloadValidator("api-test-reject", func(*config.Config) error {
		return fmt.Errorf("test cross-config rejection")
	})
	t.Cleanup(unregister)
	beforeRevision := config.Revision()
	mc := metrics.NewCollector()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api.RegisterRoutes(r, store.NewMemoryStore(), nil, mc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/config/reload", nil)
	req.Header.Set("X-API-Key", "reload-validation-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "test cross-config rejection") {
		t.Fatalf("reload response = %d: %s", w.Code, w.Body.String())
	}
	if got := config.Revision(); got != beforeRevision {
		t.Fatalf("rejected reload changed revision: before=%d after=%d", beforeRevision, got)
	}
	if got := mc.Snapshot().MultiAgentTeamReloadRejections; got != 1 {
		t.Fatalf("reload rejection metric = %d", got)
	}
}

func TestReadyFailsWhenRequiredWikiProbeFails(t *testing.T) {
	metricsBefore := tools.CurrentWikiMetrics()
	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.LLM.Provider = "openai"
		cfg.LLM.Model = "gpt-test"
		cfg.LLM.ReadinessMode = config.LLMReadinessConfigOnly
		cfg.Wiki.Directory = t.TempDir()
		cfg.Wiki.Required = true
	})
	t.Cleanup(restore)
	r := gin.New()
	handler := api.RegisterRoutes(r, store.NewMemoryStore(), nil, nil)
	handler.SetWikiReadinessChecker(failingWikiReadiness{})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/ready", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d, want 503: %s", w.Code, w.Body.String())
	}
	var response struct {
		Ready bool `json:"ready"`
		Wiki  struct {
			Configured bool   `json:"configured"`
			Required   bool   `json:"required"`
			Healthy    bool   `json:"healthy"`
			Error      string `json:"error"`
		} `json:"wiki"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Ready || !response.Wiki.Configured || !response.Wiki.Required || response.Wiki.Healthy || response.Wiki.Error != "wiki unavailable" {
		t.Fatalf("readiness response = %+v", response)
	}
	if got := tools.CurrentWikiMetrics().ReadinessFailures - metricsBefore.ReadinessFailures; got != 1 {
		t.Fatalf("Wiki readiness failure delta = %d, want 1", got)
	}
}

func TestMetricsIncludesWikiSnapshot(t *testing.T) {
	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = "metrics-admin-key"
	})
	t.Cleanup(restore)
	r := gin.New()
	api.RegisterRoutes(r, store.NewMemoryStore(), nil, metrics.NewCollector())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Header.Set("X-API-Key", "metrics-admin-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		PlannerCalls int64 `json:"planner_calls"`
		Wiki         struct {
			BackendCalls            int64   `json:"backend_calls"`
			BackendAverageLatencyMS float64 `json:"backend_average_latency_ms"`
			BackendP95LatencyMS     float64 `json:"backend_p95_latency_ms"`
		} `json:"wiki"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Wiki.BackendCalls < 0 || response.Wiki.BackendAverageLatencyMS < 0 || response.Wiki.BackendP95LatencyMS < 0 {
		t.Fatalf("metrics response = %+v", response)
	}
}

func TestHTTPMultiAgentSearchesAndFetchesLocalWiki(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.AnswerPipeline.Enabled = false
		cfg.RAG.ContextMode = "jit"
		cfg.MultiAgent.Team = "wiki"
		cfg.Wiki.Directory = root
		cfg.Wiki.DefaultSpace = "local"
		cfg.Wiki.SearchTopK = 3
		cfg.Wiki.FetchMaxItems = 2
		cfg.Wiki.FetchMaxBytes = 12000
	}))
	pageDir := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(pageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	page := "# PBL 历史旅行指南：纽约篇\n\n学生需完成一篇 800–1,000 字的历史旅行指南。"
	if err := os.WriteFile(filepath.Join(pageDir, "pbl-historical-travel-guide-new-york.md"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := wiki.NewDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := tools.RegisterWikiTools(tools.DefaultRegistry, client); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tools.Unregister("wiki_search")
		tools.Unregister("wiki_fetch")
		tools.Unregister("wiki_graph")
		tools.Unregister("wiki_graph_fetch")
		tools.Unregister("wiki_suggest")
	})

	st := store.NewMemoryStore()
	coordinator := &multiagent.Coordinator{
		Planner: &wikiHTTPPlanner{}, Researcher: &multiagent.ResearcherAgent{}, Writer: &wikiHTTPWriter{},
	}
	engine := &orchestrator.Engine{Mode: orchestrator.ModeMultiAgent, Store: st, Coordinator: coordinator}
	r := setupTestRouter(t, st, engine)

	create := httptest.NewRecorder()
	request, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(
		`{"id":"wiki-http-e2e","goal":"PBL 历史旅行指南有哪些要求？","workspace":"workspace","mode":"multiagent","max_steps":4,"tool_budget":4}`,
	))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", create.Code, create.Body.String())
	}
	run := httptest.NewRecorder()
	request, _ = http.NewRequest(http.MethodPost, "/api/tasks/wiki-http-e2e/run-all", nil)
	r.ServeHTTP(run, request)
	if run.Code != http.StatusAccepted {
		t.Fatalf("run status = %d: %s", run.Code, run.Body.String())
	}
	completed := waitForTaskStatus(t, st, "wiki-http-e2e", types.StatusCompleted)
	if !strings.Contains(completed.FinalAnswer, "800–1,000") || !strings.Contains(completed.FinalAnswer, "wiki://local/concepts/pbl-historical-travel-guide-new-york") {
		t.Fatalf("final answer = %q", completed.FinalAnswer)
	}
	actions := make(map[string]bool)
	for _, trace := range completed.Trace {
		actions[trace.Action] = true
	}
	if !actions["wiki_search"] || !actions["wiki_fetch"] {
		t.Fatalf("trace actions = %v", actions)
	}
}

func TestHTTPMultiAgentWikiGraphFetchesBoundedNeighborPages(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.AnswerPipeline.Enabled = false
		cfg.RAG.ContextMode = "jit"
		cfg.MultiAgent.Team = "software"
		cfg.Wiki.Directory = root
		cfg.Wiki.DefaultSpace = "local"
		cfg.Wiki.SearchTopK = 3
		cfg.Wiki.FetchMaxItems = 1
		cfg.Wiki.FetchMaxBytes = 12000
	}))
	pages := map[string]string{
		"concepts/pbl-historical-travel-guide-new-york.md": "# PBL 历史旅行指南：纽约篇\n\n[Vanessa](../entities/vanessa-ruales.md) and [course source](../sources/pbl-course.md).",
		"entities/vanessa-ruales.md":                       "# Vanessa Ruales\n\nHarvard sociology background.",
		"sources/pbl-course.md":                            "# PBL Course Source\n\nThe course includes six live classes.",
	}
	for path, content := range pages {
		fullPath := filepath.Join(root, "wiki", filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, err := wiki.NewDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := tools.RegisterWikiTools(tools.DefaultRegistry, client); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tools.Unregister("wiki_search")
		tools.Unregister("wiki_fetch")
		tools.Unregister("wiki_graph")
		tools.Unregister("wiki_graph_fetch")
		tools.Unregister("wiki_suggest")
	})

	st := store.NewMemoryStore()
	coordinator := &multiagent.Coordinator{
		Planner: &wikiHTTPPlanner{}, Researcher: &multiagent.ResearcherAgent{}, Writer: &wikiGraphHTTPWriter{},
	}
	engine := &orchestrator.Engine{Mode: orchestrator.ModeMultiAgent, Store: st, Coordinator: coordinator}
	r := setupTestRouter(t, st, engine)

	create := httptest.NewRecorder()
	request, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(
		`{"id":"wiki-graph-http-e2e","goal":"PBL 历史旅行指南的导师和课次是什么？","workspace":"workspace","mode":"multiagent","team":"wiki_graph","max_steps":6,"tool_budget":4}`,
	))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", create.Code, create.Body.String())
	}
	run := httptest.NewRecorder()
	request, _ = http.NewRequest(http.MethodPost, "/api/tasks/wiki-graph-http-e2e/run-all", nil)
	r.ServeHTTP(run, request)
	if run.Code != http.StatusAccepted {
		t.Fatalf("run status = %d: %s", run.Code, run.Body.String())
	}
	completed := waitForTaskStatus(t, st, "wiki-graph-http-e2e", types.StatusCompleted)
	if !strings.Contains(completed.FinalAnswer, "wiki://local/entities/vanessa-ruales") || !strings.Contains(completed.FinalAnswer, "wiki://local/sources/pbl-course") {
		t.Fatalf("final answer = %q", completed.FinalAnswer)
	}
	actions := make(map[string]bool)
	for _, trace := range completed.Trace {
		actions[trace.Action] = true
	}
	for _, action := range []string{"wiki_search", "wiki_fetch", "wiki_graph", "wiki_graph_fetch"} {
		if !actions[action] {
			t.Fatalf("trace actions = %v; missing %s", actions, action)
		}
	}
}

func TestHTTPMultiAgentWikiSuggestProducesReadOnlyCandidates(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.AnswerPipeline.Enabled = false
		cfg.RAG.ContextMode = "jit"
		cfg.MultiAgent.Team = "software"
		cfg.Wiki.Directory = root
		cfg.Wiki.DefaultSpace = "local"
		cfg.Wiki.SearchTopK = 3
		cfg.Wiki.FetchMaxItems = 1
	}))
	pages := map[string]string{
		"concepts/pbl-course.md": "# PBL Course\n\n[Teacher](../entities/teacher.md) and [Source](../sources/pbl-course.md).",
		"entities/teacher.md":    "# Teacher\n\nCourse mentor.",
		"sources/pbl-course.md":  "# PBL Course Source\n\nCourse details.",
	}
	for path, content := range pages {
		fullPath := filepath.Join(root, "wiki", filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, _ := wiki.NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := tools.RegisterWikiTools(tools.DefaultRegistry, client); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, name := range []string{"wiki_search", "wiki_fetch", "wiki_graph", "wiki_graph_fetch", "wiki_suggest"} {
			tools.Unregister(name)
		}
	})
	st := store.NewMemoryStore()
	engine := &orchestrator.Engine{Mode: orchestrator.ModeMultiAgent, Store: st, Coordinator: &multiagent.Coordinator{
		Planner: &wikiHTTPPlanner{}, Researcher: &multiagent.ResearcherAgent{}, Writer: &wikiSuggestHTTPWriter{},
	}}
	r := setupTestRouter(t, st, engine)
	create := httptest.NewRecorder()
	request, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(
		`{"id":"wiki-suggest-http-e2e","goal":"为 PBL Course 提供只读 Wiki 策展建议","workspace":"workspace","mode":"multiagent","team":"wiki_suggest","max_steps":6,"tool_budget":3}`,
	))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d: %s", create.Code, create.Body.String())
	}
	run := httptest.NewRecorder()
	request, _ = http.NewRequest(http.MethodPost, "/api/tasks/wiki-suggest-http-e2e/run-all", nil)
	r.ServeHTTP(run, request)
	if run.Code != http.StatusAccepted {
		t.Fatalf("run status=%d: %s", run.Code, run.Body.String())
	}
	completed := waitForTaskStatus(t, st, "wiki-suggest-http-e2e", types.StatusCompleted)
	if !strings.Contains(completed.FinalAnswer, "未应用任何修改") {
		t.Fatalf("final answer=%q", completed.FinalAnswer)
	}
	actions := make(map[string]bool)
	for _, trace := range completed.Trace {
		actions[trace.Action] = true
	}
	for _, action := range []string{"wiki_search", "wiki_fetch", "wiki_suggest"} {
		if !actions[action] {
			t.Fatalf("trace actions=%v; missing %s", actions, action)
		}
	}
}

func TestCancelTask_NotRunning(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	task := &types.Task{
		ID:     "task-1",
		Status: types.StatusCompleted,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-1/cancel", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestCancelTask_Orphaned(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	task := &types.Task{
		ID:     "task-1",
		Status: types.StatusRunning,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-1/cancel", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Verify that the task status was updated to failed in the store
	updatedTask, err := st.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if updatedTask.Status != types.StatusFailed {
		t.Errorf("expected status failed, got %s", updatedTask.Status)
	}
}

func TestCancelTask_Active(t *testing.T) {
	st := store.NewMemoryStore()
	blockCh := make(chan struct{})
	startedCh := make(chan struct{}, 1)
	mp := &mockPlanner{blockCh: blockCh, startedCh: startedCh}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	r := setupTestRouter(t, st, engine)

	task := &types.Task{
		ID:         "task-active",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	// Start running it asynchronously
	wRun := httptest.NewRecorder()
	reqRun, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-active/run-all", nil)
	r.ServeHTTP(wRun, reqRun)

	if wRun.Code != http.StatusAccepted {
		t.Fatalf("expected run-all status 202, got %d", wRun.Code)
	}

	select {
	case <-startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for planner to start")
	}
	waitForTaskStatus(t, st, "task-active", types.StatusRunning)

	// Cancel it
	wCancel := httptest.NewRecorder()
	reqCancel, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-active/cancel", nil)
	r.ServeHTTP(wCancel, reqCancel)

	if wCancel.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", wCancel.Code)
	}

	// Verify that the task status is failed/canceled in the store
	updatedTask := waitForTaskStatus(t, st, "task-active", types.StatusFailed)
	if updatedTask.Status != types.StatusFailed {
		t.Errorf("expected status failed, got %s", updatedTask.Status)
	}
}

func TestDeleteTask(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	task := &types.Task{ID: "task-delete", Status: types.StatusCompleted, FinalAnswer: "done"}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-delete", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if _, err := st.GetTask(context.Background(), task.ID); err != sql.ErrNoRows {
		t.Fatalf("GetTask after delete error = %v, want sql.ErrNoRows", err)
	}
}

func TestDeleteTaskRejectsRunningTask(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	if err := st.SaveFullTask(context.Background(), &types.Task{ID: "task-delete-running", Status: types.StatusRunning}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-delete-running", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete running status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestDeleteAllTasksRequiresConfirmation(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	for _, id := range []string{"task-clear-a", "task-clear-b"} {
		if err := st.SaveFullTask(context.Background(), &types.Task{ID: id, Status: types.StatusCompleted}); err != nil {
			t.Fatal(err)
		}
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/tasks", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed clear status = %d, want 400: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/tasks?confirm=true", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deleted":2`) {
		t.Fatalf("clear status = %d, want 200/deleted=2: %s", w.Code, w.Body.String())
	}
}

func TestMemoryManagement(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	for _, mem := range []*types.Memory{
		{ID: "memory-manage-a", TenantID: "tenant-a", Goal: "a", Embedding: []float32{1, 2, 3}, Timestamp: time.Now()},
		{ID: "memory-manage-b", TenantID: "tenant-b", Goal: "b", Timestamp: time.Now().Add(-time.Second)},
	} {
		if err := st.SaveMemory(context.Background(), mem); err != nil {
			t.Fatal(err)
		}
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/memories?tenant_id=tenant-a&limit=10", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"memory-manage-a"`) || strings.Contains(w.Body.String(), `"embedding":`) || !strings.Contains(w.Body.String(), `"embedding_dimensions":3`) {
		t.Fatalf("memory list = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/memories/memory-manage-a", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("memory delete = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/memories", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed memory clear = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/memories?confirm=true", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deleted":1`) {
		t.Fatalf("memory clear = %d %s", w.Code, w.Body.String())
	}
}

// TestCreateTaskPersistsTokenBudget verifies that token_budget in the POST body
// flows through CreateTaskRequest into the persisted Task. This closes the
// silent break where the field was on the type but unreachable via the API,
// making the token budget gate a dead branch.
func TestCreateTaskPersistsTokenBudget(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	body := `{"id":"task-tb","goal":"x","workspace":"./testdata","max_steps":3,"tool_budget":3,"token_budget":2500}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var created types.Task
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.TokenBudget != 2500 {
		t.Errorf("response TokenBudget = %d, want 2500", created.TokenBudget)
	}

	got, err := st.GetTask(context.Background(), "task-tb")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.TokenBudget != 2500 {
		t.Errorf("persisted TokenBudget = %d, want 2500", got.TokenBudget)
	}
}

func TestCreateTaskPersistsLLMBudgets(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = nil
		cfg.LLM.Gateway.InputCostPerMillionUSD = apiTestPtr(1.0)
	}))
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	body := `{"id":"task-llm-budget","goal":"x","workspace":"./testdata","llm_call_budget":4,"llm_cost_budget_usd":1.5}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}
	got, err := st.GetTask(context.Background(), "task-llm-budget")
	if err != nil {
		t.Fatal(err)
	}
	if got.LLMCallBudget != 4 || got.LLMCostBudgetUSD != 1.5 {
		t.Fatalf("persisted LLM budgets = %+v", got)
	}
}

func TestCreateTaskPersistsMode(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"id":"task-mode","goal":"x","workspace":"./testdata","mode":"multiagent"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	got, err := st.GetTask(context.Background(), "task-mode")
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "multiagent" {
		t.Fatalf("persisted mode = %q, want multiagent", got.Mode)
	}
}

func TestCreateTaskPersistsValidatedTeamSelection(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"id":"task-team","goal":"x","workspace":"./testdata","mode":"multiagent","team":"wiki_graph"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	got, err := st.GetTask(t.Context(), "task-team")
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestedTeam != "wiki_graph" || got.TeamSelectionSource != "explicit" || got.Team != "wiki_graph" || got.TeamConfigDigest == "" {
		t.Fatalf("persisted team selection=%+v", got)
	}
}

func TestCreateTaskPersistsGlobalDefaultSelectionSource(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.MultiAgent.Team = "software"
	}))
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"id":"task-global-default-source","goal":"x","workspace":"./testdata","mode":"multiagent"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	got, err := st.GetTask(t.Context(), "task-global-default-source")
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestedTeam != "" || got.TeamSelectionSource != "global_default" || got.Team != "software" || got.TeamConfigDigest == "" {
		t.Fatalf("persisted global default selection = %+v", got)
	}
}

func TestCreateTaskRejectsUnknownOrNonMultiAgentTeam(t *testing.T) {
	for name, body := range map[string]string{
		"unknown": `{"goal":"x","workspace":"./testdata","mode":"multiagent","team":"missing-team"}`,
		"legacy":  `{"goal":"x","workspace":"./testdata","mode":"legacy","team":"wiki"}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := store.NewMemoryStore()
			r := setupTestRouter(t, st, nil)
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateTaskEnforcesTenantTeamAllowlist(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "tenant-a-team-key", AllowedMultiAgentTeams: []string{"wiki"}},
		}
	}))
	st := store.NewMemoryStore()
	mc := metrics.NewCollector()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api.RegisterRoutes(r, st, nil, mc)

	create := func(id, team string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"id":%q,"goal":"x","workspace":"./testdata","mode":"multiagent","team":%q}`, id, team)
		req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", "tenant-a-team-key")
		r.ServeHTTP(w, req)
		return w
	}

	if w := create("tenant-wiki", "wiki"); w.Code != http.StatusCreated {
		t.Fatalf("allowed team status = %d: %s", w.Code, w.Body.String())
	}
	if w := create("tenant-graph", "wiki_graph"); w.Code != http.StatusForbidden {
		t.Fatalf("disallowed team status = %d: %s", w.Code, w.Body.String())
	}
	if _, err := st.GetTask(t.Context(), "tenant-graph"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("disallowed task persisted: %v", err)
	}
	snapshot := mc.Snapshot()
	if snapshot.MultiAgentTeamTasksCreated != 1 || snapshot.MultiAgentTeamTasksCreatedByTeam["wiki"] != 1 || snapshot.MultiAgentTeamSelectionRejections != 1 || snapshot.MultiAgentTeamDefaultUnavailable != 0 {
		t.Fatalf("team selection metrics = %+v", snapshot)
	}
	if snapshot.MultiAgentTeamTasksCreatedBySource["explicit"] != 1 || snapshot.MultiAgentTeamRejectionsBySource["explicit"] != 1 {
		t.Fatalf("explicit source metrics = %+v", snapshot)
	}
}

func TestCreateTaskTeamAllowlistAppliesToDefaultAndExemptsAdmin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		admin      bool
		body       string
		wantStatus int
	}{
		{
			name:       "resolved default is restricted",
			body:       `{"id":"tenant-default","goal":"x","workspace":"./testdata","mode":"multiagent"}`,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "tenant admin bypasses allowlist",
			admin:      true,
			body:       `{"id":"tenant-admin","goal":"x","workspace":"./testdata","mode":"multiagent","team":"wiki_graph"}`,
			wantStatus: http.StatusCreated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
				cfg.API.Auth.Mode = "api_key"
				cfg.MultiAgent.Team = "software"
				cfg.API.Tenants = map[string]config.APITenantConfig{
					"tenant-a": {
						APIKey:                 "tenant-a-team-key",
						Admin:                  tc.admin,
						AllowedMultiAgentTeams: []string{"wiki"},
					},
				}
			}))
			r := setupTestRouter(t, store.NewMemoryStore(), nil)
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", "tenant-a-team-key")
			r.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

func TestCreateTaskCountsUnavailableDefaultTeam(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.MultiAgent.Team = "software"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "tenant-default-team-key", AllowedMultiAgentTeams: []string{"wiki"}},
		}
	}))
	mc := metrics.NewCollector()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api.RegisterRoutes(r, store.NewMemoryStore(), nil, mc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"goal":"x","workspace":"./testdata","mode":"multiagent"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "tenant-default-team-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	snapshot := mc.Snapshot()
	if snapshot.MultiAgentTeamSelectionRejections != 1 || snapshot.MultiAgentTeamDefaultUnavailable != 1 {
		t.Fatalf("default team metrics = %+v", snapshot)
	}
	if snapshot.MultiAgentTeamRejectionsBySource["global_default"] != 1 {
		t.Fatalf("global default rejection metrics = %+v", snapshot)
	}
}

func TestTenantDefaultTeamOverridesGlobalDefault(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.MultiAgent.Team = "software"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {
				APIKey:                 "tenant-own-default-key",
				DefaultMultiAgentTeam:  "wiki_graph",
				AllowedMultiAgentTeams: []string{"wiki", "wiki_graph"},
			},
		}
	}))
	st := store.NewMemoryStore()
	mc := metrics.NewCollector()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api.RegisterRoutes(r, st, nil, mc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"id":"tenant-own-default","goal":"x","workspace":"./testdata","mode":"multiagent"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "tenant-own-default-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	created, err := st.GetTask(t.Context(), "tenant-own-default")
	if err != nil {
		t.Fatal(err)
	}
	if created.RequestedTeam != "" || created.TeamSelectionSource != "tenant_default" || created.Team != "wiki_graph" || created.TeamConfigDigest == "" {
		t.Fatalf("tenant default task = %+v", created)
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set("X-API-Key", "tenant-own-default-key")
	r.ServeHTTP(w, req)
	var response struct {
		DefaultTeam   string                   `json:"default_team"`
		DefaultSource string                   `json:"default_source"`
		Teams         []multiagent.TeamSummary `json:"teams"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.DefaultTeam != "wiki_graph" || response.DefaultSource != "tenant_default" {
		t.Fatalf("tenant Team list = %d: %s", w.Code, w.Body.String())
	}
	for _, team := range response.Teams {
		if team.Default != (team.Name == "wiki_graph") {
			t.Fatalf("tenant default marker = %+v", response.Teams)
		}
	}
	if got := mc.Snapshot().MultiAgentTeamTasksCreatedBySource["tenant_default"]; got != 1 {
		t.Fatalf("tenant default source metric = %d", got)
	}
}

func TestListTeamsFiltersForTenantAndHidesUnavailableDefault(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.MultiAgent.Team = "software"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "tenant-a-team-key", AllowedMultiAgentTeams: []string{"wiki", "wiki_graph"}},
		}
	}))
	r := setupTestRouter(t, store.NewMemoryStore(), nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set("X-API-Key", "tenant-a-team-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		DefaultTeam   string                   `json:"default_team"`
		DefaultSource string                   `json:"default_source"`
		Teams         []multiagent.TeamSummary `json:"teams"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.DefaultTeam != "" || response.DefaultSource != "" {
		t.Fatalf("unavailable default team exposed as %q", response.DefaultTeam)
	}
	if len(response.Teams) != 2 || response.Teams[0].Name != "wiki" || response.Teams[1].Name != "wiki_graph" {
		t.Fatalf("filtered teams = %+v", response.Teams)
	}
	for _, team := range response.Teams {
		if team.Default || team.ConfigDigest == "" {
			t.Fatalf("invalid filtered team metadata: %+v", team)
		}
	}
	if strings.Contains(w.Body.String(), "system_prompt") || strings.Contains(w.Body.String(), "tools") {
		t.Fatalf("sensitive team configuration leaked: %s", w.Body.String())
	}
}

func TestListTeamsAdminSeesAllConfiguredTeams(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.MultiAgent.Team = "software"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-admin": {APIKey: "tenant-admin-team-key", Admin: true, AllowedMultiAgentTeams: []string{"wiki"}},
		}
	}))
	r := setupTestRouter(t, store.NewMemoryStore(), nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set("X-API-Key", "tenant-admin-team-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		DefaultTeam    string                   `json:"default_team"`
		DefaultSource  string                   `json:"default_source"`
		ConfigRevision string                   `json:"config_revision"`
		Teams          []multiagent.TeamSummary `json:"teams"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.DefaultTeam != "software" || response.DefaultSource != "global_default" || response.ConfigRevision == "" || len(response.Teams) <= 1 {
		t.Fatalf("admin team response = %+v", response)
	}
}

func TestUpdateTeamLifecycleRequiresAdminAndSupportsNoOpCAS(t *testing.T) {
	t.Setenv("AI_AGENT_TEAM_LIFECYCLE_AUDIT_FILE", filepath.Join(t.TempDir(), "lifecycle-audit.jsonl"))
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.MultiAgent.Team = "wiki_suggest"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-user":  {APIKey: "team-user-key", Admin: false},
			"tenant-admin": {APIKey: "team-admin-key", Admin: true},
		}
	}))
	r := gin.New()
	mc := metrics.NewCollector()
	api.RegisterRoutes(r, store.NewMemoryStore(), nil, mc)

	list := httptest.NewRecorder()
	listReq, _ := http.NewRequest(http.MethodGet, "/api/teams", nil)
	listReq.Header.Set("X-API-Key", "team-admin-key")
	r.ServeHTTP(list, listReq)
	var listed struct {
		ConfigRevision string `json:"config_revision"`
	}
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &listed) != nil || listed.ConfigRevision == "" {
		t.Fatalf("list response = %d %s", list.Code, list.Body.String())
	}
	body := fmt.Sprintf(`{"lifecycle":"active","expected_revision":%q}`, listed.ConfigRevision)

	forbidden := httptest.NewRecorder()
	forbiddenReq, _ := http.NewRequest(http.MethodPatch, "/api/teams/wiki_suggest/lifecycle", strings.NewReader(body))
	forbiddenReq.Header.Set("Content-Type", "application/json")
	forbiddenReq.Header.Set("X-API-Key", "team-user-key")
	r.ServeHTTP(forbidden, forbiddenReq)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d: %s", forbidden.Code, forbidden.Body.String())
	}

	updated := httptest.NewRecorder()
	updatedReq, _ := http.NewRequest(http.MethodPatch, "/api/teams/wiki_suggest/lifecycle", strings.NewReader(body))
	updatedReq.Header.Set("Content-Type", "application/json")
	updatedReq.Header.Set("X-API-Key", "team-admin-key")
	r.ServeHTTP(updated, updatedReq)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"changed":false`) {
		t.Fatalf("admin no-op status = %d: %s", updated.Code, updated.Body.String())
	}
	if !strings.Contains(updated.Body.String(), `"audit"`) {
		t.Fatalf("lifecycle audit missing: %s", updated.Body.String())
	}

	stale := httptest.NewRecorder()
	staleReq, _ := http.NewRequest(http.MethodPatch, "/api/teams/wiki_suggest/lifecycle", strings.NewReader(`{"lifecycle":"active","expected_revision":"stale"}`))
	staleReq.Header.Set("Content-Type", "application/json")
	staleReq.Header.Set("X-API-Key", "team-admin-key")
	r.ServeHTTP(stale, staleReq)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale revision status = %d: %s", stale.Code, stale.Body.String())
	}

	protected := httptest.NewRecorder()
	protectedReq, _ := http.NewRequest(http.MethodPatch, "/api/teams/wiki_suggest/lifecycle", strings.NewReader(fmt.Sprintf(`{"lifecycle":"draining","expected_revision":%q}`, listed.ConfigRevision)))
	protectedReq.Header.Set("Content-Type", "application/json")
	protectedReq.Header.Set("X-API-Key", "team-admin-key")
	r.ServeHTTP(protected, protectedReq)
	if protected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("default protection status = %d: %s", protected.Code, protected.Body.String())
	}
	snapshot := mc.Snapshot()
	if snapshot.MultiAgentTeamLifecycleChanges != 0 || snapshot.MultiAgentTeamLifecycleConflicts != 1 || snapshot.MultiAgentTeamDefaultProtections != 1 {
		t.Fatalf("lifecycle API metrics = %+v", snapshot)
	}
	audits := httptest.NewRecorder()
	auditsReq, _ := http.NewRequest(http.MethodGet, "/api/teams/lifecycle-audits?limit=1", nil)
	auditsReq.Header.Set("X-API-Key", "team-admin-key")
	r.ServeHTTP(audits, auditsReq)
	if audits.Code != http.StatusOK || !strings.Contains(audits.Body.String(), `"actor_tenant":"tenant-admin"`) {
		t.Fatalf("lifecycle audits = %d: %s", audits.Code, audits.Body.String())
	}
	filteredAudits := httptest.NewRecorder()
	filteredAuditsReq, _ := http.NewRequest(http.MethodGet, "/api/teams/lifecycle-audits?team=wiki_suggest&changed=false", nil)
	filteredAuditsReq.Header.Set("X-API-Key", "team-admin-key")
	r.ServeHTTP(filteredAudits, filteredAuditsReq)
	if filteredAudits.Code != http.StatusOK || !strings.Contains(filteredAudits.Body.String(), `"changed":false`) {
		t.Fatalf("filtered lifecycle audits = %d: %s", filteredAudits.Code, filteredAudits.Body.String())
	}
	invalidFilter := httptest.NewRecorder()
	invalidFilterReq, _ := http.NewRequest(http.MethodGet, "/api/teams/lifecycle-audits?changed=sometimes", nil)
	invalidFilterReq.Header.Set("X-API-Key", "team-admin-key")
	r.ServeHTTP(invalidFilter, invalidFilterReq)
	if invalidFilter.Code != http.StatusBadRequest {
		t.Fatalf("invalid audit filter status = %d: %s", invalidFilter.Code, invalidFilter.Body.String())
	}
	forbiddenAudits := httptest.NewRecorder()
	forbiddenAuditsReq, _ := http.NewRequest(http.MethodGet, "/api/teams/lifecycle-audits", nil)
	forbiddenAuditsReq.Header.Set("X-API-Key", "team-user-key")
	r.ServeHTTP(forbiddenAudits, forbiddenAuditsReq)
	if forbiddenAudits.Code != http.StatusForbidden {
		t.Fatalf("non-admin lifecycle audits status = %d: %s", forbiddenAudits.Code, forbiddenAudits.Body.String())
	}
	integrity := httptest.NewRecorder()
	integrityReq, _ := http.NewRequest(http.MethodGet, "/api/teams/lifecycle-audits/integrity", nil)
	integrityReq.Header.Set("X-API-Key", "team-admin-key")
	r.ServeHTTP(integrity, integrityReq)
	if integrity.Code != http.StatusOK || !strings.Contains(integrity.Body.String(), `"protected_records":1`) {
		t.Fatalf("lifecycle audit integrity = %d: %s", integrity.Code, integrity.Body.String())
	}
}

func TestUpdateTeamLifecycleReturnsInsufficientStorageWhenAuditIsFull(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "full-audit.jsonl")
	legacy := `{"id":"legacy","timestamp":"2026-08-18T00:00:00Z","actor_tenant":"admin","team":"data","changed":false}` + "\n"
	if err := os.WriteFile(auditPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_AGENT_TEAM_LIFECYCLE_AUDIT_FILE", auditPath)
	t.Setenv("AI_AGENT_TEAM_LIFECYCLE_AUDIT_MAX_BYTES", "128")
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.MultiAgent.Team = "wiki_suggest"
		cfg.API.APIKey = "audit-capacity-admin-key"
	}))
	r := setupTestRouter(t, store.NewMemoryStore(), nil)
	revision, err := multiagent.TeamsConfigRevision()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"lifecycle":"active","expected_revision":%q}`, revision)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPatch, "/api/teams/data/lifecycle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "audit-capacity-admin-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), `"audit_persisted":false`) {
		t.Fatalf("capacity status = %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateTaskRejectsUnsupportedMode(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"goal":"x","workspace":"./testdata","mode":"invalid"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateTaskRejectsNegativeLLMBudget(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"goal":"x","workspace":"./testdata","llm_call_budget":-1}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func apiTestPtr[T any](value T) *T { return &value }

func TestCreateTask_AutoGenerateID(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	body := `{"goal":"x","workspace":"./testdata","max_steps":3,"tool_budget":3}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var created types.Task
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.ID == "" {
		t.Errorf("expected generated ID in response, got empty")
	}

	got, err := st.GetTask(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("persisted ID = %q, want %q", got.ID, created.ID)
	}
}

// TestRunAllAlreadyRunningInDBDoesNotLeakReservation covers the cleanup path
// that #7 hardened: when the DB TryTransitionTaskStatus rejects (because the
// task is already Running in the persisted store — possibly from a peer
// process), the handler now reserves the in-process activeTasks slot BEFORE
// the DB call and must release it on rejection. If the cleanup leaked, a
// subsequent runAll on the same task ID would return 409 forever, even after
// the DB row is corrected.
func TestRunAllAlreadyRunningInDBDoesNotLeakReservation(t *testing.T) {
	st := store.NewMemoryStore()
	mp := &mockPlanner{}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	r := setupTestRouter(t, st, engine)

	// Simulate an unsupported persisted state. The status is not in the allowed
	// transition set, so the
	// TryTransitionTaskStatus inside runAll will return false.
	task := &types.Task{
		ID:         "task-stale-running",
		Status:     types.TaskStatus("invalid"),
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-stale-running/run-all", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected first run-all to 409 on DB rejection, got %d: %s", w.Code, w.Body.String())
	}

	// Now flip the DB row back to a startable state. If the previous run-all
	// leaked an activeTasks entry, this second call would still see the
	// reservation and 409. The fix uses compare-and-delete cleanup so the slot
	// is released.
	task.Status = types.StatusCreated
	_ = st.SaveFullTask(context.Background(), task)

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-stale-running/run-all", nil)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusAccepted {
		t.Fatalf("expected second run-all to 202 after DB row corrected, got %d: %s", w2.Code, w2.Body.String())
	}

	// Drain the running task so the test doesn't leak a goroutine that
	// outlives the test binary.
	wCancel := httptest.NewRecorder()
	reqCancel, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-stale-running/cancel", nil)
	r.ServeHTTP(wCancel, reqCancel)
	if wCancel.Code != http.StatusOK {
		t.Errorf("cleanup cancel returned %d", wCancel.Code)
	}
}

func TestRunLeaseBlocksPeerRunAndRunAll(t *testing.T) {
	st := store.NewMemoryStore()
	blockCh := make(chan struct{})
	startedCh := make(chan struct{}, 1)
	mp := &mockPlanner{blockCh: blockCh, startedCh: startedCh}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	// Separate routers create separate Handler.activeTasks maps, simulating two
	// service instances that coordinate only through the shared Store lease.
	r1 := setupTestRouter(t, st, engine)
	r2 := setupTestRouter(t, st, engine)

	task := &types.Task{
		ID:         "task-peer-lease",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatalf("SaveFullTask: %v", err)
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-peer-lease/run", nil)
		r1.ServeHTTP(w, req)
		firstDone <- w
	}()

	select {
	case <-startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first /run to acquire lease")
	}

	wRun := httptest.NewRecorder()
	reqRun, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-peer-lease/run", nil)
	r2.ServeHTTP(wRun, reqRun)
	if wRun.Code != http.StatusConflict {
		t.Fatalf("peer /run status = %d, want 409: %s", wRun.Code, wRun.Body.String())
	}

	wRunAll := httptest.NewRecorder()
	reqRunAll, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-peer-lease/run-all", nil)
	r2.ServeHTTP(wRunAll, reqRunAll)
	if wRunAll.Code != http.StatusConflict {
		t.Fatalf("peer /run-all status = %d, want 409: %s", wRunAll.Code, wRunAll.Body.String())
	}

	close(blockCh)
	select {
	case w := <-firstDone:
		if w.Code != http.StatusOK {
			t.Fatalf("first /run status = %d, want 200: %s", w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first /run did not finish after planner was released")
	}
}

// TestRunAllTimeoutConfigHasDefault is a smoke test that the new
// orchestrator.run_all_timeout_seconds knob is registered with a sane default,
// so existing deployments running without this key still get the original
// 10-minute budget rather than a zero-timeout context that cancels instantly.
func TestRunAllTimeoutConfigHasDefault(t *testing.T) {
	cfg := config.Get()
	if cfg.Orchestrator.RunAllTimeoutSeconds <= 0 {
		t.Fatalf("RunAllTimeoutSeconds default = %d, want positive (>= 600)", cfg.Orchestrator.RunAllTimeoutSeconds)
	}
}

func TestRunAllPreservesLLMRuntimeWithoutRequestCancellation(t *testing.T) {
	st := store.NewMemoryStore()
	blockCh := make(chan struct{})
	runtimeCh := make(chan *llmcore.Runtime, 1)
	mp := &mockPlanner{blockCh: blockCh, runtimeCh: runtimeCh}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}

	gin.SetMode(gin.TestMode)
	runtime := llmcore.NewDefaultRuntime(nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(llmcore.WithRuntime(c.Request.Context(), runtime))
		c.Next()
	})
	handler := api.RegisterRoutes(r, st, engine, nil)

	task := &types.Task{ID: "task-runtime-context", Status: types.StatusCreated, MaxSteps: 2, ToolBudget: 2}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatalf("SaveFullTask: %v", err)
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(requestCtx, http.MethodPost, "/api/tasks/task-runtime-context/run-all", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("run-all status = %d, want 202: %s", w.Code, w.Body.String())
	}

	select {
	case got := <-runtimeCh:
		if got != runtime {
			t.Fatalf("background runtime = %p, want injected runtime %p", got, runtime)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background planner")
	}

	// The accepted task owns its lifecycle; disconnecting the HTTP caller must
	// not cancel it, but the request's Runtime value must remain available.
	cancelRequest()
	close(blockCh)
	handler.Wait()
	want := waitForTaskStatus(t, st, task.ID, types.StatusCompleted)
	if want.FinalAnswer != "Mock final answer" {
		t.Fatalf("FinalAnswer = %q, want mock answer", want.FinalAnswer)
	}
}

func TestRunAllDuplicateActiveTaskReturnsConflict(t *testing.T) {
	st := store.NewMemoryStore()
	blockCh := make(chan struct{})
	mp := &mockPlanner{blockCh: blockCh}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	r := setupTestRouter(t, st, engine)

	task := &types.Task{
		ID:         "task-duplicate",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	wFirst := httptest.NewRecorder()
	reqFirst, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-duplicate/run-all", nil)
	r.ServeHTTP(wFirst, reqFirst)
	if wFirst.Code != http.StatusAccepted {
		t.Fatalf("expected first run-all status 202, got %d", wFirst.Code)
	}

	wSecond := httptest.NewRecorder()
	reqSecond, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-duplicate/run-all", nil)
	r.ServeHTTP(wSecond, reqSecond)
	if wSecond.Code != http.StatusConflict {
		t.Fatalf("expected duplicate run-all status 409, got %d", wSecond.Code)
	}

	wCancel := httptest.NewRecorder()
	reqCancel, _ := http.NewRequest(http.MethodDelete, "/api/tasks/task-duplicate/cancel", nil)
	r.ServeHTTP(wCancel, reqCancel)
	if wCancel.Code != http.StatusOK {
		t.Fatalf("expected cancel status 200, got %d", wCancel.Code)
	}
}

// TestApproveSinglePendingResolvesImplicitly covers the common path: exactly
// one pending approval for a task → POST /approve with no body succeeds and
// returns the approval payload.
func TestApproveSinglePendingResolvesImplicitly(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	id, ch := orchestrator.RegisterApproval("task-approve-single", &types.ApprovalRequest{
		TaskID: "task-approve-single",
		Action: "write_file",
	})
	t.Cleanup(func() { orchestrator.RemoveApproval(id) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-approve-single/approve", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case got := <-ch:
		if got.Approved != true {
			t.Errorf("approval channel received %v, want true", got.Approved)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("approval channel not signaled after /approve")
	}

	var body struct {
		Message  string                 `json:"message"`
		Approval *types.ApprovalRequest `json:"approval"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Approval == nil || body.Approval.ID != id {
		t.Errorf("response approval = %+v, want ID %q", body.Approval, id)
	}
}

// TestApproveMultiPendingReturnsConflict guards the disambiguation contract:
// when >1 approvals are pending for the same task, an implicit /approve must
// surface 409 with the pending IDs so the client can pick one.
func TestApproveMultiPendingReturnsConflict(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	taskID := "task-approve-multi"
	id1, _ := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "first"})
	id2, _ := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "second"})
	t.Cleanup(func() {
		orchestrator.RemoveApproval(id1)
		orchestrator.RemoveApproval(id2)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/approve", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on ambiguous pending, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Error        string   `json:"error"`
		PendingCount int      `json:"pending_count"`
		ApprovalIDs  []string `json:"approval_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.PendingCount != 2 {
		t.Errorf("pending_count = %d, want 2", body.PendingCount)
	}
	gotIDs := map[string]bool{}
	for _, id := range body.ApprovalIDs {
		gotIDs[id] = true
	}
	if !gotIDs[id1] || !gotIDs[id2] {
		t.Errorf("approval_ids = %v, want both %q and %q", body.ApprovalIDs, id1, id2)
	}

	// Both must still be pending — the conflict response must NOT have resolved either.
	if got := orchestrator.PendingApprovalCount(taskID); got != 2 {
		t.Errorf("pending count after 409 = %d, want 2 (neither should have been resolved)", got)
	}
}

// TestRejectByApprovalIDResolvesSpecificEntry covers the explicit-ID path: the
// client picks one of the IDs the 409 surfaced and rejects it; only that
// entry's channel must receive false, and the sibling must stay pending.
func TestRejectByApprovalIDResolvesSpecificEntry(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	taskID := "task-reject-explicit"
	id1, ch1 := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "first"})
	id2, ch2 := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "second"})
	t.Cleanup(func() {
		orchestrator.RemoveApproval(id1)
		orchestrator.RemoveApproval(id2)
	})

	body := `{"approval_id":"` + id2 + `"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/reject", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case got := <-ch2:
		if got.Approved != false {
			t.Errorf("ch2 received %v, want false", got.Approved)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ch2 not signaled after /reject")
	}

	// ch1 must still be blocking — only the targeted entry was resolved.
	select {
	case got := <-ch1:
		t.Fatalf("ch1 unexpectedly received %v; only id2 was rejected", got)
	case <-time.After(50 * time.Millisecond):
		// Expected: id1 still pending.
	}

	if got := orchestrator.PendingApprovalCount(taskID); got != 1 {
		t.Errorf("after rejecting id2, pending count = %d, want 1", got)
	}
}

// TestApproveByApprovalIDNotFoundReturns404 guards the negative path:
// an unknown approval_id must 404 rather than resolving the unrelated single
// pending entry that happens to share the task.
func TestApproveByApprovalIDNotFoundReturns404(t *testing.T) {
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	taskID := "task-approve-unknown-id"
	id, ch := orchestrator.RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "real"})
	t.Cleanup(func() { orchestrator.RemoveApproval(id) })

	body := `{"approval_id":"definitely-not-a-real-id"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/approve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown approval_id, got %d: %s", w.Code, w.Body.String())
	}

	// The real pending entry must not have been resolved as collateral damage.
	select {
	case got := <-ch:
		t.Fatalf("real pending was unexpectedly resolved with %v after a 404 on a different ID", got)
	case <-time.After(50 * time.Millisecond):
	}

	if got := orchestrator.PendingApprovalCount(taskID); got != 1 {
		t.Errorf("pending count after 404 = %d, want 1 (real entry untouched)", got)
	}
}

func TestAuthMiddleware(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = "my-test-secret-api-key"
	}))

	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	// Test 1: No Key provided -> 401
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when key is missing, got %d", w1.Code)
	}

	// Test 2: Invalid Key provided -> 401
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req2.Header.Set("X-API-Key", "wrong-key")
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when key is wrong, got %d", w2.Code)
	}

	// Test 3: Correct X-API-Key -> 200
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req3.Header.Set("X-API-Key", "my-test-secret-api-key")
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("expected 200 with correct X-API-Key, got %d: %s", w3.Code, w3.Body.String())
	}

	// Test 4: Correct Authorization Bearer -> 200
	w4 := httptest.NewRecorder()
	req4, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req4.Header.Set("Authorization", "Bearer my-test-secret-api-key")
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Errorf("expected 200 with correct Authorization Bearer, got %d: %s", w4.Code, w4.Body.String())
	}
}

func TestTenantAuthenticationAndTaskIsolation(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.Auth.RequireTenantWorkspaceRoot = true
		cfg.API.APIKey = "my-test-secret-api-key"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "tenant-a-test-key", WorkspaceRoot: "./testdata/tenant-a", DailyLLMCallBudget: 2},
			"tenant-b": {APIKey: "tenant-b-test-key", WorkspaceRoot: "./testdata/tenant-b"},
		}
	}))
	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	create := func(key, id, workspace string) {
		t.Helper()
		w := httptest.NewRecorder()
		body := `{"id":"` + id + `","goal":"x","workspace":"` + workspace + `"}`
		req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", key)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", id, w.Code, w.Body.String())
		}
	}
	create("tenant-a-test-key", "tenant-a-task", "./testdata/tenant-a/project")
	create("tenant-b-test-key", "tenant-b-task", "./testdata/tenant-b/project")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"id":"tenant-a-escape","goal":"x","workspace":"./testdata/tenant-b/project"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant workspace = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/tasks/tenant-b-task", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	var list struct {
		Tasks []types.Task `json:"tasks"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Tasks) != 1 || list.Tasks[0].TenantID != "tenant-a" {
		t.Fatalf("tenant list = %d %s", w.Code, w.Body.String())
	}
	for _, mem := range []*types.Memory{
		{ID: "tenant-a-memory", TenantID: "tenant-a", Timestamp: time.Now()},
		{ID: "tenant-b-memory", TenantID: "tenant-b", Timestamp: time.Now()},
	} {
		if err := st.SaveMemory(context.Background(), mem); err != nil {
			t.Fatal(err)
		}
	}
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/memories", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"tenant-a-memory"`) || strings.Contains(w.Body.String(), `"id":"tenant-b-memory"`) {
		t.Fatalf("tenant memory list = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/memories/tenant-b-memory", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant memory delete = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/memories?confirm=true", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("tenant memory clear = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("tenant metrics access = %d", w.Code)
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/prompt/init", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("tenant prompt initialization access = %d", w.Code)
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/usage", nil)
	req.Header.Set("X-API-Key", "tenant-a-test-key")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tenant_id":"tenant-a"`) || !strings.Contains(w.Body.String(), `"llm_calls":2`) {
		t.Fatalf("tenant usage = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("X-API-Key", "my-test-secret-api-key")
	r.ServeHTTP(w, req)
	list.Tasks = nil
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Tasks) != 2 {
		t.Fatalf("admin list = %d %s", w.Code, w.Body.String())
	}
}

func TestAuthMiddleware_FailClosed(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.API.Auth.Mode = "api_key"
		cfg.API.Tenants = nil
	}))

	st := store.NewMemoryStore()
	r := setupTestRouter(t, st, nil)

	// Temporarily switch mode to DebugMode so the test-bypass is not triggered.
	originalMode := gin.Mode()
	gin.SetMode(gin.DebugMode)
	defer gin.SetMode(originalMode)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/tasks", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable when API key is empty/unset, got %d", w.Code)
	}
}

func TestRunTaskStep_Streaming(t *testing.T) {
	st := store.NewMemoryStore()
	mp := &mockPlanner{
		tokens: []string{`{"thought_summary":"must stay private","final_answer":"token1`, `token2","stop":true}`},
	}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	engine.TokenCallback = func(taskID string, chunk string) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: types.StatusRunning,
			Token:  chunk,
		})
	}
	r := setupTestRouter(t, st, engine)

	task := &types.Task{
		ID:         "task-stream-run",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-stream-run/run?stream=true", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if contentType := w.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %q", contentType)
	}

	body := w.Body.String()
	if !strings.Contains(body, "token1") || !strings.Contains(body, "token2") {
		t.Errorf("expected body to contain streamed tokens, got %q", body)
	}
	if strings.Contains(body, "must stay private") || strings.Contains(body, "thought_summary") {
		t.Errorf("planner internals leaked into SSE body: %q", body)
	}
}

func TestRunAll_Streaming(t *testing.T) {
	st := store.NewMemoryStore()
	mp := &mockPlanner{
		tokens: []string{`{"thought_summary":"private","final_answer":"token-`, `all","stop":true}`},
	}
	engine := &orchestrator.Engine{
		Mode:     orchestrator.ModeLegacy,
		Planner:  mp,
		Executor: &mockExecutor{},
		Store:    st,
	}
	engine.TokenCallback = func(taskID string, chunk string) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: types.StatusRunning,
			Token:  chunk,
		})
	}
	engine.StepCallback = func(taskID string, status types.TaskStatus, step *types.StepTrace) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: status,
			Step:   step,
		})
	}
	r := setupTestRouter(t, st, engine)

	task := &types.Task{
		ID:         "task-stream-runall",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 10,
	}
	_ = st.SaveFullTask(context.Background(), task)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/task-stream-runall/run-all?stream=true", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if contentType := w.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %q", contentType)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"token":"token-"`) || !strings.Contains(body, `"token":"all"`) {
		t.Errorf("expected body to contain streamed events, got %q", body)
	}
	if strings.Contains(body, `"private"`) || strings.Contains(body, "thought_summary") {
		t.Errorf("planner internals leaked into SSE body: %q", body)
	}
	if !strings.Contains(body, `"final_answer":"Mock final answer"`) {
		t.Errorf("expected terminal event to contain final answer, got %q", body)
	}
}
