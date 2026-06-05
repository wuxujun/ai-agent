package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/wuxujun/ai-agent/internal/api"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/skills"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/telemetry"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

var slog = logger.Component("server")

func main() {
	// Load environment variables from .env file if available
	if err := godotenv.Load(); err != nil {
		// Fallback to parent directory search (e.g. if running from cmd/server)
		_ = godotenv.Load("../../.env")
	}

	shutdown, err := telemetry.InitOTel("ai-agent", "dev", "127.0.0.1:4318")
	if err != nil {
		log.Fatalf("failed to initialize telemetry: %v", err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			slog.Error("telemetry shutdown failed", "error", err)
		}
	}()

	cfg := config.Get()

	// Sync log level from config into the structured logger.
	logger.Reinit(cfg.Log.Level)

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
		slog.Info("skills loaded", "count", len(skillReg.List()), "root", skillRoot)
	}
	tools.RegisterUseSkill(skillReg)
	planner.SkillRegistry = skillReg

	var st store.Store
	var initErr error

	switch cfg.Store.Type {
	case "memory":
		st = store.NewMemoryStore()
	case "postgres":
		if cfg.Store.DSN == "" {
			log.Fatal("Store DSN is required when AI_AGENT_STORE_TYPE=postgres")
		}
		st, initErr = store.NewPostgresStore(cfg.Store.DSN)
		if initErr != nil {
			log.Fatalf("failed to initialize PostgresStore: %v", initErr)
		}
	case "redis":
		if cfg.Store.DSN == "" {
			log.Fatal("Store DSN (Redis URL) is required when AI_AGENT_STORE_TYPE=redis")
		}
		st, initErr = store.NewRedisStoreFromURL(cfg.Store.DSN)
		if initErr != nil {
			log.Fatalf("failed to initialize RedisStore: %v", initErr)
		}
	case "sqlite":
		fallthrough
	default:
		dsn := cfg.Store.DSN
		if dsn == "" {
			dsn = "data/agent.db"
		}
		st, initErr = store.NewSQLiteStore(dsn)
		if initErr != nil {
			log.Fatalf("failed to initialize SQLiteStore: %v", initErr)
		}
	}
	defer st.Close()

	mode := orchestrator.Mode(cfg.Orchestrator.Mode)

	llmProvider := planner.ProviderType(cfg.ResolveLLMProvider())
	apiKey := cfg.ResolveLLMAPIKey(string(llmProvider))
	model := cfg.ResolveLLMModel(string(llmProvider))
	baseURL := cfg.ResolveLLMBaseURL(string(llmProvider))

	if apiKey == "" {
		if mode == orchestrator.ModeEino || mode == orchestrator.ModeLegacy || mode == orchestrator.ModeMultiAgent {
			log.Fatalf("%s provider requires an API Key", llmProvider)
		} else {
			slog.Warn("LLM API key missing",
				"provider", llmProvider,
				"mode", mode,
			)
		}
	}

	slog.Info("LLM provider configured",
		"provider", llmProvider,
		"base_url", baseURL,
		"model", model,
	)

	mc := metrics.NewCollector()

	plannerClient := planner.NewLLMPlannerWithProvider(
		llmProvider,
		apiKey,
		model,
		baseURL,
	)

	fallbackPlanner := &planner.MockPlanner{}

	combinedPlanner := &planner.FallbackPlanner{
		Primary:   plannerClient,
		Secondary: fallbackPlanner,
		Metrics:   mc,
	}

	eng := &orchestrator.Engine{
		Planner:  combinedPlanner,
		Executor: &executor.DefaultExecutor{},
		Metrics:  mc,
		Mode:     mode,
		Store:    st,
	}

	// Inject a Coordinator when running in multi-agent mode.
	// The Coordinator reuses the same LLM config as the main planner
	// (OPENAI_API_KEY / GEMINI_API_KEY / AI_AGENT_LLM_MODEL etc.).
	if mode == orchestrator.ModeMultiAgent {
		eng.Coordinator = multiagent.NewCoordinator(mc)
		eng.Coordinator.SuspendForApproval = eng.SuspendForApproval
		slog.Info("multi-agent mode enabled",
			"coordinator_provider", os.Getenv("AI_AGENT_LLM_PROVIDER"),
		)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(otelgin.Middleware("ai-agent"))
	apiHandler := api.RegisterRoutes(r, st, eng, mc)

	eng.EventCallback = func(taskID string, status types.TaskStatus) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: status,
		})
	}
	eng.StepCallback = func(taskID string, status types.TaskStatus, step *types.StepTrace) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: status,
			Step:   step,
		})
	}
	if eng.Coordinator != nil {
		eng.Coordinator.EventCallback = eng.EventCallback
	}
	eng.TokenCallback = func(taskID string, chunk string) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: types.StatusRunning,
			Token:  chunk,
		})
	}

	// Wrap Gin in a standard http.Server so we can gracefully shut it down.
	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	// Start HTTP server in the background.
	go func() {
		slog.Info("HTTP server listening", "addr", ":8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe failed: %v", err)
		}
	}()

	// Wait for SIGINT, SIGTERM (shutdown) or SIGHUP (hot-reload).
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

waitLoop:
	for {
		sig := <-quit
		switch sig {
		case syscall.SIGHUP:
			// Hot-reload: re-read config file + env vars, no restart needed.
			slog.Info("SIGHUP received, reloading configuration")
			if _, changes, err := config.Reload(); err != nil {
				slog.Error("config reload failed", "error", err)
			} else {
				slog.Info("config reloaded", "changes", len(changes))
				logger.Reinit(config.Get().Log.Level)
			}
		default:
			// SIGINT / SIGTERM — begin graceful shutdown.
			slog.Info("shutdown signal received, draining connections")
			break waitLoop
		}
	}

	// Give in-flight HTTP requests up to 10 seconds to complete.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP shutdown error", "error", err)
	}

	slog.Info("waiting for background tasks to finish")
	taskDrainCtx, taskDrainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer taskDrainCancel()
	if err := apiHandler.Shutdown(taskDrainCtx); err != nil {
		slog.Error("background task shutdown timed out", "error", err)
	}
	slog.Info("shutdown complete")
}
