package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/brain"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (h *Handler) getMetrics(c *gin.Context) {
	if h.metrics == nil {
		c.JSON(http.StatusOK, gin.H{"message": "metrics disabled"})
		return
	}
	c.JSON(http.StatusOK, struct {
		metrics.Snapshot
		Wiki  tools.WikiMetricsSnapshot `json:"wiki"`
		Brain brain.MetricsSnapshot     `json:"brain"`
	}{Snapshot: h.metrics.Snapshot(), Wiki: tools.CurrentWikiMetrics(), Brain: brain.CurrentMetrics()})
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
