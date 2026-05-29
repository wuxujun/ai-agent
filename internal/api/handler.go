package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/pkg/types"
)

type Handler struct {
	store   *store.SQLiteStore
	engine  *orchestrator.Engine
	metrics *metrics.Collector
}

type CreateTaskRequest struct {
	ID         string `json:"id"`
	Goal       string `json:"goal"`
	Workspace  string `json:"workspace"`
	MaxSteps   int    `json:"max_steps"`
	ToolBudget int    `json:"tool_budget"`
}

func RegisterRoutes(r *gin.Engine, st *store.SQLiteStore, eng *orchestrator.Engine, mc *metrics.Collector) {
	h := &Handler{store: st, engine: eng, metrics: mc}

	r.Use(ErrorMiddleware())
	r.Use(SpanAttributesMiddleware())

	api := r.Group("/api")
	tasks := api.Group("/tasks")
	{
		tasks.POST("", h.createTask)
		tasks.POST("/:id/run", h.runTaskStep)
		tasks.POST("/:id/run-all", h.runAll)
		tasks.GET("/:id", h.getTask)
	}
	api.GET("/metrics", h.getMetrics)

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})
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

	task := &types.Task{
		ID:         req.ID,
		Goal:       req.Goal,
		Workspace:  req.Workspace,
		MaxSteps:   req.MaxSteps,
		ToolBudget: req.ToolBudget,
		Status:     "created",
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := h.store.SaveFullTask(ctx, task); err != nil {
		c.Error(err)
		return
	}

	c.JSON(http.StatusOK, task)
}

func (h *Handler) runTaskStep(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
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

	if err := h.engine.Next(ctx, task); err != nil {
		c.Error(err)
		return
	}
	if err := h.store.SaveFullTask(ctx, task); err != nil {
		c.Error(err)
		return
	}

	c.JSON(http.StatusOK, task)
}

func (h *Handler) runAll(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
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

	if err := h.engine.RunAll(ctx, task); err != nil {
		c.Error(err)
		return
	}
	if err := h.store.SaveFullTask(ctx, task); err != nil {
		c.Error(err)
		return
	}

	c.JSON(http.StatusOK, task)
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
