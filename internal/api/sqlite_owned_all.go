package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// SetOwnedSQLiteRunAllAdapter selects the dedicated prototype for /run-all.
// Call before serving requests. The provider is trusted construction-time code;
// no server configuration enables it and no HTTP fields select ownership.
func (h *Handler) SetOwnedSQLiteRunAllAdapter(provider OwnedSQLiteStepOptionsProvider) error {
	st, ok := h.store.(*store.SQLiteStore)
	if !ok || provider == nil || h.engine == nil || h.engine.Store != st || h.engine.ExecutionAdmission != st {
		return types.ErrExecutionAdmission
	}
	h.ownedSQLiteRunAll = &ownedSQLiteStepAdapter{st, provider}
	return nil
}

func (h *Handler) runOwnedSQLiteAll(c *gin.Context, task *types.Task) {
	adapter := h.ownedSQLiteRunAll
	taskID, tenantID := task.ID, task.TenantID
	timeout := time.Duration(config.Get().Orchestrator.RunAllTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := orchestrator.WithPausableTimeout(context.WithoutCancel(c.Request.Context()), timeout)
	token := &activeRun{owned: true, cancel: func() { orchestrator.CancelExecution(ctx, orchestrator.ErrTaskCanceledViaAPI) }}
	h.activeTasksMu.Lock()
	if h.activeTasks[taskID] != nil {
		h.activeTasksMu.Unlock()
		cancel()
		c.JSON(http.StatusConflict, gin.H{"error": "task is already running", "task_id": taskID})
		return
	}
	h.activeTasks[taskID] = token
	h.activeTasksMu.Unlock()
	h.wg.Add(1)
	release := func() {
		cancel()
		h.activeTasksMu.Lock()
		if h.activeTasks[taskID] == token {
			delete(h.activeTasks, taskID)
		}
		h.activeTasksMu.Unlock()
		h.wg.Done()
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	reject := func(err error) {
		if errors.Is(err, types.ErrExecutionAdmission) || errors.Is(err, store.ErrSQLiteExecutionBlocked) || errors.Is(err, store.ErrSQLiteExecutionConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "task execution admission rejected", "task_id": taskID})
		} else {
			c.Error(err)
		}
	}
	options, err := adapter.options(ctx, taskID, tenantID)
	if err != nil {
		reject(types.ErrExecutionAdmission)
		return
	}
	options.RunAll = true
	options.OnAdmitted = nil
	scope, err := h.engine.PrepareOwnedSQLiteExecution(ctx, adapter.store, task, options)
	if err != nil {
		reject(err)
		return
	}
	// A scope is consumed even if reload/status admission rejects or a trusted
	// preflight panics. Abort owns its exit, final flush, cleanup and release.
	consumed := false
	defer func() {
		if !consumed {
			_ = scope.Abort(types.ErrExecutionAdmission)
		}
	}()
	resuming := h.engine.CanResumeTask(task)
	if types.IsTerminalTaskStatus(task.Status) && !resuming {
		consumed = true
		if err := scope.Abort(nil); err != nil {
			reject(err)
			return
		}
		c.JSON(http.StatusOK, task)
		return
	}
	statuses := []types.TaskStatus{types.StatusCreated, types.StatusRunning, types.StatusAwaitingApproval, types.StatusPaused}
	if resuming {
		statuses = append(statuses, types.StatusPartial)
	}
	ok, err := adapter.store.TryTransitionTaskStatus(scope.Context(), taskID, statuses, types.StatusRunning, types.TerminationNone)
	if err != nil || !ok {
		if err == nil {
			err = types.ErrExecutionAdmission
		}
		consumed = true
		reject(scope.Abort(err))
		return
	}
	task.Status = types.StatusRunning
	task.TerminationKind = types.TerminationNone
	stream := c.Query("stream") == "true"
	var events chan StepEvent
	if stream {
		events, _ = GetBus().Subscribe(taskID)
		defer GetBus().Unsubscribe(taskID, events)
	}
	result := make(chan error, 1)
	// Durable registration, checkpoint reload and status CAS all precede 202.
	consumed, handedOff = true, true
	go func() {
		defer release()
		defer func() {
			if recover() != nil {
				result <- types.ErrExecutionAdmission
			}
		}()
		err := scope.Execute(func(owned context.Context, current *types.Task) error {
			if !h.taskSem.Acquire(owned) {
				_ = orchestrator.SetTaskCanceled(current, "task_canceled", "Task was canceled.")
				return owned.Err()
			}
			defer h.taskSem.Release()
			return h.engine.RunAll(owned, current)
		})
		if err == nil && types.IsTerminalTaskStatus(task.Status) {
			GetBus().Publish(taskID, terminalStepEvent(taskID, task))
		}
		result <- err
	}()
	if !stream {
		c.JSON(http.StatusAccepted, gin.H{"message": "task is running in background", "task_id": taskID, "status": types.StatusRunning})
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Connection", "keep-alive")
	write := func(event StepEvent) {
		if !event.isTerminal() {
			writeSSEEvent(c, event)
			c.Writer.Flush()
		}
	}
	for {
		select {
		case <-c.Request.Context().Done():
			orchestrator.CancelExecution(ctx, orchestrator.ErrClientDisconnected)
			return
		case event, ok := <-events:
			if !ok {
				orchestrator.CancelExecution(ctx, orchestrator.ErrClientDisconnected)
				return
			}
			write(event)
		case err := <-result:
			for {
				select {
				case event, ok := <-events:
					if !ok {
						return
					}
					write(event)
				default:
					if err == nil && types.IsTerminalTaskStatus(task.Status) {
						writeSSEEvent(c, terminalStepEvent(taskID, task))
						c.Writer.Flush()
					}
					return
				}
			}
		}
	}
}
