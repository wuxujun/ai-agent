package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
)

// initPrompts handles POST /api/prompt/init.
//
// Each request reloads teams.yaml so newly configured prompt_name entries can
// be synchronized without restarting the service. Existing Langfuse prompts
// are fetched and cached but never overwritten; only names confirmed missing
// are created from their local system_prompt fallback.
func (h *Handler) initPrompts(c *gin.Context) {
	langfuse := config.Get().Langfuse
	if !langfuse.Enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "error",
			"error":  "Langfuse prompt management is disabled",
		})
		return
	}
	if strings.TrimSpace(langfuse.PublicKey) == "" || strings.TrimSpace(langfuse.SecretKey) == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "error",
			"error":  "Langfuse credentials are not configured",
		})
		return
	}

	timeoutSeconds := langfuse.BootstrapTimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 15
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	teamsCfg, err := multiagent.LoadTeamsConfigStrict()
	if err != nil {
		log.Error("dynamic Langfuse prompt initialization failed to load teams config", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	summary, err := multiagent.BootstrapTeamPrompts(ctx, teamsCfg)
	if err != nil {
		status := http.StatusInternalServerError
		var upstreamErr *promptmanager.HTTPStatusError
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
			status = http.StatusGatewayTimeout
		case errors.As(err, &upstreamErr):
			status = http.StatusBadGateway
		}
		log.Error("dynamic Langfuse prompt initialization failed",
			"existing", summary.Existing,
			"created", summary.Created,
			"error", err,
		)
		c.JSON(status, gin.H{
			"status":    "error",
			"existing":  summary.Existing,
			"created":   summary.Created,
			"processed": summary.Existing + summary.Created,
			"error":     err.Error(),
		})
		return
	}

	log.Info("Langfuse prompts dynamically initialized",
		"existing", summary.Existing,
		"created", summary.Created,
	)
	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"existing":  summary.Existing,
		"created":   summary.Created,
		"processed": summary.Existing + summary.Created,
	})
}
