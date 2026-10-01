package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
)

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
	if err == nil {
		h.taskSem.Resize(cfg.Orchestrator.MaxConcurrentTasks)
	}
	if err != nil {
		log.Error("manual config reload failed", "error", err)
		status := http.StatusInternalServerError
		if errors.Is(err, config.ErrCrossConfigValidation) {
			status = http.StatusUnprocessableEntity
			if h.metrics != nil {
				h.metrics.ObserveMultiAgentTeamConfigEvent(c.Request.Context(), "reload_rejected")
			}
		}
		c.JSON(status, gin.H{
			"error": err.Error(),
		})
		return
	}

	resp := gin.H{
		"status":          "ok",
		"no_changes":      len(changes) == 0,
		"changes":         changes,
		"config_revision": config.Revision(),
		// Return a few non-sensitive resolved values so the caller can confirm
		// which provider and model are now active.
		"active_provider": cfg.ResolveLLMProvider(),
		"active_model":    cfg.ResolveLLMModel(cfg.ResolveLLMProvider()),
		"llm_task_budget_defaults": gin.H{
			"max_calls":              cfg.LLM.MaxCallsPerTask,
			"max_estimated_cost_usd": cfg.LLM.MaxEstimatedCostUSDPerTask,
		},
	}
	activeScenes := make(map[string]gin.H, len(cfg.LLM.Scenes)+1)
	sceneNames := map[string]struct{}{config.LLMSceneTaskPlanner: {}}
	for scene := range cfg.LLM.Scenes {
		sceneNames[scene] = struct{}{}
	}
	for scene := range sceneNames {
		resolved := cfg.ResolveLLMScene(scene)
		activeScenes[scene] = gin.H{"provider": resolved.Provider, "model": resolved.Model, "base_url": resolved.BaseURL, "timeout_seconds": resolved.TimeoutSeconds, "routes": cfg.LLM.Scenes[scene].Routes}
	}
	resp["active_scenes"] = activeScenes
	c.JSON(http.StatusOK, resp)
}
