package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/types"
)

// StepEvent is a Server-Sent Event payload pushed to the client after each step.
type StepEvent struct {
	TaskID string           `json:"task_id"`
	Status types.TaskStatus `json:"status"`
	Step   *types.StepTrace `json:"step,omitempty"`
	Final  string           `json:"final_answer,omitempty"`
	Token  string           `json:"token,omitempty"` // For streaming tokens
}

// EventBus manages per-task SSE subscriber channels.
type EventBus struct {
	mu   sync.RWMutex
	subs map[string][]chan StepEvent
}

var globalEventBus = &EventBus{
	subs: make(map[string][]chan StepEvent),
}

// GetBus returns the singleton EventBus.
func GetBus() *EventBus { return globalEventBus }

// Subscribe registers a new channel for events on taskID.
// The caller must call Unsubscribe when done.
func (b *EventBus) Subscribe(taskID string) chan StepEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan StepEvent, 32)
	b.subs[taskID] = append(b.subs[taskID], ch)
	return ch
}

// Unsubscribe removes and closes a subscriber channel.
func (b *EventBus) Unsubscribe(taskID string, ch chan StepEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	chans := b.subs[taskID]
	for i, c := range chans {
		if c == ch {
			b.subs[taskID] = append(chans[:i], chans[i+1:]...)
			close(ch)
			break
		}
	}
	if len(b.subs[taskID]) == 0 {
		delete(b.subs, taskID)
	}
}

// Publish sends an event to all subscribers of taskID.
func (b *EventBus) Publish(taskID string, event StepEvent) {
	b.mu.RLock()
	chans := make([]chan StepEvent, len(b.subs[taskID]))
	copy(chans, b.subs[taskID])
	b.mu.RUnlock()

	for _, ch := range chans {
		select {
		case ch <- event:
		default:
			// Slow consumer: drop event rather than block
		}
	}
}

// streamTask handles GET /api/tasks/:id/stream — Server-Sent Events endpoint.
// The client receives a StepEvent JSON object for every completed step and a
// final event when the task reaches completed or failed status.
func (h *Handler) streamTask(c *gin.Context) {
	taskID := c.Param("id")

	// Validate task existence first
	checkCtx, checkCancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer checkCancel()
	task, err := h.store.GetTask(checkCtx, taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}

	// If task is already terminal, return its final state immediately as a single event
	if task.Status == types.StatusCompleted || task.Status == types.StatusFailed {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		event := StepEvent{
			TaskID: taskID,
			Status: task.Status,
			Final:  task.FinalAnswer,
		}
		writeSSEEvent(c, event)
		return
	}

	// Subscribe to live events
	bus := GetBus()
	ch := bus.Subscribe(taskID)
	defer bus.Unsubscribe(taskID, ch)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Connection", "keep-alive")

	clientGone := c.Request.Context().Done()

	for {
		select {
		case <-clientGone:
			slog.Info("SSE client disconnected", slog.String("task_id", taskID))
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			writeSSEEvent(c, event)
			c.Writer.Flush()
			// Stop streaming once task is terminal
			if event.Status == types.StatusCompleted || event.Status == types.StatusFailed {
				return
			}
		case <-time.After(25 * time.Second):
			// Send a keep-alive comment so the connection is not dropped by proxies
			fmt.Fprintf(c.Writer, ": keep-alive\n\n")
			c.Writer.Flush()
		}
	}
}

func writeSSEEvent(c *gin.Context, event StepEvent) {
	b, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Fprintf(c.Writer, "data: %s\n\n", b)
}
