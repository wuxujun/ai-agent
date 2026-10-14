package api

import (
	"container/list"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// StepEvent is a Server-Sent Event payload pushed to the client after each step.
type StepEvent struct {
	Snapshot     *TaskSnapshotMetadata  `json:"snapshot,omitempty"`
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

// stickyTerminalTTL bounds terminal hints offered by Subscribe. Hints are
// process-local and may be stale; streamTask reads storage for reconciliation.
const stickyTerminalTTL = 5 * time.Minute

type stickyEvent struct {
	event     StepEvent
	expiresAt time.Time
}

// EventBus manages per-task SSE subscriber channels.
type EventBus struct {
	mu          sync.RWMutex
	subs        map[string][]chan StepEvent
	sticky      map[string]stickyEvent
	nowFunc     func() time.Time
	stickyOrder *list.List
	stickyNodes map[string]*list.Element
	stickyTimer *time.Timer
}

var globalEventBus = &EventBus{
	subs:    make(map[string][]chan StepEvent),
	sticky:  make(map[string]stickyEvent),
	nowFunc: time.Now,
}

// GetBus returns the singleton EventBus.
func GetBus() *EventBus { return globalEventBus }

// Subscribe registers a channel and returns a recent terminal hint, if any.
// The caller must Unsubscribe when done and verify hints against storage.
// This is not a durable event log or a replay cursor.
func (b *EventBus) Subscribe(taskID string) (chan StepEvent, *StepEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneStickyLocked()
	ch := make(chan StepEvent, 32)
	b.subs[taskID] = append(b.subs[taskID], ch)
	var sticky *StepEvent
	if s, ok := b.sticky[taskID]; ok {
		if b.nowFunc().Before(s.expiresAt) {
			ev := s.event
			sticky = &ev
		} else {
			b.removeStickyLocked(taskID)
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
// (Completed/Partial/Failed) are cached for stickyTerminalTTL so subscribers arriving
// just after the publish can still receive them.
func (b *EventBus) Publish(taskID string, event StepEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneStickyLocked()
	if event.isTerminal() {
		b.rememberStickyLocked(taskID, event)
	}
	// Keep sends under the same lock as Unsubscribe's channel close.
	for _, ch := range b.subs[taskID] {
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
	b.removeStickyLocked(taskID)
	b.scheduleStickyExpiryLocked()
	b.mu.Unlock()
}

// ForgetAll removes all cached terminal events after the task store is cleared.
func (b *EventBus) ForgetAll() {
	b.mu.Lock()
	b.sticky = make(map[string]stickyEvent)
	b.stickyOrder = nil
	b.stickyNodes = nil
	if b.stickyTimer != nil {
		b.stickyTimer.Stop()
		b.stickyTimer = nil
	}
	b.mu.Unlock()
}

// TaskSnapshotMetadata identifies a durable reconciliation snapshot. Counts
// are observations, not replay cursors: Trace may be truncated or rewritten.
type TaskSnapshotMetadata struct {
	TraceCount int       `json:"trace_count"`
	StepCount  int       `json:"step_count"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const taskStreamPollInterval = 15 * time.Second

// streamTask is a best-effort live stream with durable snapshot reconciliation.
// Last-Event-ID is deliberately not a replay cursor for this endpoint.
func (h *Handler) streamTask(c *gin.Context) {
	h.streamTaskWithBus(c, GetBus(), taskStreamPollInterval)
}

func (h *Handler) streamTaskWithBus(c *gin.Context, bus *EventBus, interval time.Duration) {
	taskID := c.Param("id")
	// Subscribe before reading state so no transition falls into a read/subscribe
	// gap. Sticky/live terminal events remain hints; only storage closes the stream.
	ch, _ := bus.Subscribe(taskID)
	defer bus.Unsubscribe(taskID, ch)
	read := func() (store.TaskStreamSnapshot, error) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		return h.readTaskStreamSnapshot(ctx, taskID)
	}
	task, err := read()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task state is temporarily unavailable"})
		}
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Connection", "keep-alive")
	c.Header("X-SSE-Recovery", "snapshot")
	writeSSEEvent(c, streamSnapshotEvent(task))
	c.Writer.Flush()
	if types.IsTerminalTaskStatus(task.Status) {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	reconcile := func() bool {
		latest, err := read()
		if err != nil {
			// No cached/live payload can replace an unavailable authoritative store.
			fmt.Fprint(c.Writer, ": reconciliation-unavailable\n\n")
			c.Writer.Flush()
			return false
		}
		writeSSEEvent(c, streamSnapshotEvent(latest))
		c.Writer.Flush()
		return types.IsTerminalTaskStatus(latest.Status)
	}
	for {
		select {
		case <-c.Request.Context().Done():
			log.InfoContext(c.Request.Context(), "SSE client disconnected", "task_id", taskID)
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			if event.isTerminal() {
				if reconcile() {
					return
				}
				continue
			}
			writeSSEEvent(c, event)
			c.Writer.Flush()
		case <-ticker.C:
			if reconcile() {
				return
			}
		}
	}
}

func (h *Handler) readTaskStreamSnapshot(ctx context.Context, id string) (store.TaskStreamSnapshot, error) {
	var snapshot store.TaskStreamSnapshot
	var err error
	if reader, ok := h.store.(store.TaskStreamSnapshotStore); ok {
		snapshot, err = reader.GetTaskStreamSnapshot(ctx, id)
	} else {
		var task *types.Task
		task, err = h.store.GetTask(ctx, id)
		if err == nil {
			snapshot = store.StreamSnapshotFromTask(task)
		}
	}
	if err != nil {
		return store.TaskStreamSnapshot{}, err
	}
	if snapshot.ID != id {
		return store.TaskStreamSnapshot{}, errors.New("task stream snapshot identity mismatch")
	}
	return snapshot, nil
}

func streamSnapshotEvent(snapshot store.TaskStreamSnapshot) StepEvent {
	usage := snapshot.TokenUsage
	return StepEvent{TaskID: snapshot.ID, Status: snapshot.Status, Final: snapshot.FinalAnswer, ErrorCode: snapshot.ErrorCode, ErrorMessage: snapshot.ErrorMessage, TokenUsage: &usage, Snapshot: &TaskSnapshotMetadata{TraceCount: snapshot.TraceCount, StepCount: snapshot.StepCount, UpdatedAt: snapshot.UpdatedAt}}
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
