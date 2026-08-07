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

func TestMemoryStoreTaskSnapshotsAreDeepCloned(t *testing.T) {
	store := NewMemoryStore()
	task := &types.Task{ID: "clone", Trace: []types.StepTrace{{Evidence: []types.Evidence{{Lines: []string{"line"}}}}}, AnswerAudit: &types.AnswerAuditReport{Stages: []types.AnswerAuditStage{{Findings: []types.AnswerAuditFinding{{Detail: "detail"}}}}}}
	if err := store.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	task.Trace[0].Evidence[0].Lines[0] = "caller mutation"
	first, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Trace[0].Evidence[0].Lines[0] != "line" {
		t.Fatalf("save snapshot aliased caller: %+v", first.Trace)
	}
	first.AnswerAudit.Stages[0].Findings[0].Detail = "read mutation"
	second, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.AnswerAudit.Stages[0].Findings[0].Detail != "detail" {
		t.Fatalf("read snapshot aliased store: %+v", second.AnswerAudit)
	}
}

func TestMemoryRelevanceScoreFallsBackForMismatchedDimensions(t *testing.T) {
	queryEmbedding := make([]float32, 3072)
	mem := &types.Memory{
		Goal:      "inspect postgres configuration",
		Embedding: make([]float32, 128),
	}

	score, mismatch := memoryRelevanceScore("postgres", queryEmbedding, mem)
	if !mismatch {
		t.Fatal("expected embedding dimension mismatch")
	}
	if score <= 0 {
		t.Fatalf("keyword fallback score = %v, want > 0", score)
	}
}

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

func TestMemoryStoreDeduplicatesEquivalentCompletedTasks(t *testing.T) {
	m := NewMemoryStore()
	first := &types.Task{ID: "task-content-a", TenantID: "default", Goal: "最近有台风吗", Status: types.StatusCompleted, FinalAnswer: "有台风巴威"}
	second := &types.Task{ID: "task-content-b", TenantID: "default", Goal: "最近有台风吗", Status: types.StatusCompleted, FinalAnswer: "有台风巴威"}
	if err := m.SaveFullTask(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := m.SaveFullTask(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.memories) != 1 {
		t.Fatalf("equivalent tasks created %d memories, want 1", len(m.memories))
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

func TestMemoryStoreSessionScopeIsolation(t *testing.T) {
	st := NewMemoryStore()
	now := time.Now()
	for _, mem := range []*types.Memory{
		{ID: "a", TenantID: "tenant", SessionID: "session-a", TaskID: "task-a", Goal: "shared topic", Timestamp: now},
		{ID: "b", TenantID: "tenant", SessionID: "session-b", TaskID: "task-b", Goal: "shared topic", Timestamp: now},
	} {
		if err := st.SaveMemory(t.Context(), mem); err != nil {
			t.Fatal(err)
		}
	}
	ctx := WithSessionScope(WithTenantScope(t.Context(), "tenant"), "session-a")
	items, err := st.QueryMemories(ctx, "shared", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SessionID != "session-a" {
		t.Fatalf("session memory leakage: %+v", items)
	}
}

func TestMemoryStoreSessionSequenceIsAtomic(t *testing.T) {
	st := NewMemoryStore()
	if err := st.CreateSession(t.Context(), &types.Session{ID: "session", TenantID: "tenant", Title: "test"}); err != nil {
		t.Fatal(err)
	}
	const count = 20
	results := make(chan int64, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sequence, err := st.NextSessionTaskSequence(t.Context(), "session", "tenant")
			if err != nil {
				t.Errorf("allocate sequence: %v", err)
				return
			}
			results <- sequence
		}()
	}
	wg.Wait()
	close(results)
	seen := map[int64]bool{}
	for sequence := range results {
		seen[sequence] = true
	}
	if len(seen) != count {
		t.Fatalf("sequence values are not unique: %v", seen)
	}
}
