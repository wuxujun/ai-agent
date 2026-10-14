package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

const retentionPolicyVersion = "diagnostic-v2"

type retentionCursor struct {
	TenantID  string    `json:"tenant_id"`
	Policy    string    `json:"policy"`
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}
type retentionAssessment struct {
	store.RetentionObservation
	Assessment         string   `json:"assessment"`
	Reasons            []string `json:"reasons"`
	TerminalAgeSource  string   `json:"terminal_age_source"`
	ContentHash        *string  `json:"content_hash"`
	EstimatedTaskBytes *int64   `json:"estimated_task_bytes"`
}
type retentionPage struct {
	ReadOnly       bool                  `json:"read_only"`
	ExecutionReady bool                  `json:"execution_ready"`
	PolicyVersion  string                `json:"policy_version"`
	TenantID       string                `json:"tenant_id"`
	HotDays        int                   `json:"hot_days"`
	AssessedAt     time.Time             `json:"assessed_at"`
	Items          []retentionAssessment `json:"items"`
	Count          int                   `json:"count"`
	Limit          int                   `json:"limit"`
	ByteBudget     int                   `json:"byte_budget"`
	HasMore        bool                  `json:"has_more"`
	NextCursor     string                `json:"next_cursor"`
}

func assessRetention(row store.RetentionObservation, hotDays int, holds []string) retentionAssessment {
	a := retentionAssessment{RetentionObservation: row, Assessment: "unknown", Reasons: []string{}, TerminalAgeSource: "unavailable"}
	blocked := func(reason string) { a.Assessment = "blocked"; a.Reasons = append(a.Reasons, reason) }
	unknown := func(reason string) { a.Reasons = append(a.Reasons, reason) }
	switch row.Status {
	case types.StatusCompleted, types.StatusFailed:
	case types.StatusPartial:
		blocked("partial_may_resume")
	case types.StatusCreated, types.StatusRunning, types.StatusAwaitingApproval, types.StatusPaused:
		blocked("task_not_final")
	default:
		blocked("unrecognized_task_status")
	}
	if slices.Contains(holds, row.ID) {
		blocked("requested_hold")
	}
	if row.LeaseActive == nil {
		unknown("lease_state_unverified")
	} else if *row.LeaseActive {
		blocked("active_execution_lease")
	}
	if row.ApprovalsUnresolved == nil {
		unknown("approval_state_unverified")
	} else if *row.ApprovalsUnresolved {
		blocked("unresolved_approvals")
	}
	if row.SessionID != "" {
		unknown("session_dependency_unverified")
	}
	if row.TraceCount == nil {
		unknown("trace_count_unverified")
	}
	if hotDays == 0 {
		unknown("retention_period_unset")
	}
	now := time.Now().UTC()
	if row.MaintenanceVersion > 0 && row.MaintenanceVersion <= store.MaxMaintenanceVersion && row.TerminalSince != nil && row.LastMaintenanceAt != nil && !row.TerminalSince.After(*row.LastMaintenanceAt) && !row.LastMaintenanceAt.After(now) {
		a.TerminalAgeSource = "store_lifecycle"
		if hotDays > 0 && now.Sub(*row.LastMaintenanceAt) < time.Duration(hotDays)*24*time.Hour {
			blocked("retention_window_not_elapsed")
		}
	} else {
		unknown("unknown_terminal_age")
	}
	unknown("hold_policy_unverified")
	unknown("task_size_unverified")
	unknown("joint_snapshot_unverified")
	return a
}
func (h *Handler) listRetentionAssessments(c *gin.Context) {
	allowed := map[string]bool{"tenant_id": true, "hot_days": true, "limit": true, "byte_budget": true, "cursor": true, "hold_task_id": true}
	q, parseErr := url.ParseQuery(c.Request.URL.RawQuery)
	if parseErr != nil {
		c.JSON(400, gin.H{"error": "invalid retention parameters"})
		return
	}
	for key, values := range q {
		if !allowed[key] || (key != "hold_task_id" && len(values) != 1) {
			c.JSON(400, gin.H{"error": "invalid retention parameters"})
			return
		}
	}
	tenant := q.Get("tenant_id")
	cfg := config.Get()
	_, known := cfg.API.Tenants[tenant]
	if tenant == "default" && principalFromGin(c).TenantID == "default" {
		known = true
	}
	if !known || tenant == "" {
		c.JSON(404, gin.H{"error": "tenant not found"})
		return
	}
	limit, ok := pageLimit(c, 20, 50)
	if !ok {
		c.JSON(400, gin.H{"error": "limit must be between 1 and 50"})
		return
	}
	hotDays := 0
	if raw, exists := q["hot_days"]; exists {
		var err error
		hotDays, err = strconv.Atoi(raw[0])
		if err != nil || hotDays < 1 || hotDays > 3650 {
			c.JSON(400, gin.H{"error": "hot_days must be between 1 and 3650"})
			return
		}
	}
	budget := 65536
	if raw, exists := q["byte_budget"]; exists {
		var err error
		budget, err = strconv.Atoi(raw[0])
		if err != nil || budget < 4096 || budget > 1048576 {
			c.JSON(400, gin.H{"error": "byte_budget must be between 4096 and 1048576"})
			return
		}
	}
	holds := append([]string(nil), q["hold_task_id"]...)
	if len(holds) > 20 {
		c.JSON(400, gin.H{"error": "too many hold IDs"})
		return
	}
	for _, id := range holds {
		if id == "" || len(id) > 256 {
			c.JSON(400, gin.H{"error": "invalid hold ID"})
			return
		}
	}
	slices.Sort(holds)
	holds = slices.Compact(holds)
	policyJSON, _ := json.Marshal(struct {
		Version string
		Tenant  string
		HotDays int
		Holds   []string
	}{retentionPolicyVersion, tenant, hotDays, holds})
	digest := sha256.Sum256(policyJSON)
	policy := hex.EncodeToString(digest[:])
	filter := store.TaskSummaryFilter{TenantID: tenant, Limit: limit + 1}
	if raw, exists := q["cursor"]; exists {
		var cursor retentionCursor
		if len(raw[0]) > 2048 {
			c.JSON(400, gin.H{"error": "invalid retention cursor"})
			return
		}
		decoded, err := base64.RawURLEncoding.DecodeString(raw[0])
		if err != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.TenantID != tenant || cursor.Policy != policy || cursor.ID == "" || cursor.CreatedAt.IsZero() {
			c.JSON(400, gin.H{"error": "invalid retention cursor"})
			return
		}
		filter.BeforeID, filter.BeforeCreatedAt = cursor.ID, cursor.CreatedAt
	}
	reader, ok := h.store.(store.TaskRetentionReader)
	if !ok {
		c.JSON(501, gin.H{"error": "read-only retention assessment unsupported"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	rows, err := reader.ListTaskRetentionObservations(ctx, filter)
	if err != nil {
		c.JSON(503, gin.H{"error": "retention observation unavailable"})
		return
	}
	if len(rows) > filter.Limit {
		c.JSON(503, gin.H{"error": "invalid retention observation"})
		return
	}
	for _, row := range rows {
		if row.TenantID != tenant || row.ID == "" || row.CreatedAt.IsZero() || len(row.ID) > 256 || len(row.SessionID) > 256 {
			c.JSON(503, gin.H{"error": "invalid retention observation"})
			return
		}
	}
	response := retentionPage{ReadOnly: true, ExecutionReady: false, PolicyVersion: retentionPolicyVersion, TenantID: tenant, HotDays: hotDays, AssessedAt: time.Now().UTC(), Items: []retentionAssessment{}, Limit: limit, ByteBudget: budget}
	encode := func() ([]byte, error) {
		response.Count = len(response.Items)
		response.HasMore = response.Count < len(rows)
		response.NextCursor = ""
		if response.HasMore && response.Count > 0 {
			last := response.Items[response.Count-1]
			value, err := json.Marshal(retentionCursor{TenantID: tenant, Policy: policy, ID: last.ID, CreatedAt: last.CreatedAt})
			if err != nil {
				return nil, err
			}
			response.NextCursor = base64.RawURLEncoding.EncodeToString(value)
			if len(response.NextCursor) > 2048 {
				return nil, fmt.Errorf("retention cursor oversized")
			}
		}
		return json.Marshal(response)
	}
	for i, row := range rows {
		if i == limit {
			break
		}

		response.Items = append(response.Items, assessRetention(row, hotDays, holds))
		body, err := encode()
		if err != nil {
			c.JSON(503, gin.H{"error": "retention response unavailable"})
			return
		}
		if len(body) > budget {
			response.Items = response.Items[:len(response.Items)-1]
			break
		}
	}
	if len(rows) > 0 && len(response.Items) == 0 {
		c.JSON(413, gin.H{"error": "retention metadata exceeds response budget"})
		return
	}
	body, err := encode()
	if err != nil || len(body) > budget {
		c.JSON(413, gin.H{"error": "retention metadata exceeds response budget"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}
