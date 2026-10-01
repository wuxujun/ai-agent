package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/brain"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type CreateTaskRequest struct {
	ID             string `json:"id"`
	SessionID      string `json:"session_id"`
	Goal           string `json:"goal"`
	Workspace      string `json:"workspace"`
	Mode           string `json:"mode"`
	Team           string `json:"team"`
	BrainProjectID string `json:"brain_project_id"`
	MaxSteps       int    `json:"max_steps"`
	ToolBudget     int    `json:"tool_budget"`
	// TokenBudget caps cumulative planner+executor token usage across the task.
	// 0 (default) disables the limit; positive values stop the task once the
	// summed TokenUsage across trace entries reaches the budget.
	TokenBudget int `json:"token_budget"`
	// LLM budgets override process defaults when positive; zero inherits the
	// configured default, which is unlimited unless explicitly set.
	LLMCallBudget    int     `json:"llm_call_budget"`
	LLMCostBudgetUSD float64 `json:"llm_cost_budget_usd"`
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
	req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
	req.Team = strings.TrimSpace(req.Team)
	req.BrainProjectID = strings.TrimSpace(req.BrainProjectID)
	if req.Mode != "" && !orchestrator.IsSupportedMode(orchestrator.Mode(req.Mode)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be one of eino, legacy, adk, step, or multiagent"})
		return
	}
	if req.Team != "" && req.Mode != "" && req.Mode != string(orchestrator.ModeMultiAgent) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "team is only supported for multiagent mode"})
		return
	}
	principal := principalFromGin(c)
	runtimeConfig := config.Get()
	tenant, tenantConfigured := runtimeConfig.API.Tenants[principal.TenantID]
	brainConfigDigest := ""
	if req.BrainProjectID != "" {
		ref, err := brain.ResolveProject(runtimeConfig, principal.TenantID, req.BrainProjectID)
		if err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "brain project is not authorized"})
			return
		}
		brainConfigDigest = brain.ProjectConfigDigest(ref)
	}
	selectedTeam, teamDigest := "", ""
	teamSelectionSource := ""
	if req.Mode == string(orchestrator.ModeMultiAgent) || req.Team != "" {
		teamRequest := req.Team
		teamSelectionSource = "explicit"
		if teamRequest == "" {
			teamSelectionSource = "global_default"
			if tenantDefault := strings.TrimSpace(tenant.DefaultMultiAgentTeam); tenantDefault != "" {
				teamRequest = tenantDefault
				teamSelectionSource = "tenant_default"
			}
		}
		var err error
		selectedTeam, teamDigest, err = multiagent.ResolveTeamSelection(teamRequest)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, multiagent.ErrTeamNotAcceptingNewTasks) {
				status = http.StatusConflict
				outcome := "draining"
				var admissionErr *multiagent.TeamAdmissionError
				if errors.As(err, &admissionErr) {
					outcome = string(admissionErr.Lifecycle)
				}
				if h.metrics != nil {
					h.metrics.ObserveMultiAgentTeamSelection(c.Request.Context(), selectedTeam, outcome, teamSelectionSource)
				}
				log.Warn("Non-active multi-agent Team rejected new task",
					"tenant_id", principal.TenantID,
					"requested_team", req.Team,
					"resolved_team", selectedTeam,
					"team_lifecycle", outcome,
					"selection_source", teamSelectionSource,
				)
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID != "" && !validRequestID(req.SessionID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	if req.ToolBudget <= 0 {
		req.ToolBudget = 5
	}
	if req.TokenBudget < 0 || req.LLMCallBudget < 0 || req.LLMCostBudgetUSD < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token and LLM budgets must be greater than or equal to zero"})
		return
	}
	if req.LLMCostBudgetUSD > 0 {
		if err := config.Get().ValidateLLMCostBudgetCoverage(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	if err := policy.ValidateWorkspace(req.Workspace); err != nil {
		c.Error(err)
		return
	}
	if selectedTeam != "" && !principal.Admin && !tenantAllowsMultiAgentTeam(tenant, selectedTeam) {
		usedDefault := req.Team == ""
		if h.metrics != nil {
			h.metrics.ObserveMultiAgentTeamSelection(c.Request.Context(), selectedTeam, "forbidden", teamSelectionSource)
		}
		log.Warn("Multi-agent team selection rejected",
			"tenant_id", principal.TenantID,
			"requested_team", req.Team,
			"resolved_team", selectedTeam,
			"team_config_digest", teamDigest,
			"used_default", usedDefault,
			"selection_source", teamSelectionSource,
		)
		c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("multi-agent team %q is not allowed for tenant", selectedTeam)})
		return
	}
	if root := strings.TrimSpace(tenant.WorkspaceRoot); root != "" {
		if err := policy.ValidateWorkspaceWithinRoot(root, req.Workspace); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
	} else if runtimeConfig.API.Auth.RequireTenantWorkspaceRoot && !principal.Admin {
		message := "authenticated tenant has no workspace_root configured"
		if !tenantConfigured {
			message = "authenticated tenant is not configured with a workspace_root"
		}
		c.JSON(http.StatusForbidden, gin.H{"error": message})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	taskID := req.ID
	if taskID == "" {
		taskID = uuid.NewString()
	}
	creator, ok := h.store.(store.TaskCreationStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "atomic task creation is not supported by this store"})
		return
	}

	task := &types.Task{
		ID:                  taskID,
		TenantID:            principal.TenantID,
		SessionID:           req.SessionID,
		Goal:                req.Goal,
		Workspace:           req.Workspace,
		Mode:                req.Mode,
		RequestedTeam:       req.Team,
		TeamSelectionSource: teamSelectionSource,
		Team:                selectedTeam,
		TeamConfigDigest:    teamDigest,
		BrainProjectID:      req.BrainProjectID,
		BrainConfigDigest:   brainConfigDigest,
		MaxSteps:            req.MaxSteps,
		ToolBudget:          req.ToolBudget,
		TokenBudget:         req.TokenBudget,
		LLMCallBudget:       req.LLMCallBudget,
		LLMCostBudgetUSD:    req.LLMCostBudgetUSD,
		Status:              types.StatusCreated,
	}
	if task.SessionID != "" {
		sessions, ok := h.store.(store.SessionStore)
		if !ok {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "session storage is not supported"})
			return
		}
		sequence, err := sessions.NextSessionTaskSequence(ctx, task.SessionID, task.TenantID)
		if errors.Is(err, store.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		if errors.Is(err, store.ErrSessionArchived) {
			c.JSON(http.StatusConflict, gin.H{"error": "session is archived"})
			return
		}
		if err != nil {
			c.Error(err)
			return
		}
		task.SequenceNo = sequence
	}

	if err := creator.CreateTask(ctx, task); err != nil {
		if errors.Is(err, store.ErrTaskExists) {
			c.JSON(http.StatusConflict, gin.H{"error": "task already exists", "task_id": taskID})
			return
		}
		c.Error(err)
		return
	}
	if selectedTeam != "" {
		usedDefault := req.Team == ""
		if h.metrics != nil {
			h.metrics.ObserveMultiAgentTeamSelection(c.Request.Context(), selectedTeam, "created", teamSelectionSource)
		}
		log.Info("Multi-agent task team selected",
			"task_id", task.ID,
			"tenant_id", task.TenantID,
			"requested_team", req.Team,
			"resolved_team", selectedTeam,
			"team_config_digest", teamDigest,
			"used_default", usedDefault,
			"selection_source", teamSelectionSource,
		)
	}

	c.Set(taskIDKey, task.ID)
	c.JSON(http.StatusCreated, task)
}

func tenantAllowsMultiAgentTeam(tenant config.APITenantConfig, team string) bool {
	if len(tenant.AllowedMultiAgentTeams) == 0 {
		return true
	}
	for _, allowed := range tenant.AllowedMultiAgentTeams {
		if allowed == team {
			return true
		}
	}
	return false
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

func (h *Handler) deleteTask(c *gin.Context) {
	deleter, ok := h.store.(store.TaskDeletionStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "configured store does not support task deletion"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	taskID := c.Param("id")
	task, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}
	if task.Status == types.StatusRunning || task.Status == types.StatusAwaitingApproval {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "task must be cancelled before deletion",
			"task_id": taskID,
			"status":  task.Status,
		})
		return
	}
	h.activeTasksMu.Lock()
	_, active := h.activeTasks[taskID]
	h.activeTasksMu.Unlock()
	if active {
		c.JSON(http.StatusConflict, gin.H{"error": "task is still active", "task_id": taskID})
		return
	}
	deleted, err := deleter.DeleteTask(ctx, taskID)
	if err != nil {
		c.Error(err)
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	GetBus().Forget(taskID)
	c.JSON(http.StatusOK, gin.H{"message": "task deleted", "task_id": taskID})
}

func (h *Handler) deleteAllTasks(c *gin.Context) {
	deleter, ok := h.store.(store.TaskDeletionStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "configured store does not support task deletion"})
		return
	}
	if c.Query("confirm") != "true" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "confirm=true is required to delete all tasks"})
		return
	}
	h.activeTasksMu.Lock()
	activeCount := len(h.activeTasks)
	h.activeTasksMu.Unlock()
	if activeCount > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":        "all running tasks must be cancelled before clearing tasks",
			"active_tasks": activeCount,
		})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	count, err := deleter.DeleteAllTasks(ctx)
	if err != nil {
		c.Error(err)
		return
	}
	GetBus().ForgetAll()
	c.JSON(http.StatusOK, gin.H{"message": "all tasks deleted", "deleted": count})
}

// listTasks handles GET /api/tasks — supports pagination and status filtering.
// Query params: status (optional), limit (default 50, max 500), offset (default 0)
func (h *Handler) listTasks(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	f := store.ListFilter{}
	principal := principalFromGin(c)
	if !principal.Admin {
		f.TenantID = principal.TenantID
	}

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
	f.SessionID = c.Query("session_id")

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
