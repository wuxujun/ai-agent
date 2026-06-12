package api

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

var log = logger.Component("api")

type Handler struct {
	store   store.Store
	engine  *orchestrator.Engine
	metrics *metrics.Collector
	wg      sync.WaitGroup // tracks background run-all goroutines for graceful shutdown
	taskSem chan struct{}  // bounded worker pool for concurrency control

	// activeTasks maps task IDs to the run-all reservation that owns the slot.
	// Storing a pointer (not the raw CancelFunc) gives us identity equality so
	// a goroutine's deferred cleanup only removes its OWN entry — preventing
	// a stale defer from erasing the entry that a subsequent runAll installed.
	activeTasks   map[string]*activeRun
	activeTasksMu sync.Mutex
}

// activeRun is a uniquely allocated reservation token stored in
// Handler.activeTasks. The token's pointer identity is what callers compare
// against; the cancel function is the bgCtx cancel that cancelTask should fire.
type activeRun struct {
	cancel context.CancelFunc
}

type CreateTaskRequest struct {
	ID         string `json:"id"`
	Goal       string `json:"goal"`
	Workspace  string `json:"workspace"`
	MaxSteps   int    `json:"max_steps"`
	ToolBudget int    `json:"tool_budget"`
	// TokenBudget caps cumulative planner+executor token usage across the task.
	// 0 (default) disables the limit; positive values stop the task once the
	// summed TokenUsage across trace entries reaches the budget.
	TokenBudget int `json:"token_budget"`
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
		taskSem:     make(chan struct{}, maxTasks),
		activeTasks: make(map[string]*activeRun),
	}

	r.Use(ErrorMiddleware())
	r.Use(SpanAttributesMiddleware())

	api := r.Group("/api")
	tasks := api.Group("/tasks")
	{
		tasks.POST("", h.createTask)
		tasks.POST("/:id/run", h.runTaskStep)
		tasks.POST("/:id/run-all", h.runAll)
		tasks.GET("/:id", h.getTask)
		tasks.GET("", h.listTasks)
		tasks.GET("/:id/stream", h.streamTask)
		tasks.POST("/:id/approve", h.approveTask)
		tasks.POST("/:id/reject", h.rejectTask)
		tasks.DELETE("/:id/cancel", h.cancelTask)
	}
	api.GET("/metrics", h.getMetrics)
	api.POST("/config/reload", h.reloadConfig)

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})

	return h
}

// Wait blocks until all background run-all goroutines complete. Call during shutdown.
func (h *Handler) Wait() {
	h.wg.Wait()
}

// Shutdown cancels active background tasks and waits for them to exit or for ctx to expire.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.activeTasksMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(h.activeTasks))
	for _, run := range h.activeTasks {
		cancels = append(cancels, run.cancel)
	}
	h.activeTasksMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		h.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	if req.ToolBudget <= 0 {
		req.ToolBudget = 5
	}

	if err := policy.ValidateWorkspace(req.Workspace); err != nil {
		c.Error(err)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	// P8: Prevent silent overwrite of an existing task.
	if exists, err := h.store.ExistsTask(ctx, req.ID); err != nil {
		c.Error(err)
		return
	} else if exists {
		c.JSON(http.StatusConflict, gin.H{"error": "task already exists", "task_id": req.ID})
		return
	}

	task := &types.Task{
		ID:          req.ID,
		Goal:        req.Goal,
		Workspace:   req.Workspace,
		MaxSteps:    req.MaxSteps,
		ToolBudget:  req.ToolBudget,
		TokenBudget: req.TokenBudget,
		Status:      types.StatusCreated,
	}

	if err := h.store.SaveFullTask(ctx, task); err != nil {
		c.Error(err)
		return
	}

	c.JSON(http.StatusCreated, task)
}

func (h *Handler) runTaskStep(c *gin.Context) {
	// 60s allows for LLM planner calls (P99 ≈ 30s) plus tool execution and DB write.
	// A 5s timeout was too short and would cancel in-flight LLM requests.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	// Acquire concurrency slot
	select {
	case h.taskSem <- struct{}{}:
		defer func() { <-h.taskSem }()
	case <-ctx.Done():
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "too many concurrent tasks, please try again later"})
		return
	}

	task, err := h.store.GetTask(ctx, c.Param("id"))
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}

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

	if task.Status == types.StatusCompleted || task.Status == types.StatusFailed {
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
	bgCtx, bgCancel := context.WithTimeout(context.Background(), timeout)
	run := &activeRun{cancel: bgCancel}

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

	// Perform atomic DB state transition to guard against multi-instance races.
	// The activeTasks reservation already serializes in-process callers; this
	// check protects against a peer process holding its own reservation.
	success, err := h.store.TryTransitionTaskStatus(loadCtx, task.ID, []types.TaskStatus{types.StatusCreated, types.StatusAwaitingApproval}, types.StatusRunning)
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
			bgCancel()
		}()

		// Wait for a concurrency slot, but honor cancellation while queued.
		select {
		case h.taskSem <- struct{}{}:
			defer func() { <-h.taskSem }()
		case <-bgCtx.Done():
			_ = orchestrator.SetTaskFailed(task, "task canceled: "+bgCtx.Err().Error())
			saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer saveCancel()
			if saveErr := h.store.SaveFullTask(saveCtx, task); saveErr != nil {
				log.Error("failed to save canceled queued task", "task_id", task.ID, "error", saveErr)
			}
			return
		}

		log.Info("starting async run-all for task", "task_id", task.ID)
		execErr := h.engine.RunAll(bgCtx, task)
		saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer saveCancel()
		if saveErr := h.store.SaveFullTask(saveCtx, task); saveErr != nil {
			log.Error("failed to save task after run-all", "task_id", task.ID, "error", saveErr)
		}
		if execErr != nil {
			log.Error("run-all failed for task", "task_id", task.ID, "error", execErr)
		}
		// Publish terminal event to SSE subscribers
		finalEvent := StepEvent{
			TaskID: task.ID,
			Status: task.Status,
			Final:  task.FinalAnswer,
		}
		GetBus().Publish(task.ID, finalEvent)
	}()

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

func (h *Handler) getMetrics(c *gin.Context) {
	if h.metrics == nil {
		c.JSON(http.StatusOK, gin.H{"message": "metrics disabled"})
		return
	}
	c.JSON(http.StatusOK, h.metrics.Snapshot())
}

// approvalAction is the optional JSON body for /approve and /reject. When
// approval_id is empty, the unique pending approval for the task is resolved;
// when ambiguous (>1 pending) the handler returns 409 with the pending IDs.
type approvalAction struct {
	ApprovalID string `json:"approval_id"`
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

	if body.ApprovalID != "" {
		approval, ok := orchestrator.GetApprovalByID(body.ApprovalID)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval matches approval_id"})
			return
		}
		if !orchestrator.ResolveApprovalByID(body.ApprovalID, approved) {
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
		c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this task"})
		return
	case 1:
		approval := pending[0]
		if !orchestrator.ResolveApproval(taskID, approved) {
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
		c.JSON(http.StatusConflict, gin.H{
			"error":          "multiple pending approvals; specify approval_id",
			"pending_count":  len(pending),
			"approval_ids":   ids,
			"pending":        pending,
		})
		return
	}
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
		"status":     "ok",
		"no_changes": len(changes) == 0,
		"changes":    changes,
		// Return a few non-sensitive resolved values so the caller can confirm
		// which provider and model are now active.
		"active_provider": cfg.ResolveLLMProvider(),
		"active_model":    cfg.ResolveLLMModel(cfg.ResolveLLMProvider()),
	}
	c.JSON(http.StatusOK, resp)
}

// cancelTask handles DELETE /api/tasks/:id/cancel.
// It cancels a running task if it is active in the current process, or marks it as failed in the DB.
func (h *Handler) cancelTask(c *gin.Context) {
	taskID := c.Param("id")

	h.activeTasksMu.Lock()
	run, exists := h.activeTasks[taskID]
	if exists {
		delete(h.activeTasks, taskID)
	}
	h.activeTasksMu.Unlock()

	if !exists {
		// If not in activeTasks, check if the task exists in the db and is running.
		// If so, mark it failed to cancel it.
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
		return
	}

	// Trigger the context cancellation
	run.cancel()

	c.JSON(http.StatusOK, gin.H{
		"message": "task cancellation signal sent",
		"task_id": taskID,
	})
}
