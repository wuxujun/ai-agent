package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
)

// verifyTaskRestore has no access to h.store. Uploaded records only enter a
// private, request-scoped sandbox which is closed before returning a report.
func (h *Handler) verifyTaskRestore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	q, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid restore parameters"})
		return
	}
	for key, values := range q {
		if (key != "tenant_id" && key != "byte_budget") || len(values) != 1 {
			c.JSON(400, gin.H{"error": "invalid restore parameters"})
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
	budget := 1 << 20
	if raw, ok := q["byte_budget"]; ok {
		budget, err = strconv.Atoi(raw[0])
		if err != nil || budget < 4096 || budget > store.MaxTaskExportBytes {
			c.JSON(400, gin.H{"error": "byte_budget must be between 4096 and 8388608"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(budget))
	data, err := io.ReadAll(c.Request.Body)
	if err == nil {
		var report *store.TaskRestoreReport
		report, err = store.VerifyIsolatedTaskRestore(ctx, data, tenant, budget)
		if err == nil {
			c.JSON(200, report)
			return
		}
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge), errors.Is(err, store.ErrTaskExportTooLarge):
		c.JSON(413, gin.H{"error": "restore artifact exceeds budget"})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		c.JSON(504, gin.H{"error": "isolated restore interrupted"})
	case errors.Is(err, store.ErrTaskRestoreInvalid), errors.Is(err, store.ErrTaskExportUnavailable):
		c.JSON(400, gin.H{"error": "invalid joint restore artifact"})
	default:
		c.JSON(503, gin.H{"error": "isolated restore verification unavailable"})
	}
}
