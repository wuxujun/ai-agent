package api

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type Handler struct {
	store   store.Store
	engine  *orchestrator.Engine
	metrics *metrics.Collector
	wg      sync.WaitGroup // tracks background run-all goroutines for graceful shutdown
	taskSem chan struct{}  // bounded worker pool for concurrency control
}

type CreateTaskRequest struct {
	ID         string `json:"id"`
	Goal       string `json:"goal"`
	Workspace  string `json:"workspace"`
	MaxSteps   int    `json:"max_steps"`
	ToolBudget int    `json:"tool_budget"`
}

func RegisterRoutes(r *gin.Engine, st store.Store, eng *orchestrator.Engine, mc *metrics.Collector) {
	cfg := config.Get()
	maxTasks := cfg.Orchestrator.MaxConcurrentTasks
	if maxTasks <= 0 {
		maxTasks = 10
	}
	
	h := &Handler{
		store:   st,
		engine:  eng,
		metrics: mc,
		taskSem: make(chan struct{}, maxTasks),
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
	}
	api.GET("/metrics", h.getMetrics)

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})
}

// Wait blocks until all background run-all goroutines complete. Call during shutdown.
func (h *Handler) Wait() {
	h.wg.Wait()
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
		ID:         req.ID,
		Goal:       req.Goal,
		Workspace:  req.Workspace,
		MaxSteps:   req.MaxSteps,
		ToolBudget: req.ToolBudget,
		Status:     types.StatusCreated,
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

	// Run asynchronously so the HTTP handler returns immediately (202 Accepted).
	// The caller should poll GET /api/tasks/:id to observe completion.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()

		// Wait for a concurrency slot
		h.taskSem <- struct{}{}
		defer func() { <-h.taskSem }()

		bgCtx, bgCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer bgCancel()
		log.Printf("[Handler] Starting async run-all for task %s", task.ID)
		execErr := h.engine.RunAll(bgCtx, task)
		saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer saveCancel()
		if saveErr := h.store.SaveFullTask(saveCtx, task); saveErr != nil {
			log.Printf("[Handler Error] Failed to save task %s after run-all: %v", task.ID, saveErr)
		}
		if execErr != nil {
			log.Printf("[Handler Error] run-all failed for task %s: %v", task.ID, execErr)
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
		"task_id": task.ID,
		"status":  task.Status,
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
