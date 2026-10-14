package api

import (
	"fmt"
	"github.com/wuxujun/ai-agent/internal/types"
	"testing"
	"time"
)

// Measures process-local fanout only, not HTTP/proxy or subscriber throughput.
func BenchmarkEventBusFanoutCapacity(b *testing.B) {
	for _, subscribers := range []int{1, 100, 1000} {
		for _, full := range []bool{false, true} {
			b.Run(fmt.Sprintf("subscribers_%d/full_%t", subscribers, full), func(b *testing.B) {
				bus := newTestBus(time.Now)
				channels := make([]chan StepEvent, subscribers)
				for i := range channels {
					channels[i], _ = bus.Subscribe("capacity-task")
				}
				b.Cleanup(func() {
					for _, ch := range channels {
						bus.Unsubscribe("capacity-task", ch)
					}
				})
				event := StepEvent{TaskID: "capacity-task", Status: types.StatusRunning, Token: "synthetic"}
				if full {
					for i := 0; i < 32; i++ {
						bus.Publish("capacity-task", event)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					bus.Publish("capacity-task", event)
					if !full {
						for _, ch := range channels {
							<-ch
						}
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(subscribers), "subscribers")
				for _, ch := range channels {
					want := 0
					if full {
						want = 32
					}
					if len(ch) != want {
						b.Fatal("fanout buffer contract changed")
					}
				}
			})
		}
	}
}
