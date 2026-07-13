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
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/wuxujun/ai-agent/internal/api"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/executor"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/llmprovider"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/policy"
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

	cfg := config.Get()

	// Sync log level from config into the structured logger.
	logger.Reinit(cfg.Log.Level)

	shutdown := telemetry.NoopShutdown
	if cfg.Telemetry.Enabled {
		endpoint := cfg.Telemetry.Endpoint
		if cfg.Telemetry.Exporter == "stdout" {
			endpoint = "stdout"
		}
		var err error
		shutdown, err = telemetry.InitOTel("ai-agent", cfg.Telemetry.Environment, endpoint)
		if err != nil {
			log.Fatalf("failed to initialize telemetry: %v", err)
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

	startupScene := config.LLMSceneTaskPlanner
	if mode == orchestrator.ModeMultiAgent {
		startupScene = config.LLMSceneMultiAgentPlanner
	}
	resolvedLLM := cfg.ResolveLLMScene(startupScene)
	llmProvider := planner.ProviderType(resolvedLLM.Provider)
	apiKey := resolvedLLM.APIKey
	model := resolvedLLM.Model
	baseURL := resolvedLLM.BaseURL

	providerSpec, providerRegistered := llmprovider.Lookup(string(llmProvider))
	requiresAPIKey := !providerRegistered || providerSpec.RequiresAPIKey
	if apiKey == "" && requiresAPIKey {
		if mode == orchestrator.ModeEino || mode == orchestrator.ModeLegacy || mode == orchestrator.ModeMultiAgent {
			log.Fatalf("%s provider requires an API Key", llmProvider)
		} else {
			slog.Warn("LLM API key missing",
				"provider", llmProvider,
				"mode", mode,
			)
		}
	}

	// ── Ollama Health Probe on Startup ───────────────────────────────────────
	// If the planner or embedding scene is configured to use Ollama, perform
	// a quick startup self-check to ensure the local Ollama service is running and
	// the requested models are already pulled.
	if llmProvider == planner.ProviderOllama {
		slog.Info("Probing Ollama planner model on startup...", "model", model)
		if err := planner.ProbeOllama(context.Background(), baseURL, model); err != nil {
			log.Fatalf("Ollama planner check failed: %v", err)
		}
		slog.Info("Ollama planner model self-check passed", "model", model)
	}

	embedLLM := cfg.ResolveLLMScene(config.LLMSceneEmbedding)
	if planner.ProviderType(embedLLM.Provider) == planner.ProviderOllama {
		slog.Info("Probing Ollama embedding model on startup...", "model", embedLLM.Model)
		if err := planner.ProbeOllama(context.Background(), embedLLM.BaseURL, embedLLM.Model); err != nil {
			log.Fatalf("Ollama embedding check failed: %v", err)
		}
		slog.Info("Ollama embedding model self-check passed", "model", embedLLM.Model)
	}

	slog.Info("LLM provider configured",
		"configured_provider", cfg.LLM.Provider,
		"provider", llmProvider,
		"scene", startupScene,
		"base_url", baseURL,
		"model", model,
	)

	mc := metrics.NewCollector()
	llmRuntime := llmcore.NewDefaultRuntime(mc)

	plannerClient := planner.NewLLMPlannerForScene(config.LLMSceneTaskPlanner)
	plannerClient.Compressor = planner.NewLLMContextCompressor(config.LLMSceneContextCompressor)
	plannerClient.ArgumentRepairer = planner.NewLLMToolArgumentRepairer(config.LLMSceneToolArgumentRepair)

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
	eng.Finalizer = planner.NewLLMTaskFinalizer(config.LLMSceneTaskFinalizer)
	eng.CitationVerifier = planner.NewLLMCitationVerifier(config.LLMSceneCitationVerifier)
	eng.SafetyGuard = policy.NewLLMSafetyGuard(config.LLMSceneSafetyGuard)

	// Inject a Coordinator when running in multi-agent mode.
	// The Coordinator reuses the same LLM config as the main planner
	// (OPENAI_API_KEY / GEMINI_API_KEY / AI_AGENT_LLM_MODEL etc.).
	if mode == orchestrator.ModeMultiAgent {
		eng.Coordinator = multiagent.NewCoordinator(mc)
		eng.Coordinator.Verifier = &multiagent.VerifierAgent{}
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
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(llmcore.WithRuntime(c.Request.Context(), llmRuntime))
		c.Next()
	})
	apiHandler := api.RegisterRoutes(r, st, eng, mc)

	// ── P0: Distributed Approval Bus (Redis Pub/Sub) ─────────────────────────
	// Wire the ApprovalBus when a Redis DSN is available. The bus allows
	// approve/reject/cancel API calls that land on a peer instance (one that
	// does NOT own the suspended goroutine) to be broadcast via Redis so the
	// executing instance can pick them up.
	busDSN := cfg.Store.DSN
	if cfg.Store.Type != "redis" {
		// Check for a dedicated bus DSN env var so non-Redis store users can
		// still enable distributed signalling.
		busDSN = os.Getenv("AI_AGENT_REDIS_BUS_URL")
	}
	if busDSN != "" {
		busOpts, parseErr := redis.ParseURL(busDSN)
		if parseErr != nil {
			slog.Warn("approval bus: failed to parse Redis URL, bus disabled", "error", parseErr)
		} else {
			busClient := redis.NewClient(busOpts)
			approvalBus := orchestrator.NewApprovalBus(busClient)
			approvalBus.Start(context.Background())
			eng.ApprovalBus = approvalBus
			apiHandler.SetApprovalBus(approvalBus)
			slog.Info("approval bus started", "redis_url", busDSN)

			// Listen for remote cancel signals and fire local context cancels.
			go func() {
				cancelSigs := approvalBus.SubscribeCancelSignals(context.Background())
				for taskID := range cancelSigs {
					slog.Info("approval bus: received remote cancel signal", "task_id", taskID)
					// Attempt local cancel; no-op if the task isn't running here.
					apiHandler.CancelTaskByID(taskID)
				}
			}()
		}
	}

	// ── P1: Startup scan for paused tasks ────────────────────────────────────
	// Log any tasks left in StatusPaused from a previous graceful shutdown so
	// operators know which tasks are awaiting a manual or automatic /run-all.
	go func() {
		scanCtx, scanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scanCancel()
		pausedTasks, err := st.ListTasks(scanCtx, store.ListFilter{Status: types.StatusPaused, Limit: 500})
		if err != nil {
			slog.Warn("startup scan: failed to list paused tasks", "error", err)
			return
		}
		if len(pausedTasks) > 0 {
			slog.Warn("startup: found paused tasks from previous shutdown",
				"count", len(pausedTasks),
				"hint", "POST /api/tasks/:id/run-all to resume",
			)
			for _, t := range pausedTasks {
				slog.Info("paused task", "task_id", t.ID, "goal", t.Goal, "steps", t.StepCount)
			}
		}
	}()

	eng.EventCallback = func(taskID string, status types.TaskStatus) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID,
			Status: status,
		})
	}
	eng.ApprovalCallback = func(taskID string, approval *types.ApprovalRequest) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID:   taskID,
			Status:   types.StatusAwaitingApproval,
			Approval: approval,
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

	addr := cfg.API.Addr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	// Wrap Gin in a standard http.Server so we can gracefully shut it down.
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	// Start HTTP server in the background.
	go func() {
		slog.Info("HTTP server listening", "addr", addr)
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
