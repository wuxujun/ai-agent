package tools

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestWikiCacheDoesNotCreateOnUnknownSelection(t *testing.T) {
	cache := newWikiCache()
	if _, err := cache.selectCandidates("tenant\x00missing", []string{"wiki-1"}); err == nil {
		t.Fatal("expected unknown candidate error")
	}
	if len(cache.tasks) != 0 {
		t.Fatalf("unknown selection created %d cache entries", len(cache.tasks))
	}
}

func TestWikiCacheExpiresAndEvictsOldest(t *testing.T) {
	now := time.Unix(100, 0)
	cache := newWikiCache()
	cache.now = func() time.Time { return now }
	cache.maxTasks = 2
	cache.ttl = 10 * time.Second
	cache.replace("tenant\x00a", nil)
	now = now.Add(time.Second)
	cache.replace("tenant\x00b", nil)
	now = now.Add(time.Second)
	cache.replace("tenant\x00c", nil)
	if cache.tasks["tenant\x00a"] != nil || len(cache.tasks) != 2 {
		t.Fatalf("capacity eviction left keys: %#v", cache.tasks)
	}
	now = now.Add(11 * time.Second)
	cache.replace("tenant\x00d", nil)
	if len(cache.tasks) != 1 || cache.tasks["tenant\x00d"] == nil {
		t.Fatalf("TTL pruning left keys: %#v", cache.tasks)
	}
	cache.release("tenant\x00d")
}

func TestReleaseWikiTaskCacheIsTenantScopedAndIdempotent(t *testing.T) {
	before := CurrentWikiMetrics()
	cache := newWikiCache()
	cache.replace("tenant-a\x00task", nil)
	cache.replace("tenant-b\x00task", nil)
	ReleaseWikiTaskCache("task", "tenant-a")
	ReleaseWikiTaskCache("task", "tenant-a")
	if cache.tasks["tenant-a\x00task"] != nil || cache.tasks["tenant-b\x00task"] == nil {
		t.Fatalf("tenant-scoped release left keys: %#v", cache.tasks)
	}
	cache.release("tenant-b\x00task")
	after := CurrentWikiMetrics()
	if after.CandidateCacheTasks != before.CandidateCacheTasks || after.CandidateCacheReleased-before.CandidateCacheReleased != 2 {
		t.Fatalf("unexpected cache metrics before=%+v after=%+v", before, after)
	}
}

func TestWikiCacheConcurrentLifecycle(t *testing.T) {
	cache := newWikiCache()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			key := fmt.Sprintf("tenant\x00task-%d", index%4)
			cache.replace(key, []wikiCandidate{{ID: "candidate"}})
			_, _ = cache.selectCandidates(key, []string{"candidate"})
			cache.release(key)
		}(i)
	}
	wg.Wait()
	for key := range cache.tasks {
		cache.release(key)
	}
}
