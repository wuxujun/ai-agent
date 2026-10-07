package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type taskPageCursor struct {
	TenantID  string           `json:"tenant_id"`
	SessionID string           `json:"session_id"`
	Status    types.TaskStatus `json:"status"`
	CreatedAt time.Time        `json:"created_at"`
	ID        string           `json:"id"`
}

type tracePageCursor struct {
	TaskID   string `json:"task_id"`
	Sequence int64  `json:"sequence"`
}

func pageLimit(c *gin.Context, fallback, maximum int) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return fallback, true
	}
	limit, err := strconv.Atoi(raw)
	return limit, err == nil && limit >= 1 && limit <= maximum
}

func (h *Handler) listTaskSummaries(c *gin.Context) {
	reader, ok := h.store.(store.TaskSummaryStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "task summary listing is not supported by this store"})
		return
	}
	limit, valid := pageLimit(c, 20, 100)
	status := types.TaskStatus(strings.TrimSpace(c.Query("status")))
	if !valid || status != "" && !validTaskStatus(status) || c.Query("offset") != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid summary pagination or status"})
		return
	}
	filter := store.TaskSummaryFilter{Status: status, SessionID: c.Query("session_id"), Limit: limit + 1}
	principal := principalFromGin(c)
	if !principal.Admin {
		filter.TenantID = principal.TenantID
	}
	if raw := c.Query("cursor"); raw != "" {
		if len(raw) > 2048 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task cursor"})
			return
		}
		encoded, err := base64.RawURLEncoding.DecodeString(raw)
		var cursor taskPageCursor
		if err != nil || json.Unmarshal(encoded, &cursor) != nil || cursor.TenantID != filter.TenantID ||
			cursor.SessionID != filter.SessionID || cursor.Status != status || cursor.ID == "" || cursor.CreatedAt.IsZero() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task cursor"})
			return
		}
		filter.BeforeCreatedAt, filter.BeforeID = cursor.CreatedAt, cursor.ID
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	items, err := reader.ListTaskSummaries(ctx, filter)
	if err != nil {
		c.Error(err)
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	nextCursor := ""
	if hasMore {
		last := items[len(items)-1]
		encoded, err := json.Marshal(taskPageCursor{
			TenantID: filter.TenantID, SessionID: filter.SessionID, Status: status,
			CreatedAt: last.CreatedAt, ID: last.ID,
		})
		if err != nil {
			c.Error(err)
			return
		}
		nextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	if items == nil {
		items = []store.TaskSummary{}
	}
	c.JSON(http.StatusOK, gin.H{"tasks": items, "count": len(items), "limit": limit,
		"has_more": hasMore, "next_cursor": nextCursor})
}

func (h *Handler) allowedTaskActions(task *types.Task) []string {
	actions := make([]string, 0, 4)
	if h.engine != nil && (task.Status == types.StatusCreated || task.Status == types.StatusPaused || h.engine.CanResumeTask(task)) {
		actions = append(actions, "run_all")
	}
	if task.Status == types.StatusRunning || task.Status == types.StatusAwaitingApproval {
		actions = append(actions, "cancel")
	}
	if h.engine != nil && h.engine.AnswerPipeline != nil && types.IsTerminalTaskStatus(task.Status) && task.FinalAnswer != "" {
		actions = append(actions, "re_audit")
	}
	if _, ok := h.store.(store.TaskDeletionStore); ok && task.Status != types.StatusRunning && task.Status != types.StatusAwaitingApproval {
		actions = append(actions, "delete")
	}
	return actions
}

func (h *Handler) getTaskSummary(c *gin.Context) {
	reader, ok := h.store.(store.TaskSummaryStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "task summary is not supported by this store"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	task, err := reader.GetTaskWithoutTrace(ctx, c.Param("id"))
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if err != nil {
		c.Error(err)
		return
	}
	// Recovery checkpoints live in Trace. Load it only for states that can
	// resume from a terminal Multi-Agent checkpoint.
	if h.engine != nil && task.Status == types.StatusPartial {
		full, err := h.store.GetTask(ctx, task.ID)
		if err != nil {
			c.Error(err)
			return
		}
		task.Trace = full.Trace
	}
	actions := h.allowedTaskActions(task)
	task.Trace = nil
	task.Memories = nil
	c.JSON(http.StatusOK, struct {
		*types.Task
		AllowedActions []string `json:"allowed_actions"`
		OTelTraceURL   string   `json:"otel_trace_url,omitempty"`
	}{task, actions, config.TraceViewURL(config.Get().API.TraceViewURLTemplate, task.ExecutionTraceID)})
}

func (h *Handler) listTaskTrace(c *gin.Context) {
	reader, ok := h.store.(store.TaskTraceStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "task trace pagination is not supported by this store"})
		return
	}
	limit, valid := pageLimit(c, 100, 200)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 200"})
		return
	}
	taskID := c.Param("id")
	var after int64
	if raw := c.Query("cursor"); raw != "" {
		if len(raw) > 2048 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid trace cursor"})
			return
		}
		encoded, err := base64.RawURLEncoding.DecodeString(raw)
		var cursor tracePageCursor
		if err != nil || json.Unmarshal(encoded, &cursor) != nil || cursor.TaskID != taskID || cursor.Sequence < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid trace cursor"})
			return
		}
		after = cursor.Sequence
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if metadata, ok := h.store.(store.TaskSummaryStore); ok {
		if _, err := metadata.GetTaskWithoutTrace(ctx, taskID); errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		} else if err != nil {
			c.Error(err)
			return
		}
	}
	items, err := reader.ListTaskTraces(ctx, taskID, after, limit+1)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if err != nil {
		c.Error(err)
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	nextCursor := ""
	if hasMore {
		encoded, err := json.Marshal(tracePageCursor{TaskID: taskID, Sequence: items[len(items)-1].Sequence})
		if err != nil {
			c.Error(err)
			return
		}
		nextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	if items == nil {
		items = []store.TaskTraceEvent{}
	}
	for i := range items {
		items[i].EventID = taskID + ":" + strconv.FormatInt(items[i].Sequence, 10)
	}
	c.JSON(http.StatusOK, gin.H{"events": items, "count": len(items), "has_more": hasMore, "next_cursor": nextCursor})
}
