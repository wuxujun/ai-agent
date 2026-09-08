package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/brain"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type brainEngineFixture struct {
	root     string
	cfg      *config.Config
	ref      brain.ProjectRef
	ledger   *brain.FileRetractionLedger
	repo     *brain.Repository
	pinner   brain.SnapshotPinner
	provider *brain.Provider
	store    *store.MemoryStore
}

type brainEnginePlanner struct {
	searchOnly bool
	stop       bool
}

func (p *brainEnginePlanner) PlanNext(_ context.Context, task *types.Task, _ func(string)) (*planner.PlanDecision, error) {
	if p.stop {
		return &planner.PlanDecision{Stop: true, FinalAnswer: "resumed"}, nil
	}
	if len(task.Trace) == 0 {
		return &planner.PlanDecision{Actions: []planner.ActionCall{{
			Action:     "wiki_search",
			Parameters: map[string]any{"query": "pinned brain", "top_k": 3, "corpus": "brain"},
		}}}, nil
	}
	if p.searchOnly {
		return &planner.PlanDecision{Stop: true, FinalAnswer: "searched"}, nil
	}
	id := brainCandidateID(task.Trace)
	if id == "" {
		return &planner.PlanDecision{Stop: true, FinalAnswer: "no candidate"}, nil
	}
	return &planner.PlanDecision{Actions: []planner.ActionCall{{
		Action:     "wiki_fetch",
		Parameters: map[string]any{"ids": []string{id}},
	}}}, nil
}

type brainOrdinaryReader struct{}

func (brainOrdinaryReader) Search(context.Context, string, int, string) ([]wiki.Document, error) {
	return nil, errors.New("ordinary Wiki is not configured")
}

func (brainOrdinaryReader) Read(context.Context, wiki.Document, string) (wiki.Document, error) {
	return wiki.Document{}, errors.New("ordinary Wiki is not configured")
}

type brainEngineCorpus struct {
	provider *brain.Provider
	cfg      *config.Config
}

func (c brainEngineCorpus) resolve(scope tools.WikiScope) (brain.ProjectRef, error) {
	return brain.ResolveProject(c.cfg, scope.TenantID, scope.BrainProjectID)
}

func (c brainEngineCorpus) SearchCorpus(ctx context.Context, query string, topK int, space string, scope tools.WikiScope) ([]wiki.Document, error) {
	ref, err := c.resolve(scope)
	if err != nil {
		return nil, err
	}
	return c.provider.SearchCorpus(ctx, query, topK, space, ref, scope.BrainSnapshotID)
}

func (c brainEngineCorpus) ReadCorpus(ctx context.Context, document wiki.Document, space string, scope tools.WikiScope) (wiki.Document, error) {
	ref, err := c.resolve(scope)
	if err != nil {
		return wiki.Document{}, err
	}
	return c.provider.ReadCorpus(ctx, document, space, ref, scope.BrainSnapshotID)
}

func (c brainEngineCorpus) GraphCorpus(ctx context.Context, document wiki.Document, space string, depth int, direction string, scope tools.WikiScope) (wiki.GraphResult, error) {
	ref, err := c.resolve(scope)
	if err != nil {
		return wiki.GraphResult{}, err
	}
	return c.provider.GraphCorpus(ctx, document, space, depth, direction, ref, scope.BrainSnapshotID)
}

func (c brainEngineCorpus) CurrentWatermark(ctx context.Context, scope tools.WikiScope) (string, error) {
	ref, err := c.resolve(scope)
	if err != nil {
		return "", err
	}
	return c.provider.Ledger.Watermark(ctx, ref)
}

func (c brainEngineCorpus) BrainSpace(scope tools.WikiScope) string {
	ref, err := c.resolve(scope)
	if err != nil {
		return ""
	}
	return ref.WikiSpace
}

func newBrainEngineFixture(t *testing.T) *brainEngineFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ledger := brain.NewFileRetractionLedger(root)
	repo, err := brain.NewRepository(root, ledger)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Brain.Enabled = true
	cfg.Brain.Root = root
	cfg.API.Tenants = map[string]config.APITenantConfig{
		"tenant-a": {
			WikiSpace: "ordinary",
			BrainProjects: map[string]config.BrainProjectConfig{
				"atlas": {WikiSpace: "brain-atlas"},
			},
		},
	}
	ref, err := brain.ResolveProject(cfg, "tenant-a", "atlas")
	if err != nil {
		t.Fatal(err)
	}
	restore := config.OverrideForTesting(func(global *config.Config) { *global = *cfg })
	t.Cleanup(restore)
	store := store.NewMemoryStore()
	t.Cleanup(func() { _ = store.Close() })
	fixture := &brainEngineFixture{
		root: root, cfg: cfg, ref: ref, ledger: ledger, repo: repo,
		pinner:   brain.NewSnapshotPinner(repo, ledger, cfg),
		provider: brain.NewProvider(repo, ledger), store: store,
	}
	fixture.publish(t, "brain-snap-1", "", "Pinned Brain page")
	return fixture
}

func (f *brainEngineFixture) publish(t *testing.T, snapshotID, expectedCurrent, body string) string {
	t.Helper()
	watermark, err := f.ledger.Watermark(t.Context(), f.ref)
	if err != nil {
		t.Fatal(err)
	}
	draft := brain.SnapshotDraft{
		Files: map[string][]byte{
			"_index.md":          []byte("# Brain index\n"),
			"concepts/pinned.md": []byte("# Pinned Brain page\n\n" + body + "\n"),
		},
		Manifest: brain.Manifest{
			SnapshotID: snapshotID, ParentID: expectedCurrent,
			TenantID: f.ref.TenantID, ProjectID: f.ref.ProjectID,
			ExpectedCurrent: expectedCurrent, RetractionWatermark: watermark,
			Validation: brain.ValidationReport{Publishable: true},
		},
	}
	manifest, err := f.repo.CreateStage(t.Context(), f.ref, draft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Publish(t.Context(), f.ref, snapshotID, expectedCurrent); err != nil {
		t.Fatal(err)
	}
	return manifest.SnapshotID
}

func registerBrainEngineTools(t *testing.T, provider *brain.Provider) {
	t.Helper()
	for _, name := range []string{"wiki_search", "wiki_fetch", "wiki_graph", "wiki_graph_fetch"} {
		tools.Unregister(name)
	}
	ordinary := brainOrdinaryReader{}
	corpus := brainEngineCorpus{provider: provider, cfg: config.Get()}
	router := &tools.CorpusRouter{Ordinary: ordinary, Brain: corpus, MaxMerge: 10}
	if err := tools.RegisterWikiToolsWithCorpus(tools.DefaultRegistry, ordinary, router); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, name := range []string{"wiki_search", "wiki_fetch", "wiki_graph", "wiki_graph_fetch"} {
			tools.Unregister(name)
		}
	})
}

func brainCandidateID(trace []types.StepTrace) string {
	for index := len(trace) - 1; index >= 0; index-- {
		if trace[index].Action != "wiki_search" {
			continue
		}
		var payload struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		}
		if json.Unmarshal([]byte(trace[index].Observation), &payload) == nil && len(payload.Results) > 0 {
			return payload.Results[0].ID
		}
	}
	return ""
}

func newBrainEngine(fixture *brainEngineFixture, plannerClient planner.Planner) *Engine {
	return &Engine{
		Planner: plannerClient, Executor: &executor.DefaultExecutor{}, Mode: ModeLegacy,
		Store: fixture.store, BrainPinner: fixture.pinner,
	}
}

func TestBrainEngineNextUsesPinnedSnapshotAndReadOnlyWikiTools(t *testing.T) {
	fixture := newBrainEngineFixture(t)
	registerBrainEngineTools(t, fixture.provider)
	plannerClient := &brainEnginePlanner{}
	engine := newBrainEngine(fixture, plannerClient)
	task := &types.Task{ID: "brain-engine-e2e", TenantID: fixture.ref.TenantID, BrainProjectID: fixture.ref.ProjectID, Goal: "read the pinned Brain page", Status: types.StatusCreated, MaxSteps: 4, ToolBudget: 4}
	if err := engine.Next(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if task.BrainSnapshotID != "brain-snap-1" || task.BrainConfigDigest == "" || task.Status != types.StatusRunning {
		t.Fatalf("after search task = %+v", task)
	}
	if err := fixture.store.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := engine.Next(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if len(task.Trace) < 2 || len(task.Trace[1].Evidence) != 1 || task.Trace[1].Evidence[0].Path != "wiki://brain-atlas/concepts/pinned" {
		t.Fatalf("fetch trace = %+v", task.Trace)
	}
	if err := fixture.store.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	fixture.publish(t, "brain-snap-2", "brain-snap-1", "replacement page")
	resumed, err := fixture.store.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	resumed.Status = types.StatusRunning
	if err := fixture.store.SaveFullTask(t.Context(), resumed); err != nil {
		t.Fatal(err)
	}
	restarted := newBrainEngine(fixture, &brainEnginePlanner{stop: true})
	if err := restarted.Next(t.Context(), resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.BrainSnapshotID != "brain-snap-1" || resumed.Status != types.StatusCompleted {
		t.Fatalf("restart changed pinned snapshot: %+v", resumed)
	}
}

func TestBrainEngineNextRejectsWatermarkChangeBeforeFetch(t *testing.T) {
	fixture := newBrainEngineFixture(t)
	registerBrainEngineTools(t, fixture.provider)
	task := &types.Task{ID: "brain-engine-watermark", TenantID: fixture.ref.TenantID, BrainProjectID: fixture.ref.ProjectID, Goal: "read the pinned Brain page", Status: types.StatusCreated, MaxSteps: 3, ToolBudget: 3}
	searchEngine := newBrainEngine(fixture, &brainEnginePlanner{searchOnly: true})
	if err := searchEngine.Next(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	retraction := brain.Retraction{EvidenceURI: "brain-evidence://tenant-a/atlas/tasks/watermark-change#trace/1", Reason: "operator correction", RetractedAt: time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)}
	encoded, err := json.Marshal(retraction)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(fixture.root, fixture.ref.StorageKey, fixture.ref.ProjectID, "retractions.jsonl")
	file, err := os.OpenFile(ledgerPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	before := len(task.Trace)
	fetchEngine := newBrainEngine(fixture, &brainEnginePlanner{})
	if err := fetchEngine.Next(t.Context(), task); !errors.Is(err, brain.ErrProviderWatermark) {
		t.Fatalf("watermark change error = %v", err)
	}
	if len(task.Trace) != before {
		t.Fatalf("stale fetch appended trace: before=%d after=%d", before, len(task.Trace))
	}
}

func TestBrainEngineNextPinsAcrossOrchestratorModes(t *testing.T) {
	cases := []struct {
		name       string
		mode       Mode
		wantStatus types.TaskStatus
		wantError  bool
	}{
		{name: "eino", mode: ModeEino, wantStatus: types.StatusCompleted},
		{name: "legacy", mode: ModeLegacy, wantStatus: types.StatusCompleted},
		{name: "step", mode: ModeStep, wantStatus: types.StatusCompleted},
		{name: "adk", mode: ModeAdk, wantStatus: types.StatusPartial},
		{name: "multiagent", mode: ModeMultiAgent, wantStatus: types.StatusFailed, wantError: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newBrainEngineFixture(t)
			engine := newBrainEngine(fixture, &brainEnginePlanner{stop: true})
			engine.Mode = testCase.mode
			task := &types.Task{
				ID: "brain-engine-mode-" + testCase.name, TenantID: fixture.ref.TenantID,
				BrainProjectID: fixture.ref.ProjectID, Goal: "pin Brain in " + testCase.name,
				Status: types.StatusCreated, MaxSteps: 1, ToolBudget: 1,
			}
			if testCase.mode == ModeStep || testCase.mode == ModeAdk {
				task.MaxSteps = 0
				task.ToolBudget = 0
			}
			err := engine.Next(t.Context(), task)
			if testCase.wantError != (err != nil) {
				t.Fatalf("Next error = %v, wantError=%t", err, testCase.wantError)
			}
			if task.BrainSnapshotID != "brain-snap-1" {
				t.Fatalf("mode %s did not pin snapshot: %+v", testCase.mode, task)
			}
			if task.Status != testCase.wantStatus {
				t.Fatalf("mode %s status = %s, want %s", testCase.mode, task.Status, testCase.wantStatus)
			}
		})
	}
}
