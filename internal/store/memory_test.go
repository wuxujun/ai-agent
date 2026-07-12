package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/wuxujun/ai-agent/internal/config"
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

func TestMemoryStoreTimeDecay(t *testing.T) {
	// Ensure config is loaded and defaults are initialized
	_ = config.Get()

	m := NewMemoryStore()
	ctx := context.Background()

	// Backup original configuration values
	originalDecay := viper.Get("store.memory_decay_rate")
	t.Cleanup(func() {
		viper.Set("store.memory_decay_rate", originalDecay)
		_, _, _ = config.Reload()
	})

	now := time.Now()
	// mem1: old but perfect match
	mem1 := &types.Memory{
		ID:          "mem-old-match",
		TaskID:      "task-old",
		Goal:        "postgres",
		FinalAnswer: "postgres config",
		Timestamp:   now.Add(-100 * time.Hour), // 100 hours ago
		Embedding:   []float32{1.0, 0.0, 0.0},
	}
	// mem2: new but weaker match
	mem2 := &types.Memory{
		ID:          "mem-new-weak",
		TaskID:      "task-new",
		Goal:        "mysql",
		FinalAnswer: "mysql config",
		Timestamp:   now, // brand new
		Embedding:   []float32{0.5, 0.5, 0.0},
	}

	_ = m.SaveMemory(ctx, mem1)
	_ = m.SaveMemory(ctx, mem2)

	queryEmb := []float32{1.0, 0.0, 0.0}

	// 1. With decay rate = 0.0, the perfect match mem1 should rank first.
	viper.Set("store.memory_decay_rate", 0.0)
	if _, _, err := config.Reload(); err != nil {
		t.Fatalf("config reload failed: %v", err)
	}

	res, err := m.QueryMemories(ctx, "postgres", queryEmb, 2)
	if err != nil {
		t.Fatalf("QueryMemories failed: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(res))
	}
	if res[0].ID != "mem-old-match" {
		t.Errorf("expected mem-old-match first when decay is disabled, got %s", res[0].ID)
	}

	// 2. With decay rate = 0.05, mem1's score will decay by exp(-0.05 * 100) = exp(-5) = ~0.0067.
	// mem2's score remains at cosine similarity (0.5). So mem2 should rank first!
	viper.Set("store.memory_decay_rate", 0.05)
	if _, _, err := config.Reload(); err != nil {
		t.Fatalf("config reload failed: %v", err)
	}

	res2, err := m.QueryMemories(ctx, "postgres", queryEmb, 2)
	if err != nil {
		t.Fatalf("QueryMemories failed: %v", err)
	}
	if len(res2) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(res2))
	}
	if res2[0].ID != "mem-new-weak" {
		t.Errorf("expected mem-new-weak first when decay is enabled, got %s", res2[0].ID)
	}
}

