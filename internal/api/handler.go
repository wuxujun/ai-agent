package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
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
}

// WikiReadinessChecker probes the configured read-only Wiki dependency.
type WikiReadinessChecker interface {
	Check(context.Context) error
}

// activeRun is a uniquely allocated reservation token stored in
// Handler.activeTasks. The token's pointer identity is what callers compare
// against; the cancel function is the bgCtx cancel that cancelTask should fire.
type activeRun struct {
	cancel context.CancelFunc
	owner  string
}

type CreateTaskRequest struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	Goal       string `json:"goal"`
	Workspace  string `json:"workspace"`
	Mode       string `json:"mode"`
	MaxSteps   int    `json:"max_steps"`
	ToolBudget int    `json:"tool_budget"`
	// TokenBudget caps cumulative planner+executor token usage across the task.
	// 0 (default) disables the limit; positive values stop the task once the
	// summed TokenUsage across trace entries reaches the budget.
	TokenBudget int `json:"token_budget"`
	// LLM budgets override process defaults when positive; zero inherits the
	// configured default, which is unlimited unless explicitly set.
	LLMCallBudget    int     `json:"llm_call_budget"`
	LLMCostBudgetUSD float64 `json:"llm_cost_budget_usd"`
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
		taskSem:     newResizableSemaphore(),
		activeTasks: make(map[string]*activeRun),
	}

	r.Use(AccessLogMiddleware())
	r.Use(RecoveryMiddleware())
	r.Use(ErrorMiddleware())
	r.Use(SpanAttributesMiddleware())

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
	api.GET("/usage", h.getTenantUsage)
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
		readinessMode := config.Get().ResolveLLMReadinessMode()
		wikiCfg := config.Get().Wiki
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
		ready := healthy && (!wikiCfg.Required || wikiHealthy)
		if wikiCfg.Required && !wikiHealthy {
			tools.ObserveWikiReadinessFailure(ctx)
		}
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{
			"ready": ready, "llm_verified": verified, "llm_readiness_mode": readinessMode, "llm_scenes": scenes,
			"wiki": gin.H{"configured": wikiConfigured, "required": wikiCfg.Required, "healthy": wikiHealthy, "error": wikiError},
		})
	})

	return h
}

// SetWikiReadinessChecker wires the optional Wiki dependency probe. It must be
// called during application construction, before the HTTP server starts.
func (h *Handler) SetWikiReadinessChecker(checker WikiReadinessChecker) {
	h.wikiReady = checker
}

// Wait blocks until all background run-all goroutines complete. Call during shutdown.
func (h *Handler) Wait() {
	h.wg.Wait()
}

// SetApprovalBus wires the optional distributed approval/cancel bus. Must be
// called before any tasks are started. Safe to call with a nil bus (no-op).
func (h *Handler) SetApprovalBus(bus *orchestrator.ApprovalBus) {
	h.approvalBus = bus
}

// Shutdown cancels active background tasks and waits for them to exit or for ctx to expire.
func (h *Handler) Shutdown(ctx context.Context) error {
	// Snapshot and cancel all active tasks.
	h.activeTasksMu.Lock()
	type runEntry struct {
		taskID string
		run    *activeRun
	}
	entries := make([]runEntry, 0, len(h.activeTasks))
	for id, run := range h.activeTasks {
		entries = append(entries, runEntry{taskID: id, run: run})
	}
	h.activeTasksMu.Unlock()

	for _, e := range entries {
		e.run.cancel()
	}

	// Wait for goroutines to finish flushing.
	done := make(chan struct{})
	go func() {
		h.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		log.Warn("Shutdown timed out; some tasks may not have flushed cleanly")
	}

	// ── P1: Graceful-shutdown rollback ──────────────────────────────────────
	// Tasks that were forcibly interrupted are currently stored with status
	// "failed" (set by SetTaskFailed inside RunAll). Roll them back to
	// "paused" so the next process restart can resume them via /run-all
	// (which now accepts StatusPaused as a valid start state).
	rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer rollbackCancel()
	for _, e := range entries {
		h.activeTasksMu.Lock()
		_, running := h.activeTasks[e.taskID]
		h.activeTasksMu.Unlock()
		if running && ctx.Err() != nil {
			log.Warn("skipping shutdown rollback for still-running task to avoid last-writer-wins race", "task_id", e.taskID)
			continue
		}

		task, err := h.store.GetTask(rollbackCtx, e.taskID)
		if err != nil {
			log.Error("shutdown rollback: failed to fetch task", "task_id", e.taskID, "error", err)
			continue
		}
		// Only roll back tasks that were running/queued — completed or already
		// failed-for-a-real-reason tasks must not be touched.
		if task.Status != types.StatusFailed && task.Status != types.StatusRunning {
			continue
		}
		if task.Status == types.StatusFailed && task.FinalAnswer != "" &&
			len(task.FinalAnswer) > 20 {
			// Heuristic: a task with a real FinalAnswer failed for a business
			// reason — do not resurrect it. Only tasks that failed due to
			// context cancellation (short/empty FinalAnswer) get paused.
			continue
		}
		success, transitionErr := h.store.TryTransitionTaskStatus(rollbackCtx, e.taskID, []types.TaskStatus{types.StatusRunning, types.StatusFailed}, types.StatusPaused)
		if transitionErr != nil {
			log.Error("shutdown rollback: failed to pause task", "task_id", e.taskID, "error", transitionErr)
		} else if success {
			log.Info("shutdown rollback: task paused for resumption", "task_id", e.taskID)
		} else {
			log.Info("shutdown rollback: task status changed concurrently, skipping pause", "task_id", e.taskID)
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (h *Handler) createTask(c *gin.Context) {
	var req CreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(err)
		return
	}

	if req.MaxSteps <= 0 {
		req.MaxSteps = 5
	}
	req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
	if req.Mode != "" && !orchestrator.IsSupportedMode(orchestrator.Mode(req.Mode)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be one of eino, legacy, adk, step, or multiagent"})
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID != "" && !validRequestID(req.SessionID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	if req.ToolBudget <= 0 {
		req.ToolBudget = 5
	}
	if req.TokenBudget < 0 || req.LLMCallBudget < 0 || req.LLMCostBudgetUSD < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token and LLM budgets must be greater than or equal to zero"})
		return
	}
	if req.LLMCostBudgetUSD > 0 {
		if err := config.Get().ValidateLLMCostBudgetCoverage(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	if err := policy.ValidateWorkspace(req.Workspace); err != nil {
		c.Error(err)
		return
	}
	principal := principalFromGin(c)
	runtimeConfig := config.Get()
	tenant, tenantConfigured := runtimeConfig.API.Tenants[principal.TenantID]
	if root := strings.TrimSpace(tenant.WorkspaceRoot); root != "" {
		if err := policy.ValidateWorkspaceWithinRoot(root, req.Workspace); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
	} else if runtimeConfig.API.Auth.RequireTenantWorkspaceRoot && !principal.Admin {
		message := "authenticated tenant has no workspace_root configured"
		if !tenantConfigured {
			message = "authenticated tenant is not configured with a workspace_root"
		}
		c.JSON(http.StatusForbidden, gin.H{"error": message})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	taskID := req.ID
	if taskID == "" {
		taskID = uuid.NewString()
	} else {
		// P8: Prevent silent overwrite of an existing task.
		if exists, err := h.store.ExistsTask(ctx, taskID); err != nil {
			c.Error(err)
			return
		} else if exists {
			c.JSON(http.StatusConflict, gin.H{"error": "task already exists", "task_id": taskID})
			return
		}
	}

	task := &types.Task{
		ID:               taskID,
		TenantID:         principal.TenantID,
		SessionID:        req.SessionID,
		Goal:             req.Goal,
		Workspace:        req.Workspace,
		Mode:             req.Mode,
		MaxSteps:         req.MaxSteps,
		ToolBudget:       req.ToolBudget,
		TokenBudget:      req.TokenBudget,
		LLMCallBudget:    req.LLMCallBudget,
		LLMCostBudgetUSD: req.LLMCostBudgetUSD,
		Status:           types.StatusCreated,
	}
	if task.SessionID != "" {
		sessions, ok := h.store.(store.SessionStore)
		if !ok {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "session storage is not supported"})
			return
		}
		sequence, err := sessions.NextSessionTaskSequence(ctx, task.SessionID, task.TenantID)
		if errors.Is(err, store.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		if errors.Is(err, store.ErrSessionArchived) {
			c.JSON(http.StatusConflict, gin.H{"error": "session is archived"})
			return
		}
		if err != nil {
			c.Error(err)
			return
		}
		task.SequenceNo = sequence
	}

	if err := h.store.SaveFullTask(ctx, task); err != nil {
		c.Error(err)
		return
	}

	c.Set(taskIDKey, task.ID)
	c.JSON(http.StatusCreated, task)
}

func (h *Handler) runTaskStep(c *gin.Context) {
	// 60s allows for LLM planner calls (P99 ≈ 30s) plus tool execution and DB write.
	// A 5s timeout was too short and would cancel in-flight LLM requests.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	// Acquire concurrency slot dynamically
	limit := config.Get().Orchestrator.MaxConcurrentTasks
	if limit <= 0 {
		limit = 10
	}
	if !h.taskSem.Acquire(ctx, limit) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "too many concurrent tasks, please try again later"})
		return
	}
	defer h.taskSem.Release(limit)

	task, err := h.store.GetTask(ctx, c.Param("id"))
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}

	owner := uuid.NewString()
	run := &activeRun{cancel: cancel, owner: owner}
	h.activeTasksMu.Lock()
	if _, exists := h.activeTasks[task.ID]; exists {
		h.activeTasksMu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "task is already running", "task_id": task.ID})
		return
	}
	h.activeTasks[task.ID] = run
	h.activeTasksMu.Unlock()
	defer func() {
		h.activeTasksMu.Lock()
		if cur, ok := h.activeTasks[task.ID]; ok && cur == run {
			delete(h.activeTasks, task.ID)
		}
		h.activeTasksMu.Unlock()
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer releaseCancel()
		if err := h.store.ReleaseTaskLease(releaseCtx, task.ID, owner); err != nil {
			log.Warn("failed to release task lease", "task_id", task.ID, "error", err)
		}
	}()

	acquired, err := h.store.AcquireTaskLease(ctx, task.ID, owner, 90*time.Second)
	if err != nil {
		c.Error(err)
		return
	}
	if !acquired {
		c.JSON(http.StatusConflict, gin.H{"error": "task is already running on another instance", "task_id": task.ID})
		return
	}

	stream := c.Query("stream") == "true"
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Header("Connection", "keep-alive")

		ch, _ := GetBus().Subscribe(task.ID)
		defer GetBus().Unsubscribe(task.ID, ch)

		errChan := make(chan error, 1)
		go func() {
			execErr := h.engine.Next(ctx, task)
			if saveErr := h.store.SaveFullTask(ctx, task); saveErr != nil {
				errChan <- saveErr
				return
			}
			if types.IsTerminalTaskStatus(task.Status) {
				GetBus().Publish(task.ID, terminalStepEvent(task.ID, task))
			}
			errChan <- execErr
		}()

		clientGone := c.Request.Context().Done()
		for {
			select {
			case <-clientGone:
				return
			case <-errChan:
				// Drain any remaining events
				for {
					select {
					case event, ok := <-ch:
						if !ok {
							return
						}
						writeSSEEvent(c, event)
						c.Writer.Flush()
					default:
						return
					}
				}
			case event, ok := <-ch:
				if !ok {
					return
				}
				writeSSEEvent(c, event)
				c.Writer.Flush()
			}
		}
	} else {
		execErr := h.engine.Next(ctx, task)
		if saveErr := h.store.SaveFullTask(ctx, task); saveErr != nil {
			c.Error(saveErr)
			return
		}
		if execErr != nil {
			c.Error(execErr)
			return
		}

		c.JSON(http.StatusOK, task)
	}
}

func (h *Handler) runAll(c *gin.Context) {
	loadCtx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	task, err := h.store.GetTask(loadCtx, c.Param("id"))
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}

	resumingMultiAgent := h.engine.CanResumeTask(task)
	if types.IsTerminalTaskStatus(task.Status) && !resumingMultiAgent {
		c.JSON(http.StatusOK, task)
		return
	}

	// Reserve the in-process slot BEFORE the DB transition so we never have to
	// roll the DB back on collision. The slot is keyed by pointer identity
	// (activeRun token) so a stale deferred cleanup can't clobber a slot that
	// a subsequent runAll re-installed for the same task. We use a configurable
	// per-task wall-clock budget here; the engine still owns its own
	// step/tool/token budgets independently.
	timeout := time.Duration(config.Get().Orchestrator.RunAllTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	// Detach the asynchronous task from the HTTP request cancellation while
	// retaining request-scoped values such as the shared LLM Runtime and trace
	// context. The task's own wall-clock timeout remains the cancellation owner.
	bgBase := context.WithoutCancel(c.Request.Context())
	bgCtx, bgCancel := orchestrator.WithPausableTimeout(bgBase, timeout)
	owner := uuid.NewString()
	run := &activeRun{cancel: bgCancel, owner: owner}

	h.activeTasksMu.Lock()
	if _, exists := h.activeTasks[task.ID]; exists {
		h.activeTasksMu.Unlock()
		bgCancel()
		c.JSON(http.StatusConflict, gin.H{
			"error":   "task is already running",
			"task_id": task.ID,
		})
		return
	}
	h.activeTasks[task.ID] = run
	h.activeTasksMu.Unlock()

	acquired, err := h.store.AcquireTaskLease(loadCtx, task.ID, owner, timeout+30*time.Second)
	if err != nil || !acquired {
		h.activeTasksMu.Lock()
		if cur, ok := h.activeTasks[task.ID]; ok && cur == run {
			delete(h.activeTasks, task.ID)
		}
		h.activeTasksMu.Unlock()
		bgCancel()
		if err != nil {
			c.Error(err)
			return
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   "task is already running on another instance",
			"task_id": task.ID,
		})
		return
	}

	// Perform atomic DB state transition to guard against multi-instance races.
	// The activeTasks reservation already serializes in-process callers; this
	// check protects against a peer process holding its own reservation.
	// StatusPaused is accepted here to support resuming tasks that were
	// interrupted by a previous graceful shutdown (P1 rollback).
	startableStatuses := []types.TaskStatus{types.StatusCreated, types.StatusRunning, types.StatusAwaitingApproval, types.StatusPaused}
	if resumingMultiAgent {
		startableStatuses = append(startableStatuses, types.StatusPartial)
	}
	success, err := h.store.TryTransitionTaskStatus(loadCtx, task.ID, startableStatuses, types.StatusRunning)
	if err != nil || !success {
		// Reservation cleanup with compare-and-delete so we never erase a slot
		// some other goroutine just installed (cannot happen today because the
		// activeTasks mutex serializes inserts, but kept defensive).
		h.activeTasksMu.Lock()
		if cur, ok := h.activeTasks[task.ID]; ok && cur == run {
			delete(h.activeTasks, task.ID)
		}
		h.activeTasksMu.Unlock()
		bgCancel()
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if releaseErr := h.store.ReleaseTaskLease(releaseCtx, task.ID, owner); releaseErr != nil {
			log.Warn("failed to release task lease after transition rejection", "task_id", task.ID, "error", releaseErr)
		}
		releaseCancel()

		if err != nil {
			c.Error(err)
			return
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   "task status has changed or is already running",
			"task_id": task.ID,
		})
		return
	}

	task.Status = types.StatusRunning

	// Snapshot the fields used in the response BEFORE handing the task pointer
	// to the goroutine. The goroutine may mutate task.Status (via engine /
	// SetTaskFailed) concurrently with gin's JSON marshalling otherwise.
	respID := task.ID
	respStatus := task.Status

	stream := c.Query("stream") == "true"
	var ch chan StepEvent
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Header("Connection", "keep-alive")

		ch, _ = GetBus().Subscribe(task.ID)
		defer GetBus().Unsubscribe(task.ID, ch)
	}

	errChan := make(chan error, 1)

	// Run asynchronously so the HTTP handler returns immediately (202 Accepted).
	// The caller should poll GET /api/tasks/:id to observe completion.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() {
			h.activeTasksMu.Lock()
			if cur, ok := h.activeTasks[task.ID]; ok && cur == run {
				delete(h.activeTasks, task.ID)
			}
			h.activeTasksMu.Unlock()
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := h.store.ReleaseTaskLease(releaseCtx, task.ID, owner); err != nil {
				log.Warn("failed to release task lease", "task_id", task.ID, "error", err)
			}
			releaseCancel()
			bgCancel()
		}()

		// Wait for a concurrency slot, but honor cancellation while queued.
		limit := config.Get().Orchestrator.MaxConcurrentTasks
		if limit <= 0 {
			limit = 10
		}
		if !h.taskSem.Acquire(bgCtx, limit) {
			_ = orchestrator.SetTaskFailed(task, "task canceled: "+bgCtx.Err().Error())
			saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer saveCancel()
			if saveErr := h.store.SaveFullTask(saveCtx, task); saveErr != nil {
				log.Error("failed to save canceled queued task", "task_id", task.ID, "error", saveErr)
			}
			errChan <- bgCtx.Err()
			return
		}
		defer h.taskSem.Release(limit)

		log.Info("starting async run-all for task", "task_id", task.ID)
		execErr := h.engine.RunAll(bgCtx, task)

		taskReportLog.Info("async run-all completed", "task_id", task.ID, "status", task.Status)
		taskReportLog.Info("--- TASK DECOMPOSITION & PLANNING RESULTS ---", "task_id", task.ID, "goal", task.Goal)
		if task.Hypothesis != "" {
			taskReportLog.Info("Thought Strategy / Hypothesis:", "task_id", task.ID, "hypothesis", task.Hypothesis)
		}
		if len(task.Unresolved) > 0 {
			taskReportLog.Info("Unresolved subtasks remaining:", "task_id", task.ID, "unresolved", task.Unresolved)
		}
		taskReportLog.Info("--- STEP BY STEP EXECUTION TRACE ---", "task_id", task.ID, "step_count", len(task.Trace))
		for _, tr := range task.Trace {
			roleStr := ""
			if tr.AgentRole != "" {
				roleStr = fmt.Sprintf(" [%s]", tr.AgentRole)
			}
			taskReportLog.Info("task step",
				"task_id", task.ID,
				"step", tr.Step,
				"agent_role", strings.TrimSpace(roleStr),
				"action", tr.Action,
				"query", tr.Query,
			)
			if tr.Observation != "" {
				obs := truncateTaskReportText(tr.Observation, 300)
				taskReportLog.Info("  Observation:", "task_id", task.ID, "content", obs)
			}
			if tr.Error != "" {
				taskReportLog.Info("  Error:", "task_id", task.ID, "error", tr.Error)
			}
		}
		taskReportLog.Info("----------------------------------------------", "task_id", task.ID)

		if execErr != nil {
			// RunAll persists every successful execution step, including the
			// terminal one. A second unconditional save here used to race with
			// asynchronous memory indexing and could issue duplicate embedding
			// requests. Retain only this compensation save for failure paths,
			// where RunAll may return before persisting the failed status.
			saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if saveErr := h.store.SaveFullTask(saveCtx, task); saveErr != nil {
				log.Error("failed to save task after run-all failure", "task_id", task.ID, "error", saveErr)
			}
			saveCancel()
			log.Error("run-all failed for task", "task_id", task.ID, "error", execErr)
		}
		// Publish terminal event to SSE subscribers
		GetBus().Publish(task.ID, terminalStepEvent(task.ID, task))
		errChan <- execErr
	}()

	if stream {
		clientGone := c.Request.Context().Done()
		for {
			select {
			case <-clientGone:
				log.Info("stream run-all: client disconnected, cancelling task", "task_id", task.ID)
				bgCancel()
				return
			case <-errChan:
				// Drain any remaining events
				for {
					select {
					case event, ok := <-ch:
						if !ok {
							return
						}
						writeSSEEvent(c, event)
						c.Writer.Flush()
					default:
						return
					}
				}
			case event, ok := <-ch:
				if !ok {
					return
				}
				writeSSEEvent(c, event)
				c.Writer.Flush()
				if event.isTerminal() {
					return
				}
			}
		}
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message": "task is running in background",
		"task_id": respID,
		"status":  respStatus,
	})
}

func (h *Handler) getTask(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	task, err := h.store.GetTask(ctx, c.Param("id"))
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}

	c.JSON(http.StatusOK, task)
}

func (h *Handler) deleteTask(c *gin.Context) {
	deleter, ok := h.store.(store.TaskDeletionStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "configured store does not support task deletion"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	taskID := c.Param("id")
	task, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}
	if task.Status == types.StatusRunning || task.Status == types.StatusAwaitingApproval {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "task must be cancelled before deletion",
			"task_id": taskID,
			"status":  task.Status,
		})
		return
	}
	h.activeTasksMu.Lock()
	_, active := h.activeTasks[taskID]
	h.activeTasksMu.Unlock()
	if active {
		c.JSON(http.StatusConflict, gin.H{"error": "task is still active", "task_id": taskID})
		return
	}
	deleted, err := deleter.DeleteTask(ctx, taskID)
	if err != nil {
		c.Error(err)
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	GetBus().Forget(taskID)
	c.JSON(http.StatusOK, gin.H{"message": "task deleted", "task_id": taskID})
}

func (h *Handler) deleteAllTasks(c *gin.Context) {
	deleter, ok := h.store.(store.TaskDeletionStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "configured store does not support task deletion"})
		return
	}
	if c.Query("confirm") != "true" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "confirm=true is required to delete all tasks"})
		return
	}
	h.activeTasksMu.Lock()
	activeCount := len(h.activeTasks)
	h.activeTasksMu.Unlock()
	if activeCount > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":        "all running tasks must be cancelled before clearing tasks",
			"active_tasks": activeCount,
		})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	count, err := deleter.DeleteAllTasks(ctx)
	if err != nil {
		c.Error(err)
		return
	}
	GetBus().ForgetAll()
	c.JSON(http.StatusOK, gin.H{"message": "all tasks deleted", "deleted": count})
}

func (h *Handler) getMetrics(c *gin.Context) {
	if h.metrics == nil {
		c.JSON(http.StatusOK, gin.H{"message": "metrics disabled"})
		return
	}
	c.JSON(http.StatusOK, struct {
		metrics.Snapshot
		Wiki tools.WikiMetricsSnapshot `json:"wiki"`
	}{Snapshot: h.metrics.Snapshot(), Wiki: tools.CurrentWikiMetrics()})
}

func (h *Handler) getTenantUsage(c *gin.Context) {
	ledger, ok := h.store.(types.TenantUsageLedger)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "tenant usage ledger is unavailable"})
		return
	}
	principal := principalFromGin(c)
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	usage, err := ledger.GetTenantLLMUsage(c.Request.Context(), principal.TenantID, periodStart)
	if err != nil {
		c.Error(err)
		return
	}
	tenant := config.Get().API.Tenants[principal.TenantID]
	response := gin.H{
		"tenant_id":          principal.TenantID,
		"period_start":       periodStart.Format(time.RFC3339),
		"period_end":         periodStart.Add(24 * time.Hour).Format(time.RFC3339),
		"llm_calls":          usage.Calls,
		"estimated_cost_usd": usage.EstimatedCostUSD,
		"limits": gin.H{
			"llm_calls":          tenant.DailyLLMCallBudget,
			"estimated_cost_usd": tenant.DailyLLMCostBudgetUSD,
		},
	}
	c.JSON(http.StatusOK, response)
}

// approvalAction is the optional JSON body for /approve and /reject. When
// approval_id is empty, the unique pending approval for the task is resolved;
// when ambiguous (>1 pending) the handler returns 409 with the pending IDs.
type approvalAction struct {
	ApprovalID string         `json:"approval_id"`
	Message    string         `json:"message"`
	Parameters map[string]any `json:"parameters"`
}

func (h *Handler) approveTask(c *gin.Context) {
	h.resolveTaskApproval(c, true)
}

func (h *Handler) rejectTask(c *gin.Context) {
	h.resolveTaskApproval(c, false)
}

func (h *Handler) resolveTaskApproval(c *gin.Context, approved bool) {
	taskID := c.Param("id")

	var body approvalAction
	// Best-effort decode: an empty/malformed body is fine — we'll fall back to
	// the single-pending lookup. ShouldBindBodyWith would be stricter but
	// changes semantics for callers that omit the body.
	_ = c.ShouldBindJSON(&body)

	result := types.ApprovalResult{
		Approved:   approved,
		Message:    body.Message,
		Parameters: body.Parameters,
	}

	if body.ApprovalID != "" {
		var durableApproval *types.ApprovalRequest
		var durableExists, persisted bool
		var persistErr error
		if h.engine != nil {
			durableApproval, durableExists, persisted, persistErr = h.engine.PersistApprovalResolution(c.Request.Context(), taskID, body.ApprovalID, result)
		}
		if persistErr != nil {
			if errors.Is(persistErr, orchestrator.ErrApprovalExpired) {
				c.JSON(http.StatusGone, gin.H{"error": "approval has expired", "approval_id": body.ApprovalID})
				return
			}
			log.Error("durable approval resolution failed", "task_id", taskID, "approval_id", body.ApprovalID, "error", persistErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist approval decision"})
			return
		}
		if durableExists && !persisted {
			h.observeDurableApproval(c.Request.Context(), "conflict")
			c.JSON(http.StatusConflict, gin.H{"error": "approval was already resolved", "approval_id": body.ApprovalID})
			return
		}
		approval, ok := orchestrator.GetApprovalByID(body.ApprovalID)
		if !ok {
			if persisted {
				h.startDurableApprovalRecovery(taskID, body.ApprovalID)
			}
			// ── P0: Not found locally — broadcast via Redis so executing instance picks it up ──
			if h.approvalBus != nil {
				if pubErr := h.approvalBus.PublishApproval(c.Request.Context(), body.ApprovalID, taskID, result); pubErr != nil {
					log.Warn("approval bus publish failed", "task_id", taskID, "error", pubErr)
				}
				c.JSON(http.StatusAccepted, gin.H{"message": "approval signal forwarded to cluster", "approval_id": body.ApprovalID})
				return
			}
			if persisted {
				c.JSON(http.StatusAccepted, h.approvalResponseMessage(approved, durableApproval))
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval matches approval_id"})
			return
		}
		if !orchestrator.ResolveApprovalByID(body.ApprovalID, result) {
			// Lost the race with another resolver between Get and Resolve.
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval matches approval_id"})
			return
		}
		c.JSON(http.StatusOK, h.approvalResponseMessage(approved, approval))
		return
	}

	pending := orchestrator.ListPendingApprovals(taskID)
	switch len(pending) {
	case 0:
		if h.engine != nil {
			durableApproval, approvalID, durableCount, persisted, persistErr := h.engine.PersistUniqueApprovalResolution(c.Request.Context(), taskID, result)
			if persistErr != nil {
				if errors.Is(persistErr, orchestrator.ErrApprovalExpired) {
					c.JSON(http.StatusGone, gin.H{"error": "approval has expired", "approval_id": approvalID})
					return
				}
				log.Error("durable approval resolution failed", "task_id", taskID, "error", persistErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist approval decision"})
				return
			}
			if durableCount > 1 {
				h.observeDurableApproval(c.Request.Context(), "conflict")
				c.JSON(http.StatusConflict, gin.H{"error": "multiple pending approvals; specify approval_id", "pending_count": durableCount})
				return
			}
			if durableCount == 1 && !persisted {
				h.observeDurableApproval(c.Request.Context(), "conflict")
				c.JSON(http.StatusConflict, gin.H{"error": "approval was already resolved", "approval_id": approvalID})
				return
			}
			if persisted {
				h.startDurableApprovalRecovery(taskID, approvalID)
				if h.approvalBus != nil {
					if pubErr := h.approvalBus.PublishApproval(c.Request.Context(), approvalID, taskID, result); pubErr != nil {
						log.Warn("approval bus publish failed", "task_id", taskID, "error", pubErr)
					}
				}
				c.JSON(http.StatusAccepted, h.approvalResponseMessage(approved, durableApproval))
				return
			}
		}
		// ── P0: Not found locally — broadcast via Redis ──
		if h.approvalBus != nil {
			if pubErr := h.approvalBus.PublishApproval(c.Request.Context(), "", taskID, result); pubErr != nil {
				log.Warn("approval bus publish failed", "task_id", taskID, "error", pubErr)
			}
			c.JSON(http.StatusAccepted, gin.H{"message": "approval signal forwarded to cluster", "task_id": taskID})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this task"})
		return
	case 1:
		approval := pending[0]
		if h.engine != nil {
			_, durableExists, persisted, persistErr := h.engine.PersistApprovalResolution(c.Request.Context(), taskID, approval.ID, result)
			if persistErr != nil {
				if errors.Is(persistErr, orchestrator.ErrApprovalExpired) {
					c.JSON(http.StatusGone, gin.H{"error": "approval has expired", "approval_id": approval.ID})
					return
				}
				log.Error("durable approval resolution failed", "task_id", taskID, "approval_id", approval.ID, "error", persistErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist approval decision"})
				return
			}
			if durableExists && !persisted {
				h.observeDurableApproval(c.Request.Context(), "conflict")
				c.JSON(http.StatusConflict, gin.H{"error": "approval was already resolved", "approval_id": approval.ID})
				return
			}
		}
		if !orchestrator.ResolveApproval(taskID, result) {
			// Lost the race — another resolver beat us between List and Resolve.
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this task"})
			return
		}
		c.JSON(http.StatusOK, h.approvalResponseMessage(approved, approval))
		return
	default:
		// Multiple pending — the API contract requires explicit approval_id
		// to disambiguate. Surface the IDs so the caller can pick.
		ids := make([]string, 0, len(pending))
		for _, p := range pending {
			ids = append(ids, p.ID)
		}
		h.observeDurableApproval(c.Request.Context(), "conflict")
		c.JSON(http.StatusConflict, gin.H{
			"error":         "multiple pending approvals; specify approval_id",
			"pending_count": len(pending),
			"approval_ids":  ids,
			"pending":       pending,
		})
		return
	}
}

func (h *Handler) observeDurableApproval(ctx context.Context, event string) {
	if h.metrics != nil {
		h.metrics.ObserveDurableApproval(ctx, event)
	}
}

func (h *Handler) startDurableApprovalRecovery(taskID, approvalID string) {
	if h.engine == nil || taskID == "" || approvalID == "" {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		owner := "approval-recovery-" + uuid.NewString()
		var task *types.Task
		for {
			var err error
			task, err = h.store.GetTask(ctx, taskID)
			if err != nil {
				h.observeDurableApproval(context.Background(), "recovery_failure")
				log.Error("durable approval recovery task lookup failed", "task_id", taskID, "error", err)
				return
			}
			acquired, leaseErr := h.store.AcquireTaskLease(ctx, taskID, owner, 30*time.Second)
			if leaseErr != nil {
				h.observeDurableApproval(context.Background(), "recovery_failure")
				log.Error("durable approval recovery task lease failed", "task_id", taskID, "error", leaseErr)
				return
			}
			if acquired {
				break
			}
			select {
			case <-ctx.Done():
				h.observeDurableApproval(context.Background(), "recovery_failure")
				log.Warn("durable approval recovery timed out waiting for task lease", "task_id", taskID, "approval_id", approvalID)
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
		defer func() { _ = h.store.ReleaseTaskLease(context.Background(), taskID, owner) }()
		durableStore, ok := h.store.(store.DurableApprovalStore)
		if !ok {
			return
		}
		tenantID := task.TenantID
		if tenantID == "" {
			tenantID = "default"
		}
		approval, err := durableStore.GetApproval(ctx, approvalID, tenantID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				h.observeDurableApproval(context.Background(), "recovery_failure")
				log.Error("durable approval recovery lookup failed", "task_id", taskID, "approval_id", approvalID, "error", err)
			}
			return
		}
		var recovered bool
		switch approval.Status {
		case types.ApprovalApproved:
			recovered, err = h.engine.RecoverApprovedApproval(ctx, task, approval, owner)
		case types.ApprovalRejected:
			recovered, err = h.engine.RecoverRejectedApproval(ctx, task, approval, owner)
		default:
			return
		}
		if err != nil {
			h.observeDurableApproval(context.Background(), "recovery_failure")
			log.Error("durable approval recovery failed", "task_id", taskID, "approval_id", approvalID, "error", err)
			return
		}
		if recovered {
			log.Info("durable approval recovered", "task_id", taskID, "approval_id", approvalID, "status", types.StatusPaused)
		}
	}()
}

func (h *Handler) approvalResponseMessage(approved bool, approval *types.ApprovalRequest) gin.H {
	if approved {
		return gin.H{"message": "task action approved", "approval": approval}
	}
	return gin.H{"message": "task action rejected", "approval": approval}
}

// listTasks handles GET /api/tasks — supports pagination and status filtering.
// Query params: status (optional), limit (default 50, max 500), offset (default 0)
func (h *Handler) listTasks(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	f := store.ListFilter{}
	principal := principalFromGin(c)
	if !principal.Admin {
		f.TenantID = principal.TenantID
	}

	if s := c.Query("status"); s != "" {
		f.Status = types.TaskStatus(s)
	}
	if l := c.Query("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			f.Limit = v
		}
	}
	if o := c.Query("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			f.Offset = v
		}
	}
	f.SessionID = c.Query("session_id")

	tasks, err := h.store.ListTasks(ctx, f)
	if err != nil {
		c.Error(err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"tasks":  tasks,
		"count":  len(tasks),
		"limit":  f.Limit,
		"offset": f.Offset,
	})
}

type memoryListItem struct {
	ID                  string    `json:"id"`
	TenantID            string    `json:"tenant_id"`
	SessionID           string    `json:"session_id,omitempty"`
	TaskID              string    `json:"task_id"`
	Goal                string    `json:"goal"`
	FinalAnswer         string    `json:"final_answer"`
	KeyFindings         string    `json:"key_findings"`
	Timestamp           time.Time `json:"timestamp"`
	EmbeddingDimensions int       `json:"embedding_dimensions"`
}

func (h *Handler) listMemories(c *gin.Context) {
	manager, ok := h.store.(store.MemoryManagementStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "configured store does not support memory management"})
		return
	}
	filter := store.ListMemoryFilter{}
	principal := principalFromGin(c)
	if principal.Admin {
		filter.TenantID = c.Query("tenant_id")
	} else {
		filter.TenantID = principal.TenantID
	}
	filter.SessionID = c.Query("session_id")
	if value, err := strconv.Atoi(c.Query("limit")); err == nil && value > 0 {
		filter.Limit = value
	}
	if value, err := strconv.Atoi(c.Query("offset")); err == nil && value >= 0 {
		filter.Offset = value
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	memories, err := manager.ListMemories(ctx, filter)
	if err != nil {
		c.Error(err)
		return
	}
	items := make([]memoryListItem, 0, len(memories))
	for _, mem := range memories {
		items = append(items, memoryListItem{
			ID: mem.ID, TenantID: mem.TenantID, SessionID: mem.SessionID, TaskID: mem.TaskID,
			Goal: mem.Goal, FinalAnswer: mem.FinalAnswer, KeyFindings: mem.KeyFindings,
			Timestamp: mem.Timestamp, EmbeddingDimensions: len(mem.Embedding),
		})
	}
	c.JSON(http.StatusOK, gin.H{"memories": items, "count": len(items), "limit": filter.Limit, "offset": filter.Offset})
}

func (h *Handler) deleteMemory(c *gin.Context) {
	manager, ok := h.store.(store.MemoryManagementStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "configured store does not support memory management"})
		return
	}
	principal := principalFromGin(c)
	tenantID := ""
	if !principal.Admin {
		tenantID = principal.TenantID
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	deleted, err := manager.DeleteMemory(ctx, c.Param("id"), tenantID)
	if err != nil {
		c.Error(err)
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "memory not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "memory deleted", "memory_id": c.Param("id")})
}

func (h *Handler) deleteAllMemories(c *gin.Context) {
	manager, ok := h.store.(store.MemoryManagementStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "configured store does not support memory management"})
		return
	}
	if c.Query("confirm") != "true" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "confirm=true is required to delete memories"})
		return
	}
	tenantID := c.Query("tenant_id")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	count, err := manager.DeleteAllMemories(ctx, tenantID)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "memories deleted", "deleted": count, "tenant_id": tenantID})
}

// reloadConfig handles POST /api/config/reload.
//
// It atomically re-reads the configuration file and environment variables,
// updates the global config singleton, and returns a redacted diff so the
// caller can verify which values changed. API keys are never echoed back —
// they appear as "***" in the response.
//
// Intended for:
//   - API-key rotation without a process restart.
//   - Model/timeout/log-level tuning in production.
func (h *Handler) reloadConfig(c *gin.Context) {
	cfg, changes, err := config.Reload()
	if err != nil {
		log.Error("manual config reload failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	resp := gin.H{
		"status":          "ok",
		"no_changes":      len(changes) == 0,
		"changes":         changes,
		"config_revision": config.Revision(),
		// Return a few non-sensitive resolved values so the caller can confirm
		// which provider and model are now active.
		"active_provider": cfg.ResolveLLMProvider(),
		"active_model":    cfg.ResolveLLMModel(cfg.ResolveLLMProvider()),
		"llm_task_budget_defaults": gin.H{
			"max_calls":              cfg.LLM.MaxCallsPerTask,
			"max_estimated_cost_usd": cfg.LLM.MaxEstimatedCostUSDPerTask,
		},
	}
	activeScenes := make(map[string]gin.H, len(cfg.LLM.Scenes)+1)
	sceneNames := map[string]struct{}{config.LLMSceneTaskPlanner: {}}
	for scene := range cfg.LLM.Scenes {
		sceneNames[scene] = struct{}{}
	}
	for scene := range sceneNames {
		resolved := cfg.ResolveLLMScene(scene)
		activeScenes[scene] = gin.H{"provider": resolved.Provider, "model": resolved.Model, "base_url": resolved.BaseURL, "timeout_seconds": resolved.TimeoutSeconds, "routes": cfg.LLM.Scenes[scene].Routes}
	}
	resp["active_scenes"] = activeScenes
	c.JSON(http.StatusOK, resp)
}

// cancelTask handles DELETE /api/tasks/:id/cancel.
// It cancels a running task if it is active in the current process, or marks it as failed in the DB.
func (h *Handler) cancelTask(c *gin.Context) {
	taskID := c.Param("id")

	if h.CancelTaskByID(taskID) {
		c.JSON(http.StatusOK, gin.H{
			"message": "task cancellation signal sent",
			"task_id": taskID,
		})
		return
	}

	// ── P0: Not in local activeTasks — the task may be running on a peer
	// instance. Broadcast the cancel signal via Redis so the executing
	// instance can pick it up and cancel its context.
	if h.approvalBus != nil {
		if pubErr := h.approvalBus.PublishCancel(c.Request.Context(), taskID); pubErr != nil {
			log.Warn("cancel bus publish failed", "task_id", taskID, "error", pubErr)
		}
		c.JSON(http.StatusAccepted, gin.H{
			"message": "cancel signal forwarded to cluster",
			"task_id": taskID,
		})
		return
	}
	// No bus — fall back to the legacy DB-level cancel.
	ctx, dbCancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer dbCancel()
	task, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}

	if task.Status == types.StatusRunning {
		task.Status = types.StatusFailed
		task.FinalAnswer = "Failed: task canceled via API"
		if saveErr := h.store.SaveFullTask(ctx, task); saveErr != nil {
			c.Error(saveErr)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"message": "task cancelled (marked failed in database)",
			"task_id": taskID,
		})
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{"error": "task is not running", "status": task.Status})
}

// CancelTaskByID fires the context cancellation for a locally-running task.
// Returns true if the task was found and cancelled; false if it is not running
// in this process (caller may then try a remote signal via the ApprovalBus).
func (h *Handler) CancelTaskByID(taskID string) bool {
	h.activeTasksMu.Lock()
	run, exists := h.activeTasks[taskID]
	if exists {
		delete(h.activeTasks, taskID)
	}
	h.activeTasksMu.Unlock()

	if !exists {
		return false
	}
	run.cancel()
	return true
}

type resizableSemaphore struct {
	mu      sync.Mutex
	current int
	waiters []chan struct{}
}

func newResizableSemaphore() *resizableSemaphore {
	return &resizableSemaphore{}
}

func (s *resizableSemaphore) Acquire(ctx context.Context, limit int) bool {
	s.mu.Lock()
	s.wakeWaiters(limit)
	if s.current < limit {
		s.current++
		s.mu.Unlock()
		return true
	}

	ch := make(chan struct{})
	s.waiters = append(s.waiters, ch)
	s.mu.Unlock()

	select {
	case <-ch:
		return true
	case <-ctx.Done():
		s.mu.Lock()
		for i, w := range s.waiters {
			if w == ch {
				s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return false
	}
}

func (s *resizableSemaphore) Release(limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current--
	s.wakeWaiters(limit)
}

func (s *resizableSemaphore) wakeWaiters(limit int) {
	for len(s.waiters) > 0 && s.current < limit {
		ch := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.current++
		close(ch)
	}
}
