package api

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Wait blocks until all background run-all goroutines complete. Call during shutdown.
func (h *Handler) Wait() {
	h.wg.Wait()
}

// ResizeTaskSemaphore applies a hot-reloaded concurrent task limit.
func (h *Handler) ResizeTaskSemaphore(limit int) {
	h.taskSem.Resize(limit)
}

// SetApprovalBus wires the optional distributed approval/cancel bus. Must be
// called before any tasks are started. Safe to call with a nil bus (no-op).
func (h *Handler) SetApprovalBus(bus *orchestrator.ApprovalBus) {
	h.approvalBus = bus
}

// Shutdown cancels active background tasks and waits for them to exit or for ctx to expire.
func (h *Handler) Shutdown(ctx context.Context) error {
	// Snapshot and cancel all active tasks.
	h.activeTasksMu.Lock()
	type runEntry struct {
		taskID string
		run    *activeRun
	}
	entries := make([]runEntry, 0, len(h.activeTasks))
	for id, run := range h.activeTasks {
		entries = append(entries, runEntry{taskID: id, run: run})
	}
	h.activeTasksMu.Unlock()

	for _, e := range entries {
		e.run.cancel()
	}

	// Wait for goroutines to finish flushing.
	done := make(chan struct{})
	go func() {
		h.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		log.Warn("Shutdown timed out; some tasks may not have flushed cleanly")
	}

	// ── P1: Graceful-shutdown rollback ──────────────────────────────────────
	// Tasks that were forcibly interrupted are currently stored with status
	// "failed" (set by SetTaskFailed inside RunAll). Roll them back to
	// "paused" so the next process restart can resume them via /run-all
	// (which now accepts StatusPaused as a valid start state).
	rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer rollbackCancel()
	for _, e := range entries {
		h.activeTasksMu.Lock()
		_, running := h.activeTasks[e.taskID]
		h.activeTasksMu.Unlock()
		if running && ctx.Err() != nil {
			log.Warn("skipping shutdown rollback for still-running task to avoid last-writer-wins race", "task_id", e.taskID)
			continue
		}

		if e.run.lease != nil && e.run.lease.Err() != nil {
			continue
		}
		h.rollbackInterruptedTask(rollbackCtx, e.taskID)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// The execution lease has already been released by goroutine cleanup. Obtain a
// fresh lease so a peer resuming the task cannot race shutdown's status rollback.
func (h *Handler) rollbackInterruptedTask(ctx context.Context, taskID string) {
	owner := uuid.NewString()
	acquired, err := h.store.AcquireTaskLease(ctx, taskID, owner, 30*time.Second)
	if err != nil || !acquired {
		return
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.store.ReleaseTaskLease(releaseCtx, taskID, owner)
	}()
	ctx = store.WithTaskLease(ctx, taskID, owner)
	task, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		log.Error("shutdown rollback: failed to fetch task", "task_id", taskID, "error", err)
		return
	}
	// Only roll back tasks that were running/queued or were explicitly canceled by
	// shutdown. Business failures must not be resurrected based on FinalAnswer text.
	if task.Status != types.StatusFailed && task.Status != types.StatusRunning {
		return
	}
	if task.Status == types.StatusFailed {
		switch task.TerminationKind {
		case types.TerminationShutdownRollback, types.TerminationClientCancelled:
			// Eligible for pausing after this process initiated graceful shutdown.
		case types.TerminationNone:
			// Backward-compatible fallback for pre-termination_kind rows: only empty
			// cancellation-shaped failures are resumable.
			if task.FinalAnswer != "" || task.ErrorCode == "" {
				return
			}
		default:
			return
		}
	}
	success, transitionErr := h.store.TryTransitionTaskStatus(ctx, taskID, []types.TaskStatus{types.StatusRunning, types.StatusFailed}, types.StatusPaused, types.TerminationShutdownRollback)
	if transitionErr != nil {
		log.Error("shutdown rollback: failed to pause task", "task_id", taskID, "error", transitionErr)
	} else if success {
		log.Info("shutdown rollback: task paused for resumption", "task_id", taskID)
	} else {
		log.Info("shutdown rollback: task status changed concurrently, skipping pause", "task_id", taskID)
	}
}
