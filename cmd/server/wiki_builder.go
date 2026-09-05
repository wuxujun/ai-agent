package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/brain"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type wikiClient interface {
	tools.WikiReader
	Initialize(context.Context) error
	Probe(context.Context) error
	Close(context.Context) error
}

type wikiClientFactory func(wiki.Config) (wikiClient, error)

type wikiRuntime struct {
	client wikiClient
	brain  *brainCorpusAdapter
}

type localWikiStatusProvider interface {
	Status() wiki.DirectoryStatus
}

type brainCorpusAdapter struct {
	provider *brain.Provider
	cfg      *config.Config
	root     string
}

func (a *brainCorpusAdapter) currentConfig() (*config.Config, error) {
	cfg := config.Get()
	if cfg == nil || !cfg.Brain.Enabled || a == nil || filepath.Clean(cfg.Brain.Root) != a.root {
		return nil, brain.ErrPinConfiguration
	}
	return cfg, nil
}

func (a *brainCorpusAdapter) SearchCorpus(ctx context.Context, query string, topK int, space string, scope tools.WikiScope) ([]wiki.Document, error) {
	cfg, err := a.currentConfig()
	if err != nil {
		return nil, err
	}
	ref, err := brain.ResolveProject(cfg, scope.TenantID, scope.BrainProjectID)
	if err != nil {
		return nil, err
	}
	return a.provider.SearchCorpus(ctx, query, topK, space, ref, scope.BrainSnapshotID)
}
func (a *brainCorpusAdapter) ReadCorpus(ctx context.Context, document wiki.Document, space string, scope tools.WikiScope) (wiki.Document, error) {
	cfg, err := a.currentConfig()
	if err != nil {
		return wiki.Document{}, err
	}
	ref, err := brain.ResolveProject(cfg, scope.TenantID, scope.BrainProjectID)
	if err != nil {
		return wiki.Document{}, err
	}
	return a.provider.ReadCorpus(ctx, document, space, ref, scope.BrainSnapshotID)
}
func (a *brainCorpusAdapter) GraphCorpus(ctx context.Context, document wiki.Document, space string, depth int, direction string, scope tools.WikiScope) (wiki.GraphResult, error) {
	cfg, err := a.currentConfig()
	if err != nil {
		return wiki.GraphResult{}, err
	}
	ref, err := brain.ResolveProject(cfg, scope.TenantID, scope.BrainProjectID)
	if err != nil {
		return wiki.GraphResult{}, err
	}
	return a.provider.GraphCorpus(ctx, document, space, depth, direction, ref, scope.BrainSnapshotID)
}
func (a *brainCorpusAdapter) CurrentWatermark(ctx context.Context, scope tools.WikiScope) (string, error) {
	cfg, err := a.currentConfig()
	if err != nil {
		return "", err
	}
	ref, err := brain.ResolveProject(cfg, scope.TenantID, scope.BrainProjectID)
	if err != nil {
		return "", err
	}
	return a.provider.Ledger.Watermark(ctx, ref)
}
func (a *brainCorpusAdapter) BrainSpace(scope tools.WikiScope) string {
	cfg, err := a.currentConfig()
	if err != nil {
		return ""
	}
	ref, err := brain.ResolveProject(cfg, scope.TenantID, scope.BrainProjectID)
	if err != nil {
		return ""
	}
	return ref.WikiSpace
}

func (a *brainCorpusAdapter) ReadBrain(ctx context.Context, document wiki.Document, space, tenant, project, snapshot string) (wiki.Document, error) {
	cfg, err := a.currentConfig()
	if err != nil {
		return wiki.Document{}, err
	}
	ref, err := brain.ResolveProject(cfg, tenant, project)
	if err != nil || ref.WikiSpace != space {
		return wiki.Document{}, fmt.Errorf("brain page is not authorized")
	}
	return a.provider.ReadCorpus(ctx, document, space, ref, snapshot)
}

func (a *brainCorpusAdapter) BrainStatus(ctx context.Context) any {
	cfg, err := a.currentConfig()
	if err != nil {
		return map[string]any{"configured": true, "healthy": false, "current_projects": 0}
	}
	projects, revoked, empty := 0, 0, 0
	for tenantID, tenant := range cfg.API.Tenants {
		for projectID := range tenant.BrainProjects {
			ref, resolveErr := brain.ResolveProject(cfg, tenantID, projectID)
			if resolveErr != nil {
				continue
			}
			projects++
			status, statusErr := a.provider.Repository.Status(ctx, ref)
			if statusErr != nil || status.RevocationState == "revoked_or_invalid" {
				revoked++
			}
			if status.Current == "" {
				empty++
			}
		}
	}
	// Empty CURRENT is a reportable lifecycle state, not a server readiness
	// failure for unrelated ordinary Wiki traffic.
	return map[string]any{"configured": true, "healthy": revoked == 0, "current_projects": projects - empty, "empty_projects": empty, "revoked_projects": revoked}
}

func attachBrainCorpus(cfg *config.Config, registry *tools.Registry, client wikiClient) (*brainCorpusAdapter, error) {
	if cfg == nil || !cfg.Brain.Enabled {
		return nil, nil
	}
	adapter, err := newBrainCorpusAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return adapter, tools.RegisterWikiToolsWithCorpus(registry, client, &tools.CorpusRouter{Ordinary: client, Brain: adapter, MaxMerge: 10})
}

func newBrainCorpusAdapter(cfg *config.Config) (*brainCorpusAdapter, error) {
	ledger := brain.NewFileRetractionLedger(cfg.Brain.Root)
	repo, err := brain.NewRepository(cfg.Brain.Root, ledger)
	if err != nil {
		return nil, err
	}
	return &brainCorpusAdapter{provider: brain.NewProvider(repo, ledger), cfg: cfg, root: filepath.Clean(cfg.Brain.Root)}, nil
}

func (r *wikiRuntime) Check(ctx context.Context) error {
	if r == nil || r.client == nil {
		return fmt.Errorf("wiki is not configured or initialized")
	}
	return r.client.Probe(ctx)
}

func (r *wikiRuntime) Status() any {
	if r == nil || r.client == nil {
		return nil
	}
	if provider, ok := r.client.(localWikiStatusProvider); ok {
		return provider.Status()
	}
	return map[string]any{"backend": "remote"}
}

func (r *wikiRuntime) Read(ctx context.Context, document wiki.Document, space string) (wiki.Document, error) {
	if r == nil || r.client == nil {
		return wiki.Document{}, fmt.Errorf("wiki is not configured or initialized")
	}
	return r.client.Read(ctx, document, space)
}

func newWikiClient(cfg wiki.Config) (wikiClient, error) { return wiki.New(cfg) }

func buildWikiRuntime(ctx context.Context, cfg *config.Config, registry *tools.Registry) (*wikiRuntime, error) {
	return buildWikiRuntimeWithFactory(ctx, cfg, registry, newWikiClient)
}

func buildWikiRuntimeWithFactory(ctx context.Context, cfg *config.Config, registry *tools.Registry, factory wikiClientFactory) (*wikiRuntime, error) {
	runtime := &wikiRuntime{}
	if cfg != nil && cfg.Brain.Enabled {
		adapter, err := newBrainCorpusAdapter(cfg)
		if err != nil {
			return nil, err
		}
		runtime.brain = adapter
	}
	if cfg == nil || (strings.TrimSpace(cfg.Wiki.URL) == "" && strings.TrimSpace(cfg.Wiki.Directory) == "") {
		return runtime, nil
	}
	if strings.TrimSpace(cfg.Wiki.Directory) != "" {
		client, err := wiki.NewDirectory(cfg.Wiki.Directory,
			wiki.WithSearchMode(cfg.Wiki.LocalSearchMode),
			wiki.WithRefreshInterval(time.Duration(cfg.Wiki.LocalRefreshIntervalSeconds)*time.Second),
			wiki.WithGraphMaxNodes(cfg.Wiki.LocalGraphMaxNodes),
		)
		if err == nil {
			err = client.Initialize(ctx)
		}
		if err != nil {
			if cfg.Wiki.Required {
				return nil, fmt.Errorf("initialize required local Wiki: %w", err)
			}
			slog.Warn("optional local Wiki unavailable", "error", err)
			return runtime, nil
		}
		if err := tools.RegisterWikiTools(registry, client); err != nil {
			return nil, err
		}
		brainAdapter, err := attachBrainCorpus(cfg, registry, client)
		if err != nil {
			return nil, err
		}
		runtime.client = client
		runtime.brain = brainAdapter
		slog.Info("read-only local Wiki initialized",
			"directory", cfg.Wiki.Directory,
			"search_mode", cfg.Wiki.LocalSearchMode,
			"refresh_interval_seconds", cfg.Wiki.LocalRefreshIntervalSeconds,
			"graph_max_nodes", cfg.Wiki.LocalGraphMaxNodes,
		)
		return runtime, nil
	}
	authorization := ""
	if envName := strings.TrimSpace(cfg.Wiki.AuthorizationEnv); envName != "" {
		authorization = os.Getenv(envName)
		if authorization == "" {
			err := fmt.Errorf("wiki authorization environment variable %s is empty", envName)
			if cfg.Wiki.Required {
				return nil, err
			}
			slog.Warn("optional LLM Wiki unavailable", "error", err)
			return runtime, nil
		}
	}
	timeout := time.Duration(cfg.Wiki.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client, err := factory(wiki.Config{
		URL: cfg.Wiki.URL, Authorization: authorization, Timeout: timeout,
		AllowPrivateNetwork: cfg.Wiki.AllowPrivateNetwork,
	})
	if err == nil {
		initCtx, cancel := context.WithTimeout(ctx, timeout)
		err = client.Initialize(initCtx)
		cancel()
	}
	if err != nil {
		if client != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = client.Close(closeCtx)
			cancel()
		}
		if cfg.Wiki.Required {
			return nil, fmt.Errorf("initialize required LLM Wiki: %w", err)
		}
		slog.Warn("optional LLM Wiki unavailable", "error", err)
		return runtime, nil
	}
	if err := tools.RegisterWikiTools(registry, client); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.Close(closeCtx)
		cancel()
		return nil, err
	}
	brainAdapter, err := attachBrainCorpus(cfg, registry, client)
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.Close(closeCtx)
		cancel()
		return nil, err
	}
	runtime.client = client
	runtime.brain = brainAdapter
	slog.Info("read-only LLM Wiki initialized", "url", cfg.Wiki.URL, "default_space", cfg.Wiki.DefaultSpace)
	return runtime, nil
}

func (r *wikiRuntime) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.client.Close(ctx)
}
