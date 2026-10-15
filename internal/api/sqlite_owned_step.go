package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// OwnedSQLiteStepOptionsProvider is trusted construction-time code. It supplies
// current gate epoch, unique attempt/instance identity and mandatory cleanup,
// never from HTTP query/body fields. No default/server configuration selects it.
type OwnedSQLiteStepOptionsProvider func(context.Context, string, string) (orchestrator.SQLiteOwnedExecutionOptions, error)
type ownedSQLiteStepAdapter struct {
	store   *store.SQLiteStore
	options OwnedSQLiteStepOptionsProvider
}

// SetOwnedSQLiteStepAdapter must be called before serving any request. It only
// opts /run JSON/SSE into the dedicated prototype; /run-all remains unchanged.
func (h *Handler) SetOwnedSQLiteStepAdapter(provider OwnedSQLiteStepOptionsProvider) error {
	st, ok := h.store.(*store.SQLiteStore)
	if !ok || provider == nil || h.engine == nil || h.engine.Store != st || h.engine.ExecutionAdmission != st {
		return types.ErrExecutionAdmission
	}
	h.ownedSQLiteStep = &ownedSQLiteStepAdapter{st, provider}
	return nil
}

// Returns true only when a worker owns the caller's acquired semaphore slot.
func (h *Handler) runOwnedSQLiteStep(c *gin.Context, ctx context.Context, cancel context.CancelFunc, task *types.Task) bool {
	adapter := h.ownedSQLiteStep
	taskID, tenantID := task.ID, task.TenantID
	options, err := adapter.options(ctx, taskID, tenantID)
	if err != nil {
		c.Error(types.ErrExecutionAdmission)
		return false
	}
	options.RunAll = false
	admitted := make(chan struct{}, 1)
	options.OnAdmitted = func(orchestrator.SQLiteOwnedExecutionIdentity) { admitted <- struct{}{} }
	token := &activeRun{cancel: cancel, owned: true}
	h.activeTasksMu.Lock()
	if _, exists := h.activeTasks[taskID]; exists {
		h.activeTasksMu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "task is already running", "task_id": taskID})
		return false
	}
	h.activeTasks[taskID] = token
	h.activeTasksMu.Unlock()
	stream := c.Query("stream") == "true"
	var events chan StepEvent
	if stream {
		events, _ = GetBus().Subscribe(taskID)
		defer GetBus().Unsubscribe(taskID, events)
	}
	result := make(chan error, 1)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer h.taskSem.Release()
		defer func() {
			h.activeTasksMu.Lock()
			if h.activeTasks[taskID] == token {
				delete(h.activeTasks, taskID)
			}
			h.activeTasksMu.Unlock()
		}()
		// Panic does not produce success/exit/cleanup receipts. Keep the server
		// process alive and unblock the handler while durable unresolved state stays.
		defer func() {
			if recover() != nil {
				result <- types.ErrExecutionAdmission
			}
		}()
		err := h.engine.RunOwnedSQLiteExecution(ctx, adapter.store, task, options)
		if err == nil && types.IsTerminalTaskStatus(task.Status) {
			GetBus().Publish(taskID, terminalStepEvent(taskID, task))
		}
		result <- err
	}()
	beginStream := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Header("Connection", "keep-alive")
	}
	drainEvents := func() {
		for {
			select {
			case event, ok := <-events:
				if !ok {
					return
				}
				if event.isTerminal() {
					continue
				}
				writeSSEEvent(c, event)
				c.Writer.Flush()
			default:
				return
			}
		}
	}
	respondError := func(err error) {
		if errors.Is(err, store.ErrSQLiteExecutionBlocked) || errors.Is(err, store.ErrSQLiteExecutionConflict) || errors.Is(err, types.ErrExecutionAdmission) {
			c.JSON(http.StatusConflict, gin.H{"error": "task execution admission rejected", "task_id": taskID})
			return
		}
		c.Error(err)
	}
	// No SSE headers or success response before durable attempt registration.
	select {
	case err := <-result:
		if err != nil {
			respondError(err)
		} else if stream {
			beginStream()
			drainEvents()
			if types.IsTerminalTaskStatus(task.Status) {
				writeSSEEvent(c, terminalStepEvent(taskID, task))
				c.Writer.Flush()
			}
		} else {
			c.JSON(http.StatusOK, task)
		}
		return true
	case <-ctx.Done():
		c.Error(ctx.Err())
		return true
	case <-admitted:
	}
	if !stream {
		select {
		case err := <-result:
			if err != nil {
				respondError(err)
			} else {
				c.JSON(http.StatusOK, task)
			}
		case <-ctx.Done():
			c.Error(ctx.Err())
		}
		return true
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Connection", "keep-alive")
	write := func(event StepEvent) { writeSSEEvent(c, event); c.Writer.Flush() }
	for {
		select {
		case <-ctx.Done():
			return true
		case event, ok := <-events:
			if !ok {
				return true
			}
			if !event.isTerminal() {
				write(event)
			}
		case executionErr := <-result:
			drainEvents()
			if executionErr == nil && types.IsTerminalTaskStatus(task.Status) {
				write(terminalStepEvent(taskID, task))
			}
			return true
		}
	}
}
