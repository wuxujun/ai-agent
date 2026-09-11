package api

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestStickyCacheExpiresWithoutRevisitingTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bus := &EventBus{subs: make(map[string][]chan StepEvent), sticky: make(map[string]stickyEvent), nowFunc: time.Now}
		bus.Publish("old", StepEvent{TaskID: "old", Status: types.StatusCompleted})
		time.Sleep(2 * stickyTerminalTTL)
		synctest.Wait()
		bus.mu.Lock()
		count := len(bus.sticky)
		bus.mu.Unlock()
		if count != 0 {
			t.Fatalf("expired entries retained=%d", count)
		}
		bus.ForgetAll()
	})
}
func TestStickyCacheCapacityIsBounded(t *testing.T) {
	bus := &EventBus{subs: make(map[string][]chan StepEvent), sticky: make(map[string]stickyEvent), nowFunc: time.Now}
	defer bus.ForgetAll()
	for i := 0; i < 1100; i++ {
		id := fmt.Sprint(i)
		bus.Publish(id, StepEvent{TaskID: id, Status: types.StatusCompleted})
	}
	if len(bus.sticky) > 1024 {
		t.Fatalf("sticky entries=%d", len(bus.sticky))
	}
	ch, latest := bus.Subscribe("1099")
	defer bus.Unsubscribe("1099", ch)
	if latest == nil {
		t.Fatal("latest terminal event not replayable")
	}
}
