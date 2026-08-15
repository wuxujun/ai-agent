package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/skills"
	"github.com/wuxujun/ai-agent/internal/telemetry"
	"github.com/wuxujun/ai-agent/internal/tools"
)

var slog = logger.Component("server")

// approvalBusLogInfo contains only non-secret Redis connection metadata that
// is safe to emit to application logs. In particular, redis.Options separates
// the endpoint from URL userinfo, so usernames and passwords never reach the
// logger.
type approvalBusLogInfo struct {
	Address string
	DB      int
	TLS     bool
}

func newApprovalBusLogInfo(opts *redis.Options) approvalBusLogInfo {
	if opts == nil {
		return approvalBusLogInfo{}
	}
	return approvalBusLogInfo{
		Address: opts.Addr,
		DB:      opts.DB,
		TLS:     opts.TLSConfig != nil,
	}
}

type approvalBusCloser interface {
	Close()
}

type redisClientCloser interface {
	Close() error
}

// approvalBusRuntime owns every background resource created for distributed
// approval/cancel signalling. Close is called after task draining so suspended
// tasks can still receive remote signals while graceful shutdown is in flight.
type approvalBusRuntime struct {
	cancel context.CancelFunc
	bus    approvalBusCloser
	client redisClientCloser
	wg     sync.WaitGroup
}

func (r *approvalBusRuntime) Close() error {
	if r == nil {
		return nil
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.bus != nil {
		r.bus.Close()
	}
	r.wg.Wait()
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Printf("server stopped with error: %v", err)
		os.Exit(1)
	}
}

func run() error {
	// Load environment variables from .env file if available
	if err := godotenv.Load(); err != nil {
		// Fallback to parent directory search (e.g. if running from cmd/server)
		_ = godotenv.Load("../../.env")
	}

	cfg := config.Get()

	if err := configureLogger(cfg); err != nil {
		return fmt.Errorf("initialize file logging: %w", err)
	}
	defer func() {
		if err := logger.Close(); err != nil {
			log.Printf("failed to close log files: %v", err)
		}
	}()

	shutdown := telemetry.NoopShutdown
	if cfg.Telemetry.Enabled {
		endpoint := cfg.Telemetry.Endpoint
		if cfg.Telemetry.Exporter == "stdout" {
			endpoint = "stdout"
		}
		var err error
		shutdown, err = telemetry.InitOTel("ai-agent", cfg.Telemetry.Environment, endpoint)
		if err != nil {
			return fmt.Errorf("initialize telemetry: %w", err)
		}
		slog.Info("telemetry initialized",
			"endpoint", endpoint,
			"environment", cfg.Telemetry.Environment,
			"exporter", cfg.Telemetry.Exporter,
		)
	} else {
		slog.Info("telemetry disabled by config")
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			slog.Error("telemetry shutdown failed", "error", err)
		}
	}()

	if cfg.Langfuse.BootstrapMissingPrompts {
		summary, err := bootstrapLangfusePrompts(cfg)
		if err != nil {
			if strings.EqualFold(strings.TrimSpace(cfg.Langfuse.BootstrapFailurePolicy), "warn") {
				slog.Warn("Langfuse prompt bootstrap failed; local fallbacks remain available", "error", err)
			} else {
				return fmt.Errorf("bootstrap Langfuse prompts: %w", err)
			}
		} else {
			slog.Info("Langfuse prompts synchronized",
				"existing", summary.Existing,
				"created", summary.Created,
			)
		}
	}

	// Start filesystem watcher so the config is hot-reloaded automatically
	// whenever config.yaml is saved on disk (no signal required).
	config.Watch()

	// Build the skill capability layer before the planner first compiles its
	// decision schema. RegisterUseSkill adds the use_skill tool to
	// tools.DefaultRegistry, from which PlannerDecisionSchema,
	// PlannerDecisionGenAISchema and ValidateDecision all derive — so the
	// three-way invariant stays intact. A missing skills dir is non-fatal.
	skillRoot := cfg.Skill.Root
	if skillRoot == "" {
		skillRoot = "skills"
	}
	skillReg := skills.NewRegistry(skillRoot)
	if err := skillReg.Load(); err != nil {
		slog.Warn("skills load failed, continuing without skills", "error", err)
	} else {
		loadedSkills := skillReg.List()
		skillNames := make([]string, 0, len(loadedSkills))
		for _, skill := range loadedSkills {
			skillNames = append(skillNames, skill.Name)
		}
		slog.Info("skills loaded",
			"count", len(loadedSkills),
			"root", skillRoot,
			"skills", skillNames,
			"mode", cfg.Orchestrator.Mode,
		)
	}
	tools.RegisterUseSkill(skillReg)
	planner.SkillRegistry = skillReg

	st, err := buildStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	tools.RegisterRetrievalTools(tools.RetrievalDependencies{
		SearchRAG:    memory.SearchThirdPartyRAG,
		GetEmbedding: memory.GetEmbedding,
		MemoryStore:  st,
	})
	mcpRuntime, err := buildMCPRuntime(context.Background(), cfg, tools.DefaultRegistry)
	if err != nil {
		return err
	}
	defer func() {
		if err := mcpRuntime.Close(); err != nil {
			slog.Warn("MCP session cleanup failed", "error", err)
		}
	}()
	wikiRuntime, err := buildWikiRuntime(context.Background(), cfg, tools.DefaultRegistry)
	if err != nil {
		return err
	}
	defer func() {
		if err := wikiRuntime.Close(); err != nil {
			slog.Warn("LLM Wiki session cleanup failed", "error", err)
		}
	}()
	engineBuild, err := buildEngine(context.Background(), cfg, st, planner.ProbeOllama)
	if err != nil {
		return err
	}
	app := buildApp(cfg, st, engineBuild.engine, engineBuild.metrics, engineBuild.runtime)

	// Wait for SIGINT, SIGTERM (shutdown) or SIGHUP (hot-reload).
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(quit)
	slog.Info("HTTP server listening", "addr", app.server.Addr)
	return runApp(appRuntime{
		server: app.server,
		tasks:  app.tasks,
		bus:    app.bus,
		expiry: app.expiry,
		reload: func() error {
			_, changes, err := config.Reload()
			if err != nil {
				return err
			}
			slog.Info("config reloaded", "changes", len(changes))
			if err := configureLogger(config.Get()); err != nil {
				return fmt.Errorf("logging reconfiguration failed; keeping previous outputs: %w", err)
			}
			return nil
		},
	}, quit)
}

func bootstrapLangfusePrompts(cfg *config.Config) (multiagent.PromptBootstrapSummary, error) {
	timeoutSeconds := cfg.Langfuse.BootstrapTimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 15
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	teamsCfg, err := multiagent.LoadTeamsConfigStrict()
	if err != nil {
		return multiagent.PromptBootstrapSummary{}, err
	}
	return multiagent.BootstrapTeamPrompts(ctx, teamsCfg)
}

func configureLogger(cfg *config.Config) error {
	return logger.Configure(logger.Options{
		Level:         cfg.Log.Level,
		Console:       cfg.Log.Console,
		FileEnabled:   cfg.Log.FileEnabled,
		AccessEnabled: cfg.Log.AccessEnabled,
		Directory:     cfg.Log.Directory,
		RetentionDays: cfg.Log.RetentionDays,
	})
}
