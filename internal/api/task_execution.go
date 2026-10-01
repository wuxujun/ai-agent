package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (h *Handler) runTaskStep(c *gin.Context) {
	// 60s allows for LLM planner calls (P99 ≈ 30s) plus tool execution and DB write.
	// A 5s timeout was too short and would cancel in-flight LLM requests.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	// Acquire concurrency slot using the semaphore's authoritative hot-reloaded limit.
	if !h.taskSem.Acquire(ctx) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "too many concurrent tasks, please try again later"})
		return
	}
	handedOff := false
	defer func() {
		if !handedOff {
			h.taskSem.Release()
		}
	}()

	task, err := h.store.GetTask(ctx, c.Param("id"))
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}

	owner := uuid.NewString()
	run := &activeRun{cancel: cancel, owner: owner}
	h.activeTasksMu.Lock()
	if _, exists := h.activeTasks[task.ID]; exists {
		h.activeTasksMu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "task is already running", "task_id": task.ID})
		return
	}
	h.activeTasks[task.ID] = run
	h.activeTasksMu.Unlock()
	cleanupRun := func() {
		h.activeTasksMu.Lock()
		if cur, ok := h.activeTasks[task.ID]; ok && cur == run {
			delete(h.activeTasks, task.ID)
		}
		h.activeTasksMu.Unlock()
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer releaseCancel()
		if err := h.store.ReleaseTaskLease(releaseCtx, task.ID, owner); err != nil {
			log.Warn("failed to release task lease", "task_id", task.ID, "error", err)
		}
	}
	defer func() {
		if !handedOff {
			cleanupRun()
		}
	}()

	acquired, err := h.store.AcquireTaskLease(ctx, task.ID, owner, 90*time.Second)
	if err != nil {
		c.Error(err)
		return
	}
	if !acquired {
		c.JSON(http.StatusConflict, gin.H{"error": "task is already running on another instance", "task_id": task.ID})
		return
	}

	ctx = store.WithTaskLease(ctx, task.ID, owner)
	freshTask, err := h.store.GetTask(ctx, task.ID)
	if err != nil {
		c.Error(err)
		return
	}
	task = freshTask

	stream := c.Query("stream") == "true"
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Header("Connection", "keep-alive")

		ch, _ := GetBus().Subscribe(task.ID)
		defer GetBus().Unsubscribe(task.ID, ch)

		errChan := make(chan error, 1)
		handedOff = true
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer h.taskSem.Release()
			defer cleanupRun()
			execErr := h.engine.Next(ctx, task)
			if saveErr := h.store.SaveFullTask(ctx, task); saveErr != nil {
				errChan <- saveErr
				return
			}
			if types.IsTerminalTaskStatus(task.Status) {
				GetBus().Publish(task.ID, terminalStepEvent(task.ID, task))
			}
			errChan <- execErr
		}()

		clientGone := c.Request.Context().Done()
		for {
			select {
			case <-clientGone:
				return
			case <-errChan:
				// Drain any remaining events
				for {
					select {
					case event, ok := <-ch:
						if !ok {
							return
						}
						writeSSEEvent(c, event)
						c.Writer.Flush()
					default:
						return
					}
				}
			case event, ok := <-ch:
				if !ok {
					return
				}
				writeSSEEvent(c, event)
				c.Writer.Flush()
			}
		}
	} else {
		execErr := h.engine.Next(ctx, task)
		if saveErr := h.store.SaveFullTask(ctx, task); saveErr != nil {
			c.Error(saveErr)
			return
		}
		if execErr != nil {
			c.Error(execErr)
			return
		}

		c.JSON(http.StatusOK, task)
	}
}

func (h *Handler) runAll(c *gin.Context) {
	loadCtx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	task, err := h.store.GetTask(loadCtx, c.Param("id"))
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}

	resumingMultiAgent := h.engine.CanResumeTask(task)
	if types.IsTerminalTaskStatus(task.Status) && !resumingMultiAgent {
		c.JSON(http.StatusOK, task)
		return
	}

	leaseStore, ok := h.store.(store.TaskLeaseStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "store does not support task lease renewal"})
		return
	}

	// Reserve the in-process slot BEFORE the DB transition so we never have to
	// roll the DB back on collision. The slot is keyed by pointer identity
	// (activeRun token) so a stale deferred cleanup can't clobber a slot that
	// a subsequent runAll re-installed for the same task. We use a configurable
	// per-task wall-clock budget here; the engine still owns its own
	// step/tool/token budgets independently.
	timeout := time.Duration(config.Get().Orchestrator.RunAllTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	// Detach the asynchronous task from the HTTP request cancellation while
	// retaining request-scoped values such as the shared LLM Runtime and trace
	// context. The task's own wall-clock timeout remains the cancellation owner.
	owner := uuid.NewString()
	var lease *store.ExecutionLease
	bgBase := store.WithTaskLease(context.WithoutCancel(c.Request.Context()), task.ID, owner)
	bgBase = orchestrator.WithExecutionLeaseCheck(bgBase, func() error { return lease.Err() })
	bgCtx, bgCancel := orchestrator.WithPausableTimeout(bgBase, timeout)
	lease = store.NewExecutionLease(func() { orchestrator.CancelExecution(bgCtx, store.ErrTaskLeaseLost) })
	run := &activeRun{
		cancel: func() { orchestrator.CancelExecution(bgCtx, orchestrator.ErrTaskCanceledViaAPI) },
		owner:  owner,
		lease:  lease,
	}

	h.activeTasksMu.Lock()
	if _, exists := h.activeTasks[task.ID]; exists {
		h.activeTasksMu.Unlock()
		bgCancel()
		c.JSON(http.StatusConflict, gin.H{
			"error":   "task is already running",
			"task_id": task.ID,
		})
		return
	}
	h.activeTasks[task.ID] = run
	h.activeTasksMu.Unlock()

	acquiredAt := time.Now()
	acquired, err := h.store.AcquireTaskLease(loadCtx, task.ID, owner, timeout+30*time.Second)
	if err != nil || !acquired {
		h.activeTasksMu.Lock()
		if cur, ok := h.activeTasks[task.ID]; ok && cur == run {
			delete(h.activeTasks, task.ID)
		}
		h.activeTasksMu.Unlock()
		bgCancel()
		if err != nil {
			c.Error(err)
			return
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   "task is already running on another instance",
			"task_id": task.ID,
		})
		return
	}

	lease.Start(bgBase, leaseStore, task.ID, owner, timeout+30*time.Second, acquiredAt)

	cleanupStart := func() {
		// Reservation cleanup with compare-and-delete so we never erase a slot
		// some other goroutine just installed (cannot happen today because the
		// activeTasks mutex serializes inserts, but kept defensive).
		h.activeTasksMu.Lock()
		if cur, ok := h.activeTasks[task.ID]; ok && cur == run {
			delete(h.activeTasks, task.ID)
		}
		h.activeTasksMu.Unlock()
		bgCancel()
		lease.Finish()
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if releaseErr := h.store.ReleaseTaskLease(releaseCtx, task.ID, owner); releaseErr != nil {
			log.Warn("failed to release task lease after transition rejection", "task_id", task.ID, "error", releaseErr)
		}
		releaseCancel()
	}
	// The pre-lease read was only for admission. A peer may have committed a
	// newer checkpoint before releasing ownership; execution must use that copy.
	freshTask, loadErr := h.store.GetTask(loadCtx, task.ID)
	if loadErr != nil {
		cleanupStart()
		c.Error(loadErr)
		return
	}
	task = freshTask
	resumingMultiAgent = h.engine.CanResumeTask(task)
	if types.IsTerminalTaskStatus(task.Status) && !resumingMultiAgent {
		cleanupStart()
		c.JSON(http.StatusOK, task)
		return
	}

	// Perform atomic DB state transition to guard against multi-instance races.
	// The activeTasks reservation already serializes in-process callers; this
	// check protects against a peer process holding its own reservation.
	// StatusPaused is accepted here to support resuming tasks that were
	// interrupted by a previous graceful shutdown (P1 rollback).
	startableStatuses := []types.TaskStatus{types.StatusCreated, types.StatusRunning, types.StatusAwaitingApproval, types.StatusPaused}
	if resumingMultiAgent {
		startableStatuses = append(startableStatuses, types.StatusPartial)
	}
	success, err := h.store.TryTransitionTaskStatus(store.WithTaskLease(loadCtx, task.ID, owner), task.ID, startableStatuses, types.StatusRunning, types.TerminationNone)
	if err != nil || !success {
		cleanupStart()

		if err != nil {
			c.Error(err)
			return
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   "task status has changed or is already running",
			"task_id": task.ID,
		})
		return
	}

	task.Status = types.StatusRunning
	task.TerminationKind = types.TerminationNone

	// Snapshot the fields used in the response BEFORE handing the task pointer
	// to the goroutine. The goroutine may mutate task.Status (via engine /
	// SetTaskFailed) concurrently with gin's JSON marshalling otherwise.
	respID := task.ID
	respStatus := task.Status

	stream := c.Query("stream") == "true"
	var ch chan StepEvent
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Header("Connection", "keep-alive")

		ch, _ = GetBus().Subscribe(task.ID)
		defer GetBus().Unsubscribe(task.ID, ch)
	}

	errChan := make(chan error, 1)

	// Run asynchronously so the HTTP handler returns immediately (202 Accepted).
	// The caller should poll GET /api/tasks/:id to observe completion.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() {
			lease.Finish()
			h.activeTasksMu.Lock()
			if cur, ok := h.activeTasks[task.ID]; ok && cur == run {
				delete(h.activeTasks, task.ID)
			}
			h.activeTasksMu.Unlock()
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := h.store.ReleaseTaskLease(releaseCtx, task.ID, owner); err != nil {
				log.Warn("failed to release task lease", "task_id", task.ID, "error", err)
			}
			releaseCancel()
			bgCancel()
		}()

		// Wait for a concurrency slot, but honor cancellation while queued.
		if !h.taskSem.Acquire(bgCtx) {
			if leaseErr := lease.Err(); leaseErr != nil {
				errChan <- leaseErr
				return
			}
			_ = orchestrator.SetTaskCanceled(task, "task_canceled", "Task was canceled.")
			saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(bgCtx), 10*time.Second)
			defer saveCancel()
			if saveErr := h.store.SaveFullTask(saveCtx, task); saveErr != nil {
				log.Error("failed to save canceled queued task", "task_id", task.ID, "error", saveErr)
			}
			errChan <- bgCtx.Err()
			return
		}
		defer h.taskSem.Release()

		log.Info("starting async run-all for task", "task_id", task.ID)
		execErr := h.engine.RunAll(bgCtx, task)
		if errors.Is(execErr, store.ErrTaskLeaseLost) {
			lease.Lose()
		}
		if leaseErr := lease.Err(); leaseErr != nil {
			errChan <- leaseErr
			return
		}

		taskReportLog.Info("async run-all completed", "task_id", task.ID, "status", task.Status)
		taskReportLog.Info("--- TASK DECOMPOSITION & PLANNING RESULTS ---", "task_id", task.ID, "goal", task.Goal)
		if task.Hypothesis != "" {
			taskReportLog.Info("Thought Strategy / Hypothesis:", "task_id", task.ID, "hypothesis", task.Hypothesis)
		}
		if len(task.Unresolved) > 0 {
			taskReportLog.Info("Unresolved subtasks remaining:", "task_id", task.ID, "unresolved", task.Unresolved)
		}
		taskReportLog.Info("--- STEP BY STEP EXECUTION TRACE ---", "task_id", task.ID, "step_count", len(task.Trace))
		for _, tr := range task.Trace {
			roleStr := ""
			if tr.AgentRole != "" {
				roleStr = fmt.Sprintf(" [%s]", tr.AgentRole)
			}
			taskReportLog.Info("task step",
				"task_id", task.ID,
				"step", tr.Step,
				"agent_role", strings.TrimSpace(roleStr),
				"action", tr.Action,
				"query", tr.Query,
			)
			if tr.Observation != "" {
				obs := truncateTaskReportText(tr.Observation, 300)
				taskReportLog.Info("  Observation:", "task_id", task.ID, "content", obs)
			}
			if tr.Error != "" {
				taskReportLog.Info("  Error:", "task_id", task.ID, "error", tr.Error)
			}
		}
		taskReportLog.Info("----------------------------------------------", "task_id", task.ID)

		if execErr != nil {
			// RunAll persists every successful execution step, including the
			// terminal one. A second unconditional save here used to race with
			// asynchronous memory indexing and could issue duplicate embedding
			// requests. Retain only this compensation save for failure paths,
			// where RunAll may return before persisting the failed status.
			saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(bgCtx), 10*time.Second)
			if saveErr := h.store.SaveFullTask(saveCtx, task); saveErr != nil {
				if errors.Is(saveErr, store.ErrTaskLeaseLost) {
					lease.Lose()
				}
				log.Error("failed to save task after run-all failure", "task_id", task.ID, "error", saveErr)
			}
			saveCancel()
			log.Error("run-all failed for task", "task_id", task.ID, "error", execErr)
		}
		if leaseErr := lease.Err(); leaseErr != nil {
			errChan <- leaseErr
			return
		}
		// Publish terminal event to SSE subscribers
		GetBus().Publish(task.ID, terminalStepEvent(task.ID, task))
		errChan <- execErr
	}()

	if stream {
		clientGone := c.Request.Context().Done()
		for {
			select {
			case <-clientGone:
				log.Info("stream run-all: client disconnected, cancelling task", "task_id", task.ID)
				orchestrator.CancelExecution(bgCtx, orchestrator.ErrClientDisconnected)
				return
			case <-errChan:
				// Drain any remaining events
				for {
					select {
					case event, ok := <-ch:
						if !ok {
							return
						}
						writeSSEEvent(c, event)
						c.Writer.Flush()
					default:
						return
					}
				}
			case event, ok := <-ch:
				if !ok {
					return
				}
				writeSSEEvent(c, event)
				c.Writer.Flush()
				if event.isTerminal() {
					return
				}
			}
		}
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message": "task is running in background",
		"task_id": respID,
		"status":  respStatus,
	})
}

// cancelTask handles DELETE /api/tasks/:id/cancel.
// It cancels a running task if it is active in the current process, or marks it as failed in the DB.
func (h *Handler) cancelTask(c *gin.Context) {
	taskID := c.Param("id")

	if h.CancelTaskByID(taskID) {
		c.JSON(http.StatusOK, gin.H{
			"message":       "task cancellation signal sent",
			"task_id":       taskID,
			"error_code":    "task_canceled",
			"error_message": "Task was canceled via API.",
		})
		return
	}

	// ── P0: Not in local activeTasks — the task may be running on a peer
	// instance. Broadcast the cancel signal via Redis so the executing
	// instance can pick it up and cancel its context.
	if h.approvalBus != nil {
		if pubErr := h.approvalBus.PublishCancel(c.Request.Context(), taskID); pubErr != nil {
			log.Warn("cancel bus publish failed", "task_id", taskID, "error", pubErr)
		}
		c.JSON(http.StatusAccepted, gin.H{
			"message": "cancel signal forwarded to cluster",
			"task_id": taskID,
		})
		return
	}
	// No bus — fall back to the legacy DB-level cancel.
	ctx, dbCancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer dbCancel()
	task, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.Error(err)
		return
	}

	if task.Status == types.StatusRunning {
		if err := orchestrator.SetTaskCanceled(task, "task_canceled", "Task was canceled via API."); err != nil {
			c.Error(err)
			return
		}
		if saveErr := h.store.SaveFullTask(ctx, task); saveErr != nil {
			c.Error(saveErr)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"message": "task cancelled (marked failed in database)",
			"task_id": taskID,
		})
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{"error": "task is not running", "status": task.Status})
}

// CancelTaskByID fires the context cancellation for a locally-running task.
// Returns true if the task was found and cancelled; false if it is not running
// in this process (caller may then try a remote signal via the ApprovalBus).
func (h *Handler) CancelTaskByID(taskID string) bool {
	h.activeTasksMu.Lock()
	run, exists := h.activeTasks[taskID]
	if exists {
		delete(h.activeTasks, taskID)
	}
	h.activeTasksMu.Unlock()

	if !exists {
		return false
	}
	run.cancel()
	return true
}
