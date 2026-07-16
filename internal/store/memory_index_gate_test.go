package store

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestMemoryIndexGateAllowsOneConcurrentIndexer(t *testing.T) {
	var gate memoryIndexGate
	var started atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if gate.tryStart("mem-1") {
				started.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := started.Load(); got != 1 {
		t.Fatalf("concurrent indexers started=%d, want 1", got)
	}
	gate.done("mem-1")
	if !gate.tryStart("mem-1") {
		t.Fatal("memory ID should be claimable after indexing completes")
	}
}
