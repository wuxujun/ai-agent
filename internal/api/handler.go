package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

var log = logger.Component("api")

var taskReportLog = logger.ReportComponent("api")

var accessLog = logger.AccessComponent("access")

const taskIDKey = "task_id"

func truncateTaskReportText(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "... (truncated)"
}

type Handler struct {
	store   store.Store
	engine  *orchestrator.Engine
	metrics *metrics.Collector
	wg      sync.WaitGroup      // tracks background run-all goroutines for graceful shutdown
	taskSem *resizableSemaphore // bounded worker pool for concurrency control

	// activeTasks maps task IDs to the run-all reservation that owns the slot.
	// Storing a pointer (not the raw CancelFunc) gives us identity equality so
	// a goroutine's deferred cleanup only removes its OWN entry — preventing
	// a stale defer from erasing the entry that a subsequent runAll installed.
	activeTasks   map[string]*activeRun
	activeTasksMu sync.Mutex

	// approvalBus is the optional Redis-backed distributed approval/cancel bus.
	// When non-nil, approve and cancel API calls that cannot be resolved
	// locally (task running on a peer instance) are broadcast via Redis Pub/Sub
	// so the executing instance can pick them up.
	approvalBus *orchestrator.ApprovalBus
	wikiReady   WikiReadinessChecker
	wikiPages   WikiPageReader
	brainPages  BrainPageReader
	brainStatus interface{ BrainStatus(context.Context) any }
}

// WikiReadinessChecker probes the configured read-only Wiki dependency.
type WikiReadinessChecker interface {
	Check(context.Context) error
}

// WikiPageReader is the read-only page boundary used by the authenticated
// browser API. Implementations retain their normal path and backend checks.
type WikiPageReader interface {
	Read(context.Context, wiki.Document, string) (wiki.Document, error)
}

// BrainPageReader is the pinned, tenant-scoped Brain page boundary. The
// caller supplies an already selected immutable snapshot; implementations
// must never fall back to CURRENT.
type BrainPageReader interface {
	ReadBrain(context.Context, wiki.Document, string, string, string, string) (wiki.Document, error)
}

type wikiStatusProvider interface {
	Status() any
}

// activeRun is a uniquely allocated reservation token stored in
// Handler.activeTasks. The token's pointer identity is what callers compare
// against; the cancel function is the bgCtx cancel that cancelTask should fire.
type activeRun struct {
	cancel context.CancelFunc
	owner  string
	lease  *store.ExecutionLease
}

func RegisterRoutes(r *gin.Engine, st store.Store, eng *orchestrator.Engine, mc *metrics.Collector) *Handler {
	cfg := config.Get()
	maxTasks := cfg.Orchestrator.MaxConcurrentTasks
	if maxTasks <= 0 {
		maxTasks = 10
	}

	h := &Handler{
		store:       st,
		engine:      eng,
		metrics:     mc,
		taskSem:     newResizableSemaphore(maxTasks),
		activeTasks: make(map[string]*activeRun),
	}

	r.Use(AccessLogMiddleware())
	r.Use(RecoveryMiddleware())
	r.Use(ErrorMiddleware())
	r.Use(SpanAttributesMiddleware())
	r.Use(RequestBodyLimitMiddleware())

	api := r.Group("/api")
	api.Use(AuthMiddleware())
	tasks := api.Group("/tasks")
	tasks.Use(TaskTenantMiddleware(st))
	{
		tasks.POST("", h.createTask)
		tasks.DELETE("", AdminMiddleware(), h.deleteAllTasks)
		tasks.POST("/:id/run", h.runTaskStep)
		tasks.POST("/:id/run-all", h.runAll)
		tasks.POST("/:id/re-audit", h.reauditTask)
		tasks.GET("/:id", h.getTask)
		tasks.GET("", h.listTasks)
		tasks.GET("/:id/stream", h.streamTask)
		tasks.POST("/:id/approve", h.approveTask)
		tasks.POST("/:id/reject", h.rejectTask)
		tasks.DELETE("/:id/cancel", h.cancelTask)
		tasks.DELETE("/:id", h.deleteTask)
	}
	audits := api.Group("/audits")
	{
		audits.GET("", h.listAudits)
		audits.GET("/summary", h.getAuditSummary)
	}
	memories := api.Group("/memories")
	{
		memories.GET("", h.listMemories)
		memories.DELETE("", AdminMiddleware(), h.deleteAllMemories)
		memories.DELETE("/:id", h.deleteMemory)
	}
	sessions := api.Group("/sessions")
	{
		sessions.POST("", h.createSession)
		sessions.GET("", h.listSessions)
		sessions.GET("/:id", h.getSession)
		sessions.PATCH("/:id", h.updateSession)
		sessions.POST("/:id/archive", h.archiveSession)
		sessions.GET("/:id/tasks", h.listSessionTasks)
		sessions.GET("/:id/memories", h.listSessionMemories)
	}
	api.GET("/metrics", AdminMiddleware(), h.getMetrics)
	api.GET("/wiki/pages/:space/*slug", h.getWikiPage)
	api.GET("/usage", h.getTenantUsage)
	api.GET("/teams", h.listTeams)
	api.PATCH("/teams/:name/lifecycle", AdminMiddleware(), h.updateTeamLifecycle)
	api.GET("/teams/lifecycle-audits", AdminMiddleware(), h.listTeamLifecycleAudits)
	api.GET("/teams/lifecycle-audits/integrity", AdminMiddleware(), h.getTeamLifecycleAuditIntegrity)
	api.POST("/teams/lifecycle-audits/archive", AdminMiddleware(), h.archiveTeamLifecycleAudits)
	api.GET("/teams/lifecycle-audits/archives", AdminMiddleware(), h.listTeamLifecycleAuditArchives)
	api.POST("/config/reload", AdminMiddleware(), h.reloadConfig)
	api.POST("/prompt/init", AdminMiddleware(), h.initPrompts)

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})
	r.GET("/ready", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		scenes, healthy := llmcore.CheckConfiguredScenes(ctx)
		verified := llmcore.AllScenesVerified(scenes)
		runtimeConfig := config.Get()
		readinessMode := runtimeConfig.ResolveLLMReadinessMode()
		wikiCfg := runtimeConfig.Wiki
		teamHealth := multiagent.CheckTeamRouting(runtimeConfig)
		if !teamHealth.Healthy && h.metrics != nil {
			h.metrics.ObserveMultiAgentTeamConfigEvent(ctx, "readiness_failure")
		}
		wikiConfigured := strings.TrimSpace(wikiCfg.URL) != "" || strings.TrimSpace(wikiCfg.Directory) != ""
		wikiHealthy := !wikiCfg.Required
		wikiError := ""
		if wikiConfigured && h.wikiReady != nil {
			if err := h.wikiReady.Check(ctx); err != nil {
				wikiError = err.Error()
			} else {
				wikiHealthy = true
			}
		} else if wikiCfg.Required {
			wikiError = "required Wiki is not initialized"
		}
		var wikiStatus any
		if provider, ok := h.wikiReady.(wikiStatusProvider); ok {
			wikiStatus = provider.Status()
		}
		brainConfigured := runtimeConfig.Brain.Enabled
		brainHealthy := true
		var brainStatus any = gin.H{"configured": false, "healthy": true, "current_projects": 0}
		if brainConfigured {
			brainStatus = gin.H{"configured": true, "healthy": true, "current_projects": len(runtimeConfig.API.Tenants)}
			if h.brainStatus != nil {
				brainStatus = h.brainStatus.BrainStatus(ctx)
			}
			if status, ok := brainStatus.(map[string]any); ok {
				if healthy, exists := status["healthy"].(bool); exists {
					brainHealthy = healthy
				}
			}
		}
		ready := healthy && (!wikiCfg.Required || wikiHealthy) && teamHealth.Healthy && (!brainConfigured || brainHealthy)
		if wikiCfg.Required && !wikiHealthy {
			tools.ObserveWikiReadinessFailure(ctx)
		}
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{
			"ready": ready, "llm_verified": verified, "llm_readiness_mode": readinessMode, "llm_scenes": scenes,
			"wiki":  gin.H{"configured": wikiConfigured, "required": wikiCfg.Required, "healthy": wikiHealthy, "error": wikiError, "status": wikiStatus},
			"teams": teamHealth, "brain": brainStatus, "brain_healthy": brainHealthy,
		})
	})

	return h
}

// SetWikiReadinessChecker wires the optional Wiki dependency probe. It must be
// called during application construction, before the HTTP server starts.
func (h *Handler) SetWikiReadinessChecker(checker WikiReadinessChecker) {
	h.wikiReady = checker
	if reader, ok := checker.(WikiPageReader); ok {
		h.wikiPages = reader
	}
}

// SetBrainPageReader wires the pinned Brain page boundary. It is optional so
// Brain-disabled and ordinary Wiki deployments retain their existing path.
func (h *Handler) SetBrainPageReader(reader BrainPageReader) {
	h.brainPages = reader
	if provider, ok := reader.(interface{ BrainStatus(context.Context) any }); ok {
		h.brainStatus = provider
	}
}
