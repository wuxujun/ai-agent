package api

import (
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func newTestBus(now func() time.Time) *EventBus {
	return &EventBus{
		subs:    make(map[string][]chan StepEvent),
		sticky:  make(map[string]stickyEvent),
		nowFunc: now,
	}
}

// TestSubscribeAfterTerminalPublishReplaysSticky covers the race the sticky
// cache exists to close: a terminal Publish lands before Subscribe registers,
// so the subscriber's live channel never sees it. Without the sticky replay
// the subscriber would wait for the 15s store-poll backstop in streamTask;
// with it, Subscribe returns the cached event for immediate replay.
func TestSubscribeAfterTerminalPublishReplaysSticky(t *testing.T) {
	bus := newTestBus(time.Now)

	bus.Publish("task-late-sub", StepEvent{
		TaskID: "task-late-sub",
		Status: types.StatusCompleted,
		Final:  "done",
	})

	ch, sticky := bus.Subscribe("task-late-sub")
	defer bus.Unsubscribe("task-late-sub", ch)

	if sticky == nil {
		t.Fatal("Subscribe returned nil sticky after terminal Publish; replay would be lost")
	}
	if sticky.Status != types.StatusCompleted || sticky.Final != "done" {
		t.Errorf("sticky payload = %+v, want completed/done", sticky)
	}
}

// TestNonTerminalPublishIsNotSticky guards against the sticky cache being used
// for routine step events. Only terminal events (Completed/Failed) should be
// replayed — running events would deliver stale state to fresh subscribers.
func TestNonTerminalPublishIsNotSticky(t *testing.T) {
	bus := newTestBus(time.Now)

	bus.Publish("task-step", StepEvent{
		TaskID: "task-step",
		Status: types.StatusRunning,
	})

	ch, sticky := bus.Subscribe("task-step")
	defer bus.Unsubscribe("task-step", ch)

	if sticky != nil {
		t.Errorf("non-terminal Publish was cached as sticky: %+v", sticky)
	}
}

// TestStickyExpiresAfterTTL ensures the cache does not grow unbounded for
// long-lived processes: an entry older than stickyTerminalTTL is dropped on
// the next Subscribe and not replayed.
func TestStickyExpiresAfterTTL(t *testing.T) {
	now := time.Now()
	clock := now
	bus := newTestBus(func() time.Time { return clock })

	bus.Publish("task-expired", StepEvent{
		TaskID: "task-expired",
		Status: types.StatusFailed,
		Final:  "boom",
	})

	clock = now.Add(stickyTerminalTTL + time.Second)

	ch, sticky := bus.Subscribe("task-expired")
	defer bus.Unsubscribe("task-expired", ch)

	if sticky != nil {
		t.Errorf("expected sticky entry to be expired and dropped, got %+v", sticky)
	}

	bus.mu.RLock()
	_, stillCached := bus.sticky["task-expired"]
	bus.mu.RUnlock()
	if stillCached {
		t.Error("expired sticky entry should be deleted on Subscribe, but remains in the cache")
	}
}

// TestPublishStillDeliversToLiveSubscribers is the no-regression guard: adding
// the sticky cache must not break the original live-fanout path. A subscriber
// present at Publish time must still receive the event on its channel.
func TestPublishStillDeliversToLiveSubscribers(t *testing.T) {
	bus := newTestBus(time.Now)

	ch, sticky := bus.Subscribe("task-live")
	defer bus.Unsubscribe("task-live", ch)
	if sticky != nil {
		t.Fatalf("fresh task should have no sticky, got %+v", sticky)
	}

	bus.Publish("task-live", StepEvent{
		TaskID: "task-live",
		Status: types.StatusCompleted,
		Final:  "ok",
	})

	select {
	case got := <-ch:
		if got.Status != types.StatusCompleted || got.Final != "ok" {
			t.Errorf("live event payload = %+v, want completed/ok", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("live subscriber did not receive the published terminal event")
	}
}

func TestTerminalStepEventIncludesAggregatedTokenUsage(t *testing.T) {
	task := &types.Task{
		ID:          "task-token-event",
		Status:      types.StatusCompleted,
		FinalAnswer: "done",
		Trace: []types.StepTrace{
			{TokenUsage: types.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
			{TokenUsage: types.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}},
		},
	}

	event := terminalStepEvent(task.ID, task)
	if event.TokenUsage == nil {
		t.Fatal("terminal event TokenUsage is nil")
	}
	if event.TokenUsage.PromptTokens != 17 ||
		event.TokenUsage.CompletionTokens != 8 ||
		event.TokenUsage.TotalTokens != 25 {
		t.Fatalf("token usage = %+v, want prompt=17 completion=8 total=25", event.TokenUsage)
	}
}
