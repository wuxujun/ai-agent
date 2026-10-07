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
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type approvalView struct {
	ID               string                      `json:"id"`
	TaskID           string                      `json:"task_id"`
	Status           types.DurableApprovalStatus `json:"status"`
	Version          int64                       `json:"version"`
	Action           string                      `json:"action"`
	RiskLevel        types.RiskLevel             `json:"risk_level"`
	Workspace        string                      `json:"workspace"`
	ParameterSummary []string                    `json:"parameter_summary,omitempty"`
	Preview          string                      `json:"preview,omitempty"`
	CreatedAt        time.Time                   `json:"created_at"`
	UpdatedAt        time.Time                   `json:"updated_at"`
	ResolvedAt       *time.Time                  `json:"resolved_at,omitempty"`
	ActorID          string                      `json:"actor_id,omitempty"`
}

func safeApprovalView(record *types.DurableApproval) approvalView {
	view := approvalView{
		ID: record.ID, TaskID: record.TaskID, Status: record.Status, Version: record.Version,
		Action: record.Request.Action, RiskLevel: record.Request.RiskLevel,
		Workspace: truncateTaskReportText(sanitize.Secrets(record.Request.Workspace), 512),
		Preview:   truncateTaskReportText(sanitize.Secrets(record.Request.Preview), 1600),
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	for _, item := range record.Request.ParameterSummary {
		if len(view.ParameterSummary) >= 32 {
			break
		}
		view.ParameterSummary = append(view.ParameterSummary, truncateTaskReportText(sanitize.Secrets(item), 240))
	}
	if !record.ResolvedAt.IsZero() {
		resolved := record.ResolvedAt
		view.ResolvedAt = &resolved
	}
	return view
}

func (h *Handler) approvalViewForRecord(record *types.DurableApproval) approvalView {
	view := safeApprovalView(record)
	if h.engine == nil || h.engine.ApprovalCodec == nil || len(record.ResolutionPayload) == 0 {
		return view
	}
	plain, err := h.engine.ApprovalCodec.Decrypt(record.ResolutionPayload)
	if err != nil {
		return view
	}
	var result struct {
		ActorID string `json:"actor_id"`
	}
	if json.Unmarshal(plain, &result) == nil {
		view.ActorID = truncateTaskReportText(result.ActorID, 128)
	}
	return view
}

type approvalPageCursor struct {
	TenantID  string                      `json:"tenant_id"`
	Status    types.DurableApprovalStatus `json:"status"`
	CreatedAt time.Time                   `json:"created_at"`
	ID        string                      `json:"id"`
}

func parseApprovalStatus(raw string) (types.DurableApprovalStatus, bool) {
	status := types.DurableApprovalStatus(raw)
	switch status {
	case "", types.ApprovalPending, types.ApprovalApproved, types.ApprovalRejected, types.ApprovalExpired, types.ApprovalConsumed:
		return status, true
	default:
		return "", false
	}
}

func (h *Handler) listApprovalRecords(c *gin.Context) {
	lister, ok := h.store.(store.ApprovalListStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "approval listing is not supported by this store"})
		return
	}
	status, valid := parseApprovalStatus(strings.TrimSpace(c.DefaultQuery("status", string(types.ApprovalPending))))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid approval status"})
		return
	}
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 100"})
			return
		}
		limit = parsed
	}
	tenantID := principalFromGin(c).TenantID
	filter := store.ApprovalListFilter{TenantID: tenantID, Status: status, Limit: limit + 1}
	if raw := c.Query("cursor"); raw != "" {
		if len(raw) > 2048 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid approval cursor"})
			return
		}
		encoded, err := base64.RawURLEncoding.DecodeString(raw)
		var cursor approvalPageCursor
		if err != nil || json.Unmarshal(encoded, &cursor) != nil || cursor.TenantID != tenantID || cursor.Status != status || cursor.ID == "" || cursor.CreatedAt.IsZero() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid approval cursor"})
			return
		}
		filter.BeforeCreatedAt, filter.BeforeID = cursor.CreatedAt, cursor.ID
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	records, err := lister.ListApprovals(ctx, filter)
	if err != nil {
		c.Error(err)
		return
	}
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	items := make([]approvalView, 0, len(records))
	for _, record := range records {
		items = append(items, h.approvalViewForRecord(record))
	}
	nextCursor := ""
	if hasMore {
		last := records[len(records)-1]
		encoded, err := json.Marshal(approvalPageCursor{TenantID: tenantID, Status: status, CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			c.Error(err)
			return
		}
		nextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	c.JSON(http.StatusOK, gin.H{"approvals": items, "count": len(items), "has_more": hasMore, "next_cursor": nextCursor})
}

func (h *Handler) getApprovalStats(c *gin.Context) {
	reader, ok := h.store.(store.ApprovalStatsStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "approval statistics are not supported by this store"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	stats, err := reader.GetApprovalStats(ctx, principalFromGin(c).TenantID)
	if err != nil {
		c.Error(err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, stats)
}

func (h *Handler) getApprovalRecord(c *gin.Context) {
	approvalStore, ok := h.store.(store.DurableApprovalStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "approval lookup is not supported by this store"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	record, err := approvalStore.GetApproval(ctx, c.Param("id"), principalFromGin(c).TenantID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "approval not found"})
		return
	}
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, h.approvalViewForRecord(record))
}

func (h *Handler) listTaskApprovalRecords(c *gin.Context) {
	approvalStore, ok := h.store.(store.DurableApprovalStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "approval lookup is not supported by this store"})
		return
	}
	status, valid := parseApprovalStatus(strings.TrimSpace(c.Query("status")))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid approval status"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	taskID := c.Param("id")
	task, err := h.store.GetTask(ctx, taskID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !principalFromGin(c).Admin && task.TenantID != principalFromGin(c).TenantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if err != nil {
		c.Error(err)
		return
	}
	records, err := approvalStore.ListTaskApprovals(ctx, taskID, task.TenantID, status)
	if err != nil {
		c.Error(err)
		return
	}
	items := make([]approvalView, 0, len(records))
	for _, record := range records {
		items = append(items, h.approvalViewForRecord(record))
	}
	c.JSON(http.StatusOK, gin.H{"approvals": items, "count": len(items)})
}
