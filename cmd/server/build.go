package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/wuxujun/ai-agent/internal/api"
	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type builtApp struct {
	server *http.Server
	tasks  *api.Handler
	bus    *approvalBusRuntime
}

func buildStore(cfg *config.Config) (store.Store, error) {
	switch cfg.Store.Type {
	case "memory":
		return store.NewMemoryStore(), nil
	case "postgres":
		if cfg.Store.DSN == "" {
			return nil, errors.New("Store DSN is required when AI_AGENT_STORE_TYPE=postgres")
		}
		st, err := store.NewPostgresStore(cfg.Store.DSN)
		if err != nil {
			return nil, fmt.Errorf("initialize PostgresStore: %w", err)
		}
		return st, nil
	case "redis":
		if cfg.Store.DSN == "" {
			return nil, errors.New("Store DSN (Redis URL) is required when AI_AGENT_STORE_TYPE=redis")
		}
		st, err := store.NewRedisStoreFromURL(cfg.Store.DSN)
		if err != nil {
			return nil, fmt.Errorf("initialize RedisStore: %w", err)
		}
		return st, nil
	case "sqlite":
		fallthrough
	default:
		dsn := cfg.Store.DSN
		if dsn == "" {
			dsn = "data/agent.db"
		}
		st, err := store.NewSQLiteStore(dsn)
		if err != nil {
			return nil, fmt.Errorf("initialize SQLiteStore: %w", err)
		}
		return st, nil
	}
}

func buildApp(cfg *config.Config, st store.Store, eng *orchestrator.Engine, mc *metrics.Collector, llmRuntime *llmcore.Runtime) builtApp {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(otelgin.Middleware("ai-agent"))
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(llmcore.WithRuntime(c.Request.Context(), llmRuntime))
		c.Next()
	})
	apiHandler := api.RegisterRoutes(router, st, eng, mc)
	busRuntime := buildApprovalBus(cfg, eng, apiHandler)
	startPausedTaskScan(st)
	wireEngineEvents(eng)

	addr := cfg.API.Addr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	return builtApp{
		server: &http.Server{Addr: addr, Handler: router},
		tasks:  apiHandler,
		bus:    busRuntime,
	}
}

func resolveApprovalBusDSN(cfg *config.Config, getenv func(string) string) string {
	if cfg.Store.Type == "redis" {
		return cfg.Store.DSN
	}
	return getenv("AI_AGENT_REDIS_BUS_URL")
}

func buildApprovalBus(cfg *config.Config, eng *orchestrator.Engine, apiHandler *api.Handler) *approvalBusRuntime {
	busDSN := resolveApprovalBusDSN(cfg, os.Getenv)
	if busDSN == "" {
		return nil
	}
	busOpts, err := redis.ParseURL(busDSN)
	if err != nil {
		// Parse errors can include URL userinfo. Do not log the error or DSN.
		slog.Warn("approval bus: Redis URL is invalid, bus disabled")
		return nil
	}

	busClient := redis.NewClient(busOpts)
	approvalBus := orchestrator.NewApprovalBus(busClient)
	busCtx, busCancel := context.WithCancel(context.Background())
	runtime := &approvalBusRuntime{cancel: busCancel, bus: approvalBus, client: busClient}
	approvalBus.Start(busCtx)
	eng.ApprovalBus = approvalBus
	apiHandler.SetApprovalBus(approvalBus)
	busLogInfo := newApprovalBusLogInfo(busOpts)
	slog.Info("approval bus started",
		"redis_address", busLogInfo.Address,
		"redis_db", busLogInfo.DB,
		"redis_tls", busLogInfo.TLS,
	)

	runtime.wg.Add(1)
	go func() {
		defer runtime.wg.Done()
		for taskID := range approvalBus.SubscribeCancelSignals(busCtx) {
			slog.Info("approval bus: received remote cancel signal", "task_id", taskID)
			apiHandler.CancelTaskByID(taskID)
		}
	}()
	return runtime
}

func startPausedTaskScan(st store.Store) {
	go func() {
		scanCtx, scanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scanCancel()
		pausedTasks, err := st.ListTasks(scanCtx, store.ListFilter{Status: types.StatusPaused, Limit: 500})
		if err != nil {
			slog.Warn("startup scan: failed to list paused tasks", "error", err)
			return
		}
		if len(pausedTasks) == 0 {
			return
		}
		slog.Warn("startup: found paused tasks from previous shutdown",
			"count", len(pausedTasks),
			"hint", "POST /api/tasks/:id/run-all to resume",
		)
		for _, task := range pausedTasks {
			slog.Info("paused task", "task_id", task.ID, "goal", task.Goal, "steps", task.StepCount)
		}
	}()
}

func wireEngineEvents(eng *orchestrator.Engine) {
	eng.EventCallback = func(taskID string, status types.TaskStatus) {
		api.GetBus().Publish(taskID, api.StepEvent{TaskID: taskID, Status: status})
	}
	eng.ApprovalCallback = func(taskID string, approval *types.ApprovalRequest) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID, Status: types.StatusAwaitingApproval, Approval: approval,
		})
	}
	eng.StepCallback = func(taskID string, status types.TaskStatus, step *types.StepTrace) {
		api.GetBus().Publish(taskID, api.StepEvent{TaskID: taskID, Status: status, Step: step})
	}
	if eng.Coordinator != nil {
		eng.Coordinator.EventCallback = eng.EventCallback
	}
	eng.TokenCallback = func(taskID, chunk string) {
		api.GetBus().Publish(taskID, api.StepEvent{
			TaskID: taskID, Status: types.StatusRunning, Token: chunk,
		})
	}
}
