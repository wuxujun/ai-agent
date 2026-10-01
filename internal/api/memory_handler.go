package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/store"
)

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
