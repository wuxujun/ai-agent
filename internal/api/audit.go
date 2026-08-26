package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type auditListItem struct {
	TaskID   string                   `json:"task_id"`
	TenantID string                   `json:"tenant_id"`
	Status   types.TaskStatus         `json:"task_status"`
	Audit    *types.AnswerAuditReport `json:"answer_audit"`
}

type auditFilter struct {
	tenantID    string
	taskStatus  types.TaskStatus
	stage       string
	stageState  string
	confidence  string
	enforcement string
	publishable *bool
}

func (f auditFilter) matches(task *types.Task) bool {
	if task == nil || task.AnswerAudit == nil {
		return false
	}
	report := task.AnswerAudit
	if f.confidence != "" && report.FinalConfidence != f.confidence {
		return false
	}
	if f.enforcement != "" && report.Enforcement != f.enforcement {
		return false
	}
	if f.publishable != nil && report.Publishable != *f.publishable {
		return false
	}
	if f.stage == "" && f.stageState == "" {
		return true
	}
	for _, stage := range report.Stages {
		if (f.stage == "" || stage.Name == f.stage) && (f.stageState == "" || stage.Status == f.stageState) {
			return true
		}
	}
	return false
}

func (h *Handler) listAudits(c *gin.Context) {
	filter, ok := parseAuditFilter(c)
	if !ok {
		return
	}
	limit, offset := pageParams(c, 50, 200)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	items := make([]auditListItem, 0, limit+1)
	matched := 0
	err := h.scanTasks(ctx, filter.tenantID, filter.taskStatus, func(task *types.Task) bool {
		if !filter.matches(task) {
			return true
		}
		if matched < offset {
			matched++
			return true
		}
		matched++
		items = append(items, auditListItem{TaskID: task.ID, TenantID: task.TenantID, Status: task.Status, Audit: task.AnswerAudit})
		return len(items) <= limit
	})
	if err != nil {
		c.Error(err)
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	c.JSON(http.StatusOK, gin.H{
		"audits": items, "count": len(items), "limit": limit, "offset": offset,
		"has_more": hasMore, "next_offset": offset + len(items),
	})
}

func (h *Handler) getAuditSummary(c *gin.Context) {
	filter, ok := parseAuditFilter(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	eligible, audited, publishable := 0, 0, 0
	confidence := map[string]int{}
	enforcement := map[string]int{}
	stageStatuses := map[string]map[string]int{}
	err := h.scanTasks(ctx, filter.tenantID, filter.taskStatus, func(task *types.Task) bool {
		if !types.IsTerminalTaskStatus(task.Status) || strings.TrimSpace(task.FinalAnswer) == "" {
			return true
		}
		eligible++
		if task.AnswerAudit == nil {
			return true
		}
		if !filter.matches(task) {
			return true
		}
		audited++
		if task.AnswerAudit.Publishable {
			publishable++
		}
		if task.AnswerAudit.FinalConfidence != "" {
			confidence[task.AnswerAudit.FinalConfidence]++
		}
		enforcement[task.AnswerAudit.Enforcement]++
		for _, stage := range task.AnswerAudit.Stages {
			if stageStatuses[stage.Name] == nil {
				stageStatuses[stage.Name] = map[string]int{}
			}
			stageStatuses[stage.Name][stage.Status]++
		}
		return true
	})
	if err != nil {
		c.Error(err)
		return
	}
	coverage := 0.0
	if eligible > 0 {
		coverage = float64(audited) / float64(eligible)
	}
	c.JSON(http.StatusOK, gin.H{
		"tenant_id": filter.tenantID, "eligible_tasks": eligible, "audited_tasks": audited,
		"coverage_rate": coverage, "publishable_tasks": publishable,
		"confidence_counts": confidence, "enforcement_counts": enforcement,
		"stage_status_counts": stageStatuses,
	})
}

func parseAuditFilter(c *gin.Context) (auditFilter, bool) {
	principal := principalFromGin(c)
	filter := auditFilter{
		taskStatus: types.TaskStatus(strings.TrimSpace(c.Query("task_status"))),
		stage:      strings.TrimSpace(c.Query("stage")), stageState: strings.TrimSpace(c.Query("stage_status")),
		confidence: strings.TrimSpace(c.Query("confidence")), enforcement: strings.TrimSpace(c.Query("enforcement")),
	}
	if principal.Admin {
		filter.tenantID = strings.TrimSpace(c.Query("tenant_id"))
	} else {
		filter.tenantID = principal.TenantID
	}
	if raw := strings.TrimSpace(c.Query("publishable")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "publishable must be true or false"})
			return auditFilter{}, false
		}
		filter.publishable = &value
	}
	if filter.confidence != "" && filter.confidence != "high" && filter.confidence != "medium" && filter.confidence != "low" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "confidence must be high, medium, or low"})
		return auditFilter{}, false
	}
	if filter.enforcement != "" && filter.enforcement != "observe" && filter.enforcement != "advisory" && filter.enforcement != "strict" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enforcement must be observe, advisory, or strict"})
		return auditFilter{}, false
	}
	if filter.taskStatus != "" && !validTaskStatus(filter.taskStatus) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task_status"})
		return auditFilter{}, false
	}
	if filter.stage != "" && !validAuditStage(filter.stage) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid stage"})
		return auditFilter{}, false
	}
	if filter.stageState != "" && !validAuditStageStatus(filter.stageState) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid stage_status"})
		return auditFilter{}, false
	}
	return filter, true
}

func validTaskStatus(status types.TaskStatus) bool {
	switch status {
	case types.StatusCreated, types.StatusRunning, types.StatusAwaitingApproval, types.StatusPaused, types.StatusCompleted, types.StatusPartial, types.StatusFailed:
		return true
	default:
		return false
	}
}

func validAuditStage(stage string) bool {
	switch stage {
	case "answer_verify", "citation_verify", "wiki_citation_integrity", "fact_freshness_check", "numeric_consistency_check", "answer_uncertainty_calibrate", "safety_guard_output":
		return true
	default:
		return false
	}
}

func validAuditStageStatus(status string) bool {
	switch status {
	case "passed", "warned", "not_applicable", "disabled", "budget_insufficient", "dependency_failed", "failed":
		return true
	default:
		return false
	}
}

func pageParams(c *gin.Context, defaultLimit, maxLimit int) (int, int) {
	limit, offset := defaultLimit, 0
	if value, err := strconv.Atoi(c.Query("limit")); err == nil && value > 0 {
		limit = value
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if value, err := strconv.Atoi(c.Query("offset")); err == nil && value >= 0 {
		offset = value
	}
	return limit, offset
}

func (h *Handler) scanTasks(ctx context.Context, tenantID string, status types.TaskStatus, visit func(*types.Task) bool) error {
	const batchSize = 500
	for offset := 0; ; offset += batchSize {
		batch, err := h.store.ListTasks(ctx, store.ListFilter{TenantID: tenantID, Status: status, Limit: batchSize, Offset: offset})
		if err != nil {
			return err
		}
		for _, task := range batch {
			if !visit(task) {
				return nil
			}
		}
		if len(batch) < batchSize {
			return nil
		}
	}
}

type reauditRequest struct {
	Force bool `json:"force"`
}

func (h *Handler) reauditTask(c *gin.Context) {
	if h.engine == nil || h.engine.AnswerPipeline == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "answer pipeline is unavailable"})
		return
	}
	var request reauditRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil && err != io.EOF {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid re-audit request"})
			return
		}
	}
	timeout := config.Get().AnswerPipeline.StageTimeoutSeconds*6 + 10
	if timeout < 60 {
		timeout = 60
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeout)*time.Second)
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
	if !types.IsTerminalTaskStatus(task.Status) {
		c.JSON(http.StatusConflict, gin.H{"error": "only terminal tasks can be re-audited"})
		return
	}
	if strings.TrimSpace(task.FinalAnswer) == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "task has no answer to audit"})
		return
	}
	limit := config.Get().Orchestrator.MaxConcurrentTasks
	if limit <= 0 {
		limit = 10
	}
	if !h.taskSem.Acquire(ctx, limit) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "too many concurrent tasks, please try again later"})
		return
	}
	defer h.taskSem.Release(limit)
	owner := "reaudit-" + uuid.NewString()
	acquired, err := h.store.AcquireTaskLease(ctx, task.ID, owner, time.Duration(timeout+30)*time.Second)
	if err != nil {
		c.Error(err)
		return
	}
	if !acquired {
		c.JSON(http.StatusConflict, gin.H{"error": "task is active on another instance", "task_id": task.ID})
		return
	}
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer releaseCancel()
		if releaseErr := h.store.ReleaseTaskLease(releaseCtx, task.ID, owner); releaseErr != nil {
			log.Warn("failed to release re-audit lease", "task_id", task.ID, "error", releaseErr)
		}
	}()
	report, err := h.engine.Reaudit(ctx, task, request.Force)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.SaveFullTask(ctx, task); err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task_id": task.ID, "status": task.Status, "final_answer": task.FinalAnswer, "answer_audit": report})
}
