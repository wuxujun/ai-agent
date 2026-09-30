package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResizableSemaphoreCancellationDoesNotLeakCapacity(t *testing.T) {
	sem := newResizableSemaphore(1)
	if !sem.Acquire(context.Background()) {
		t.Fatal("initial acquire failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sem.Acquire(ctx) {
				sem.Release()
			}
		}()
	}
	time.Sleep(time.Millisecond)
	cancel()
	sem.Release()
	wg.Wait()

	sem.mu.Lock()
	current := sem.current
	sem.mu.Unlock()
	if current != 0 {
		t.Fatalf("semaphore current leaked: got %d, want 0", current)
	}
}

func TestResizableSemaphoreResizeLimitsFutureConcurrency(t *testing.T) {
	sem := newResizableSemaphore(10)
	for i := 0; i < 10; i++ {
		if !sem.Acquire(context.Background()) {
			t.Fatalf("acquire %d failed", i)
		}
	}
	sem.Resize(3)
	for i := 0; i < 10; i++ {
		sem.Release()
	}

	var active int64
	var peak int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sem.Acquire(context.Background()) {
				return
			}
			cur := atomic.AddInt64(&active, 1)
			for {
				old := atomic.LoadInt64(&peak)
				if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&active, -1)
			sem.Release()
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&peak); got > 3 {
		t.Fatalf("peak concurrency = %d, want <= 3", got)
	}
}
