package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// approvalAction is the optional JSON body for /approve and /reject. When
// approval_id is empty, the unique pending approval for the task is resolved;
// when ambiguous (>1 pending) the handler returns 409 with the pending IDs.
type approvalAction struct {
	ApprovalID string         `json:"approval_id"`
	Message    string         `json:"message"`
	Parameters map[string]any `json:"parameters"`
}

func (h *Handler) approveTask(c *gin.Context) {
	h.resolveTaskApproval(c, true)
}

func (h *Handler) rejectTask(c *gin.Context) {
	h.resolveTaskApproval(c, false)
}

func (h *Handler) resolveTaskApproval(c *gin.Context, approved bool) {
	taskID := c.Param("id")

	var body approvalAction
	// Best-effort decode: an empty/malformed body is fine — we'll fall back to
	// the single-pending lookup. ShouldBindBodyWith would be stricter but
	// changes semantics for callers that omit the body.
	_ = c.ShouldBindJSON(&body)

	result := types.ApprovalResult{
		Approved:   approved,
		Message:    body.Message,
		Parameters: body.Parameters,
		ActorID:    principalFromGin(c).ActorID,
	}

	if body.ApprovalID != "" {
		var durableApproval *types.ApprovalRequest
		var durableExists, persisted bool
		var persistErr error
		if h.engine != nil {
			durableApproval, durableExists, persisted, persistErr = h.engine.PersistApprovalResolution(c.Request.Context(), taskID, body.ApprovalID, result)
		}
		if persistErr != nil {
			if errors.Is(persistErr, orchestrator.ErrApprovalExpired) {
				c.JSON(http.StatusGone, gin.H{"error": "approval has expired", "approval_id": body.ApprovalID})
				return
			}
			log.Error("durable approval resolution failed", "task_id", taskID, "approval_id", body.ApprovalID, "error", persistErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist approval decision"})
			return
		}
		if durableExists && !persisted {
			h.observeDurableApproval(c.Request.Context(), "conflict")
			c.JSON(http.StatusConflict, gin.H{"error": "approval was already resolved", "approval_id": body.ApprovalID})
			return
		}
		approval, ok := orchestrator.GetApprovalByID(body.ApprovalID)
		if !ok {
			if persisted {
				h.startDurableApprovalRecovery(taskID, body.ApprovalID)
			}
			// ── P0: Not found locally — broadcast via Redis so executing instance picks it up ──
			if h.approvalBus != nil {
				if pubErr := h.approvalBus.PublishApproval(c.Request.Context(), body.ApprovalID, taskID, result); pubErr != nil {
					log.Warn("approval bus publish failed", "task_id", taskID, "error", pubErr)
				}
				c.JSON(http.StatusAccepted, gin.H{"message": "approval signal forwarded to cluster", "approval_id": body.ApprovalID})
				return
			}
			if persisted {
				c.JSON(http.StatusAccepted, h.approvalResponseMessage(approved, durableApproval))
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval matches approval_id"})
			return
		}
		if !orchestrator.ResolveApprovalByID(body.ApprovalID, result) {
			// Lost the race with another resolver between Get and Resolve.
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval matches approval_id"})
			return
		}
		c.JSON(http.StatusOK, h.approvalResponseMessage(approved, approval))
		return
	}

	pending := orchestrator.ListPendingApprovals(taskID)
	switch len(pending) {
	case 0:
		if h.engine != nil {
			durableApproval, approvalID, durableCount, persisted, persistErr := h.engine.PersistUniqueApprovalResolution(c.Request.Context(), taskID, result)
			if persistErr != nil {
				if errors.Is(persistErr, orchestrator.ErrApprovalExpired) {
					c.JSON(http.StatusGone, gin.H{"error": "approval has expired", "approval_id": approvalID})
					return
				}
				log.Error("durable approval resolution failed", "task_id", taskID, "error", persistErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist approval decision"})
				return
			}
			if durableCount > 1 {
				h.observeDurableApproval(c.Request.Context(), "conflict")
				c.JSON(http.StatusConflict, gin.H{"error": "multiple pending approvals; specify approval_id", "pending_count": durableCount})
				return
			}
			if durableCount == 1 && !persisted {
				h.observeDurableApproval(c.Request.Context(), "conflict")
				c.JSON(http.StatusConflict, gin.H{"error": "approval was already resolved", "approval_id": approvalID})
				return
			}
			if persisted {
				h.startDurableApprovalRecovery(taskID, approvalID)
				if h.approvalBus != nil {
					if pubErr := h.approvalBus.PublishApproval(c.Request.Context(), approvalID, taskID, result); pubErr != nil {
						log.Warn("approval bus publish failed", "task_id", taskID, "error", pubErr)
					}
				}
				c.JSON(http.StatusAccepted, h.approvalResponseMessage(approved, durableApproval))
				return
			}
		}
		// ── P0: Not found locally — broadcast via Redis ──
		if h.approvalBus != nil {
			if pubErr := h.approvalBus.PublishApproval(c.Request.Context(), "", taskID, result); pubErr != nil {
				log.Warn("approval bus publish failed", "task_id", taskID, "error", pubErr)
			}
			c.JSON(http.StatusAccepted, gin.H{"message": "approval signal forwarded to cluster", "task_id": taskID})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this task"})
		return
	case 1:
		approval := pending[0]
		if h.engine != nil {
			_, durableExists, persisted, persistErr := h.engine.PersistApprovalResolution(c.Request.Context(), taskID, approval.ID, result)
			if persistErr != nil {
				if errors.Is(persistErr, orchestrator.ErrApprovalExpired) {
					c.JSON(http.StatusGone, gin.H{"error": "approval has expired", "approval_id": approval.ID})
					return
				}
				log.Error("durable approval resolution failed", "task_id", taskID, "approval_id", approval.ID, "error", persistErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist approval decision"})
				return
			}
			if durableExists && !persisted {
				h.observeDurableApproval(c.Request.Context(), "conflict")
				c.JSON(http.StatusConflict, gin.H{"error": "approval was already resolved", "approval_id": approval.ID})
				return
			}
		}
		if !orchestrator.ResolveApproval(taskID, result) {
			// Lost the race — another resolver beat us between List and Resolve.
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this task"})
			return
		}
		c.JSON(http.StatusOK, h.approvalResponseMessage(approved, approval))
		return
	default:
		// Multiple pending — the API contract requires explicit approval_id
		// to disambiguate. Surface the IDs so the caller can pick.
		ids := make([]string, 0, len(pending))
		for _, p := range pending {
			ids = append(ids, p.ID)
		}
		h.observeDurableApproval(c.Request.Context(), "conflict")
		c.JSON(http.StatusConflict, gin.H{
			"error":         "multiple pending approvals; specify approval_id",
			"pending_count": len(pending),
			"approval_ids":  ids,
			"pending":       pending,
		})
		return
	}
}

func (h *Handler) observeDurableApproval(ctx context.Context, event string) {
	if h.metrics != nil {
		h.metrics.ObserveDurableApproval(ctx, event)
	}
}

func (h *Handler) startDurableApprovalRecovery(taskID, approvalID string) {
	if h.engine == nil || taskID == "" || approvalID == "" {
		return
	}
	durableStore, ok := h.store.(store.DurableApprovalStore)
	if !ok {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		owner := "approval-recovery-" + uuid.NewString()
		for {
			task, err := h.store.GetTask(ctx, taskID)
			if err != nil {
				h.observeDurableApproval(ctx, "recovery_failure")
				return
			}
			tenantID := task.TenantID
			if tenantID == "" {
				tenantID = "default"
			}
			approval, err := durableStore.GetApproval(ctx, approvalID, tenantID)
			if err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					h.observeDurableApproval(ctx, "recovery_failure")
				}
				return
			}
			var recovered bool
			switch approval.Status {
			case types.ApprovalApproved:
				recovered, err = h.engine.RecoverApprovedApproval(ctx, task, approval, owner)
			case types.ApprovalRejected:
				recovered, err = h.engine.RecoverRejectedApproval(ctx, task, approval, owner)
			default:
				return
			}
			if errors.Is(err, store.ErrTaskLeaseBusy) {
				select {
				case <-ctx.Done():
					h.observeDurableApproval(ctx, "recovery_failure")
					return
				case <-time.After(500 * time.Millisecond):
					continue
				}
			}
			if err != nil {
				h.observeDurableApproval(ctx, "recovery_failure")
				log.Error("durable approval recovery failed", "task_id", taskID, "approval_id", approvalID, "error", err)
			} else if recovered {
				log.Info("durable approval recovered", "task_id", taskID, "approval_id", approvalID, "status", types.StatusPaused)
			}
			return
		}
	}()
}

func (h *Handler) approvalResponseMessage(approved bool, approval *types.ApprovalRequest) gin.H {
	if approved {
		return gin.H{"message": "task action approved", "approval": approval}
	}
	return gin.H{"message": "task action rejected", "approval": approval}
}
