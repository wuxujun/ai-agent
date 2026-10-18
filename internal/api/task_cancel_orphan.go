package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func orphanTaskCancelable(task *types.Task) bool {
	return task.Status == types.StatusRunning || task.Status == types.StatusAwaitingApproval || task.Status == types.StatusPaused ||
		(task.Status == types.StatusFailed && task.TerminationKind == types.TerminationUserCancelled)
}

// This is the no-local-owner/no-bus fallback. A real peer lease must be absent
// before modifying the checkpoint. Persist non-recoverable user cancellation
// first, then expire outstanding decisions; partial expiration errors remain
// fail closed and can be retried without resurrecting or replaying an approval.
func (h *Handler) cancelOrphanedTask(c *gin.Context, ctx context.Context, task *types.Task) {
	reject := func() { c.JSON(http.StatusBadRequest, gin.H{"error": "task is not running", "status": task.Status}) }
	if !orphanTaskCancelable(task) {
		reject()
		return
	}
	taskID, tenantID := task.ID, task.TenantID
	owner := "cancel-" + uuid.NewString()
	acquired, err := h.store.AcquireTaskLease(ctx, taskID, owner, 30*time.Second)
	if err != nil {
		c.Error(err)
		return
	}
	if !acquired {
		if h.approvalBus != nil {
			if err := h.approvalBus.PublishCancel(ctx, taskID); err != nil {
				c.Error(err)
				return
			}
			c.JSON(http.StatusAccepted, gin.H{"message": "cancel signal forwarded to cluster", "task_id": taskID})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": "task is active on another instance", "task_id": taskID})
		return
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := h.store.ReleaseTaskLease(releaseCtx, taskID, owner); err != nil {
			log.Warn("failed to release cancellation lease", "task_id", taskID, "error", err)
		}
	}()
	ctx = store.WithTaskLease(ctx, taskID, owner)
	task, err = h.store.GetTask(ctx, taskID)
	if err != nil {
		c.Error(err)
		return
	}
	if task.ID != taskID || task.TenantID != tenantID {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if !orphanTaskCancelable(task) {
		reject()
		return
	}
	if task.Status != types.StatusFailed || task.TerminationKind != types.TerminationUserCancelled {
		if err := orchestrator.SetTaskCanceled(task, "task_canceled", "Task was canceled via API."); err != nil {
			c.Error(err)
			return
		}
		task.TerminationKind = types.TerminationUserCancelled
		if err := h.store.SaveFullTask(ctx, task); err != nil {
			c.Error(err)
			return
		}
	}
	if ds, ok := h.store.(store.DurableApprovalStore); ok {
		if err := expireOrphanTaskApprovals(ctx, ds, task); err != nil {
			c.Error(err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "task cancelled (marked failed in database)", "task_id": taskID})
}

func expireOrphanTaskApprovals(ctx context.Context, ds store.DurableApprovalStore, task *types.Task) error {
	tenant := task.TenantID
	if tenant == "" {
		tenant = "default"
	}
	for _, status := range []types.DurableApprovalStatus{types.ApprovalPending, types.ApprovalApproved} {
		records, err := ds.ListTaskApprovals(ctx, task.ID, tenant, status)
		if err != nil {
			return err
		}
		for _, record := range records {
			for retry := 0; retry < 3; retry++ {
				if record.TaskID != task.ID || record.TenantID != tenant {
					return fmt.Errorf("approval scope mismatch during cancellation")
				}
				if record.Status != types.ApprovalPending && record.Status != types.ApprovalApproved {
					break
				}
				matched, err := ds.TransitionApproval(ctx, record.ID, tenant, record.Version, record.Status, types.ApprovalExpired, record.ResolutionPayload)
				if err != nil {
					return err
				}
				if matched {
					break
				}
				// A resolver may have won pending->approved while cancellation was being
				// persisted. Re-read and expire that decision too; never retry execution.
				latest, err := ds.GetApproval(ctx, record.ID, tenant)
				if errors.Is(err, sql.ErrNoRows) {
					break
				}
				if err != nil {
					return err
				}
				record = latest
				if retry == 2 && (record.Status == types.ApprovalPending || record.Status == types.ApprovalApproved) {
					return fmt.Errorf("approval state changed during cancellation")
				}
			}
		}
	}
	return nil
}
