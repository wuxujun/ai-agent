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
	h.bootstrapPrompts(c, false)
}

// initTenantPrompts never accepts a target tenant from request data. Even
// administrators may only initialize the authenticated tenant's namespace.
func (h *Handler) initTenantPrompts(c *gin.Context) {
	h.bootstrapPrompts(c, true)
}

func (h *Handler) bootstrapPrompts(c *gin.Context, tenantScoped bool) {
	tenantID := ""
	var tenant config.APITenantConfig
	if tenantScoped {
		tenantID = principalFromGin(c).TenantID
		if strings.TrimSpace(tenantID) == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "tenant prompt initialization requires an authenticated tenant"})
			return
		}
		// AuthMiddleware already enforces require_known_tenant. Dynamic tenants
		// use the same default Team access policy as the other tenant APIs.
		tenant = config.Get().API.Tenants[tenantID]
	}
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
		message := err.Error()
		if tenantScoped {
			message = "tenant prompt configuration is unavailable"
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": "error",
			"error":  message,
		})
		return
	}

	var summary multiagent.PromptBootstrapSummary
	if tenantScoped {
		filtered := &multiagent.TeamsConfig{Teams: make(map[string]multiagent.TeamConfig)}
		for name, team := range teamsCfg.Teams {
			if tenantAllowsMultiAgentTeam(tenant, name) {
				filtered.Teams[name] = team
			}
		}
		summary, err = multiagent.BootstrapTenantTeamPrompts(ctx, tenantID, filtered)
	} else {
		summary, err = multiagent.BootstrapTeamPrompts(ctx, teamsCfg)
	}
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
			"tenant_id", tenantID,
			"existing", summary.Existing,
			"created", summary.Created,
			"error", err,
		)
		message := err.Error()
		if tenantScoped {
			message = "tenant prompt initialization failed"
		}
		c.JSON(status, gin.H{
			"status":    "error",
			"existing":  summary.Existing,
			"created":   summary.Created,
			"processed": summary.Existing + summary.Created,
			"error":     message,
		})
		return
	}

	log.Info("Langfuse prompts dynamically initialized",
		"tenant_id", tenantID,
		"existing", summary.Existing,
		"created", summary.Created,
	)
	response := gin.H{
		"status":    "ok",
		"existing":  summary.Existing,
		"created":   summary.Created,
		"processed": summary.Existing + summary.Created,
	}
	if tenantScoped {
		response["tenant_id"] = tenantID
	}
	c.JSON(http.StatusOK, response)
}
