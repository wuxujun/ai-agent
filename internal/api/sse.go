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
	TaskID       string                 `json:"task_id"`
	Status       types.TaskStatus       `json:"status"`
	Step         *types.StepTrace       `json:"step,omitempty"`
	Final        string                 `json:"final_answer,omitempty"`
	ErrorCode    string                 `json:"error_code,omitempty"`
	ErrorMessage string                 `json:"error_message,omitempty"`
	Token        string                 `json:"token,omitempty"` // For streaming tokens
	TokenUsage   *types.TokenUsage      `json:"token_usage,omitempty"`
	Approval     *types.ApprovalRequest `json:"approval,omitempty"`
}

func (e StepEvent) isTerminal() bool {
	return e.Step == nil && types.IsTerminalTaskStatus(e.Status)
}

// stickyTerminalTTL bounds how long the EventBus replays a task's terminal
// event to late subscribers. The 15s store-poll backstop in streamTask already
// catches anything older than this; the sticky cache exists purely to close the
// short window between Publish() and a subscriber's Subscribe() landing.
const stickyTerminalTTL = 5 * time.Minute

type stickyEvent struct {
	event     StepEvent
	expiresAt time.Time
}

// EventBus manages per-task SSE subscriber channels.
type EventBus struct {
	mu      sync.RWMutex
	subs    map[string][]chan StepEvent
	sticky  map[string]stickyEvent
	nowFunc func() time.Time
}

var globalEventBus = &EventBus{
	subs:    make(map[string][]chan StepEvent),
	sticky:  make(map[string]stickyEvent),
	nowFunc: time.Now,
}

// GetBus returns the singleton EventBus.
func GetBus() *EventBus { return globalEventBus }

// Subscribe registers a new channel for events on taskID.
// The caller must call Unsubscribe when done. If a terminal event for taskID
// was published within the sticky TTL, it is returned so the caller can replay
// it before entering the live-event loop — this closes the race window between
// Publish() and a late Subscribe() that would otherwise wait for the 15s
// store-poll backstop in streamTask.
func (b *EventBus) Subscribe(taskID string) (chan StepEvent, *StepEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan StepEvent, 32)
	b.subs[taskID] = append(b.subs[taskID], ch)
	var sticky *StepEvent
	if s, ok := b.sticky[taskID]; ok {
		if b.nowFunc().Before(s.expiresAt) {
			ev := s.event
			sticky = &ev
		} else {
			delete(b.sticky, taskID)
		}
	}
	return ch, sticky
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

// Publish sends an event to all subscribers of taskID. Terminal events
// (Completed/Failed) are cached for stickyTerminalTTL so subscribers arriving
// just after the publish can still receive them.
func (b *EventBus) Publish(taskID string, event StepEvent) {
	b.mu.Lock()
	if event.isTerminal() {
		b.sticky[taskID] = stickyEvent{
			event:     event,
			expiresAt: b.nowFunc().Add(stickyTerminalTTL),
		}
	}
	chans := make([]chan StepEvent, len(b.subs[taskID]))
	copy(chans, b.subs[taskID])
	b.mu.Unlock()

	for _, ch := range chans {
		select {
		case ch <- event:
		default:
			// Slow consumer: drop event rather than block. Terminal events are
			// recoverable via the sticky cache above and the store-poll backstop.
		}
	}
}

// Forget removes a cached terminal event after its task is deleted.
func (b *EventBus) Forget(taskID string) {
	b.mu.Lock()
	delete(b.sticky, taskID)
	b.mu.Unlock()
}

// ForgetAll removes all cached terminal events after the task store is cleared.
func (b *EventBus) ForgetAll() {
	b.mu.Lock()
	b.sticky = make(map[string]stickyEvent)
	b.mu.Unlock()
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
	if types.IsTerminalTaskStatus(task.Status) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		writeSSEEvent(c, terminalStepEvent(taskID, task))
		return
	}

	// Subscribe to live events
	bus := GetBus()
	ch, sticky := bus.Subscribe(taskID)
	defer bus.Unsubscribe(taskID, ch)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Connection", "keep-alive")

	// Replay a sticky terminal event if one was Publish()'d in the narrow
	// window between the GetTask check above and Subscribe landing. Without
	// this, a late subscriber would wait for the 15s poll backstop below.
	if sticky != nil {
		writeSSEEvent(c, *sticky)
		c.Writer.Flush()
		return
	}

	clientGone := c.Request.Context().Done()

	// Reliability backstop: Publish drops events for slow consumers (see
	// Publish), so a terminal event can be lost if this subscriber's buffer is
	// momentarily full. Poll the store periodically and, once the task reaches a
	// terminal state, emit an authoritative terminal event and return — even if
	// the live event was dropped. The store is the source of truth. A
	// non-terminal poll tick doubles as a keep-alive so proxies don't drop idle
	// connections.
	pollTicker := time.NewTicker(15 * time.Second)
	defer pollTicker.Stop()

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
			if event.isTerminal() {
				return
			}
		case <-pollTicker.C:
			// Backstop check: has the task reached a terminal state in the store?
			pollCtx, pollCancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
			latest, err := h.store.GetTask(pollCtx, taskID)
			pollCancel()
			if err != nil {
				// Transient store error: keep the stream alive and retry next tick.
				fmt.Fprintf(c.Writer, ": keep-alive\n\n")
				c.Writer.Flush()
				continue
			}
			if types.IsTerminalTaskStatus(latest.Status) {
				writeSSEEvent(c, terminalStepEvent(taskID, latest))
				c.Writer.Flush()
				return
			}
			// Non-terminal: send a keep-alive comment so proxies keep the
			// connection open.
			fmt.Fprintf(c.Writer, ": keep-alive\n\n")
			c.Writer.Flush()
		}
	}
}

func terminalStepEvent(taskID string, task *types.Task) StepEvent {
	usage := aggregateTokenUsage(task)
	return StepEvent{
		TaskID:       taskID,
		Status:       task.Status,
		Final:        task.FinalAnswer,
		ErrorCode:    task.ErrorCode,
		ErrorMessage: task.ErrorMessage,
		TokenUsage:   &usage,
	}
}

func aggregateTokenUsage(task *types.Task) types.TokenUsage {
	var usage types.TokenUsage
	if task == nil {
		return usage
	}
	for _, tr := range task.Trace {
		usage.PromptTokens += tr.TokenUsage.PromptTokens
		usage.CompletionTokens += tr.TokenUsage.CompletionTokens
		usage.TotalTokens += tr.TokenUsage.TotalTokens
	}
	return usage
}

func writeSSEEvent(c *gin.Context, event StepEvent) {
	b, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Fprintf(c.Writer, "data: %s\n\n", b)
}
