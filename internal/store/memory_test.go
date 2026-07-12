package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// TestMemoryStoreDuplicateIndexing verifies that when SaveFullTask is called
// multiple times in parallel for a completed task, only one goroutine is
// spawned to generate the memory.
func TestMemoryStoreDuplicateIndexing(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()

	task := &types.Task{
		ID:          "task-indexing-dup-test",
		Goal:        "duplicate indexing goal",
		Status:      types.StatusCompleted,
		FinalAnswer: "completed!",
	}

	// We will call SaveFullTask concurrently N times.
	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)

	// Since memory.CreateMemoryFromTask generates embeddings locally,
	// it completes very fast. To ensure we have concurrent overlap and see
	// the indexing map in action, we run SaveFullTask concurrently.
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = m.SaveFullTask(ctx, task)
		}()
	}
	wg.Wait()

	// Wait briefly to make sure any spawned indexing goroutines complete.
	time.Sleep(100 * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()

	// There should be exactly one memory created.
	if len(m.memories) != 1 {
		t.Errorf("expected exactly 1 memory, got %d", len(m.memories))
	}

	// The indexing map must be empty after completion.
	if len(m.indexing) != 0 {
		t.Errorf("expected indexing map to be empty, got %v", m.indexing)
	}
}
