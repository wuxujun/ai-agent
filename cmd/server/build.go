package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	expiry *approvalExpiryRuntime
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
	wireEngineEvents(eng)
	startPausedTaskScan(st, eng)
	expiryRuntime := startApprovalExpiryScan(st, eng)

	addr := cfg.API.Addr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	return builtApp{
		server: &http.Server{Addr: addr, Handler: router},
		tasks:  apiHandler,
		bus:    busRuntime,
		expiry: expiryRuntime,
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

func startPausedTaskScan(st store.Store, eng *orchestrator.Engine) {
	go func() {
		scanCtx, scanCancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			for _, task := range pausedTasks {
				slog.Info("paused task", "task_id", task.ID, "goal", task.Goal, "steps", task.StepCount)
			}
		}

		durableStore, ok := st.(store.DurableApprovalStore)
		if !ok || eng == nil || eng.ApprovalCodec == nil {
			return
		}
		awaitingTasks, err := st.ListTasks(scanCtx, store.ListFilter{Status: types.StatusAwaitingApproval, Limit: 500})
		if err != nil {
			slog.Warn("startup scan: failed to list awaiting approval tasks", "error", err)
			return
		}
		failedTasks, failedErr := st.ListTasks(scanCtx, store.ListFilter{Status: types.StatusFailed, Limit: 500})
		if failedErr != nil {
			slog.Warn("startup scan: failed to list legacy cancellation failures", "error", failedErr)
		} else {
			for _, task := range failedTasks {
				if strings.Contains(strings.ToLower(task.FinalAnswer), "context canceled") {
					awaitingTasks = append(awaitingTasks, task)
				}
			}
		}
		owner := "startup-recovery-" + uuid.NewString()
		for _, task := range awaitingTasks {
			tenantID := task.TenantID
			if tenantID == "" {
				tenantID = "default"
			}
			for _, status := range []types.DurableApprovalStatus{types.ApprovalApproved, types.ApprovalRejected} {
				resolved, listErr := durableStore.ListTaskApprovals(scanCtx, task.ID, tenantID, status)
				if listErr != nil {
					slog.Warn("startup scan: failed to list resolved checkpoints", "task_id", task.ID, "status", status, "error", listErr)
					continue
				}
				for _, approval := range resolved {
					var recovered bool
					var recoverErr error
					if status == types.ApprovalApproved {
						recovered, recoverErr = eng.RecoverApprovedApproval(scanCtx, task, approval, owner)
					} else {
						recovered, recoverErr = eng.RecoverRejectedApproval(scanCtx, task, approval, owner)
					}
					if recoverErr != nil {
						if eng.Metrics != nil {
							eng.Metrics.ObserveDurableApproval(scanCtx, "recovery_failure")
						}
						slog.Error("startup approval recovery failed", "task_id", task.ID, "approval_id", approval.ID, "error", recoverErr)
						continue
					}
					if recovered {
						slog.Info("startup approval recovered", "task_id", task.ID, "approval_id", approval.ID,
							"decision", status, "status", types.StatusPaused, "hint", "POST /api/tasks/:id/run-all to continue")
						break
					}
				}
			}
		}
	}()
}

type approvalExpiryRuntime struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (r *approvalExpiryRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.cancel()
	r.wg.Wait()
	return nil
}

func startApprovalExpiryScan(st store.Store, eng *orchestrator.Engine) *approvalExpiryRuntime {
	durableStore, ok := st.(store.DurableApprovalStore)
	if !ok || eng == nil || eng.ApprovalCodec == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &approvalExpiryRuntime{cancel: cancel}
	runtime.wg.Add(1)
	go func() {
		defer runtime.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		lastCleanup := time.Time{}
		for {
			scanCtx, scanCancel := context.WithTimeout(ctx, 30*time.Second)
			expirePendingApprovals(scanCtx, st, durableStore, eng)
			if lastCleanup.IsZero() || time.Since(lastCleanup) >= time.Hour {
				cleanupTerminalApprovals(scanCtx, st, eng)
				lastCleanup = time.Now()
			}
			scanCancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return runtime
}

func cleanupTerminalApprovals(ctx context.Context, st store.Store, eng *orchestrator.Engine) {
	retentionDays := config.Get().Approval.RetentionDays
	if retentionDays <= 0 {
		return
	}
	cleanupStore, ok := st.(store.ApprovalCleanupStore)
	if !ok {
		return
	}
	deleted, err := cleanupStore.DeleteTerminalApprovalsBefore(ctx, time.Now().UTC().AddDate(0, 0, -retentionDays))
	if eng.Metrics != nil {
		eng.Metrics.ObserveApprovalCleanup(ctx, deleted, err)
	}
	if err != nil {
		slog.Warn("approval cleanup failed", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("approval cleanup completed", "deleted", deleted, "retention_days", retentionDays)
	}
}

func expirePendingApprovals(ctx context.Context, st store.Store, durableStore store.DurableApprovalStore, eng *orchestrator.Engine) {
	if config.Get().Approval.TTLSeconds <= 0 {
		return
	}
	var expiredCount int
	for offset := 0; ; offset += 500 {
		tasks, err := st.ListTasks(ctx, store.ListFilter{Status: types.StatusAwaitingApproval, Limit: 500, Offset: offset})
		if err != nil {
			slog.Warn("approval expiry scan: failed to list awaiting tasks", "offset", offset, "error", err)
			return
		}
		for _, task := range tasks {
			tenantID := task.TenantID
			if tenantID == "" {
				tenantID = "default"
			}
			pending, listErr := durableStore.ListTaskApprovals(ctx, task.ID, tenantID, types.ApprovalPending)
			if listErr != nil {
				slog.Warn("approval expiry scan: failed to list pending approvals", "task_id", task.ID, "error", listErr)
				continue
			}
			for _, approval := range pending {
				expired, expireErr := eng.ExpireDurableApproval(ctx, durableStore, approval)
				if expireErr != nil {
					slog.Warn("approval expiry scan: transition failed", "task_id", task.ID, "approval_id", approval.ID, "error", expireErr)
					continue
				}
				if expired {
					expiredCount++
				}
			}
		}
		if len(tasks) < 500 {
			break
		}
	}
	if expiredCount > 0 {
		slog.Info("approval expiry scan completed", "expired", expiredCount)
	}
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
	if eng.Coordinator != nil {
		eng.Coordinator.TokenCallback = eng.TokenCallback
	}
}
