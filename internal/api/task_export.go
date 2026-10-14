package api

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
)

func (h *Handler) exportTask(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	q, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid export parameters"})
		return
	}
	for key, values := range q {
		if (key != "tenant_id" && key != "byte_budget" && key != "scope") || len(values) != 1 {
			c.JSON(400, gin.H{"error": "invalid export parameters"})
			return
		}
	}
	tenant := q.Get("tenant_id")
	_, known := config.Get().API.Tenants[tenant]
	if tenant == "default" && principalFromGin(c).TenantID == "default" {
		known = true
	}
	if tenant == "" || !known {
		c.JSON(404, gin.H{"error": "tenant not found"})
		return
	}
	id := c.Param("id")
	if id == "" || len(id) > 256 {
		c.JSON(400, gin.H{"error": "invalid task id"})
		return
	}
	budget := 1 << 20
	if raw, exists := q["byte_budget"]; exists {
		budget, err = strconv.Atoi(raw[0])
		if err != nil || budget < 4096 || budget > store.MaxTaskExportBytes {
			c.JSON(400, gin.H{"error": "byte_budget must be between 4096 and 8388608"})
			return
		}
	}
	scope := q.Get("scope")
	if scope != "" && scope != "task_trace" && scope != "joint" {
		c.JSON(400, gin.H{"error": "invalid export scope"})
		return
	}
	reader, ok := h.store.(store.TaskExportReader)
	if !ok {
		c.JSON(501, gin.H{"error": "stable task export unsupported"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	var snapshot *store.TaskExportSnapshot
	if scope == "joint" {
		jointReader, ok := h.store.(store.TaskJointExportReader)
		if !ok {
			c.JSON(501, gin.H{"error": "joint task export unsupported"})
			return
		}
		snapshot, err = jointReader.ReadJointTaskExport(ctx, id, tenant, budget)
		if err == nil && (snapshot == nil || snapshot.Dependencies == nil) {
			err = store.ErrTaskExportUnavailable
		}
	} else {
		snapshot, err = reader.ReadTaskExport(ctx, id, tenant, budget)
		if err == nil && snapshot != nil && snapshot.Dependencies != nil {
			err = store.ErrTaskExportUnavailable
		}
	}
	backend := "custom"
	switch h.store.(type) {
	case *store.MemoryStore:
		backend = "memory"
	case *store.SQLiteStore:
		backend = "sqlite"
	case *store.PostgresStore:
		backend = "postgres"
	case *store.RedisStore:
		backend = "redis"
	}
	var data []byte
	if err == nil {
		data, err = store.EncodeTaskExport(ctx, snapshot, backend, id, tenant, budget)
	}
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			c.JSON(404, gin.H{"error": "task not found"})
		case errors.Is(err, store.ErrTaskExportTooLarge):
			c.JSON(413, gin.H{"error": "task export exceeds budget"})
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			c.JSON(504, gin.H{"error": "task export interrupted"})
		default:
			c.JSON(503, gin.H{"error": "stable task export unavailable"})
		}
		return
	}
	c.Header("Content-Disposition", `attachment; filename="task-export.json"`)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(200, "application/json; charset=utf-8", data)
}
