package tools

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/wiki"
)

func byteCandidate(id string, size int) wikiCandidate {
	return wikiCandidate{ID: id, Document: wiki.Document{Content: strings.Repeat("x", size)}}
}

func TestWikiCacheByteBudgetLRU(t *testing.T) {
	before := CurrentWikiMetrics()
	now := time.Unix(100, 0)
	cache := newWikiCache()
	cache.now = func() time.Time { return now }
	cache.replace("tenant\x00a", "wiki", []wikiCandidate{byteCandidate("id", 4096)})
	size := cache.currentSizeBytes
	if size < 4096 {
		t.Fatalf("content not counted: %d", size)
	}
	cache.byteLimit = func() int64 { return size * 2 }
	now = now.Add(time.Second)
	cache.replace("tenant\x00b", "wiki", []wikiCandidate{byteCandidate("id", 4096)})
	now = now.Add(time.Second)
	if _, err := cache.selectCandidates("tenant\x00a", []string{"id"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	cache.replace("tenant\x00c", "wiki", []wikiCandidate{byteCandidate("id", 4096)})
	if cache.tasks["tenant\x00b"] != nil || cache.tasks["tenant\x00a"] == nil || cache.tasks["tenant\x00c"] == nil {
		t.Fatal("did not evict least recently accessed task")
	}
	if cache.currentSizeBytes > size*2 {
		t.Fatal("byte cap exceeded")
	}
	if got := CurrentWikiMetrics().CandidateCacheEvictedBytes - before.CandidateCacheEvictedBytes; got != size {
		t.Fatalf("evicted bytes=%d want=%d", got, size)
	}
	cache.release("tenant\x00a")
	cache.release("tenant\x00c")
	if cache.currentSizeBytes != 0 || CurrentWikiMetrics().CandidateCacheBytes != before.CandidateCacheBytes {
		t.Fatal("release leaked accounting")
	}
}

func TestWikiCacheByteBudgetReplacementExpiryAndOversize(t *testing.T) {
	cache := newWikiCache()
	now := time.Unix(100, 0)
	cache.now = func() time.Time { return now }
	cache.replace("tenant\x00a", "wiki", []wikiCandidate{byteCandidate("wiki", 2000)})
	first := cache.currentSizeBytes
	cache.replace("tenant\x00a", "brain", []wikiCandidate{byteCandidate("brain", 3000)})
	if cache.currentSizeBytes <= first+3000 {
		t.Fatal("corpus partition not counted")
	}
	both := cache.currentSizeBytes
	cache.replace("tenant\x00a", "brain", []wikiCandidate{byteCandidate("brain", 3000)})
	if cache.currentSizeBytes != both {
		t.Fatal("replacement double counted bytes")
	}
	cache.replace("tenant\x00a", "brain", nil)
	if cache.currentSizeBytes >= both {
		t.Fatal("replacement did not return bytes")
	}
	cache.byteLimit = func() int64 { return both }
	cache.replace("tenant\x00b", "wiki", []wikiCandidate{byteCandidate("big", int(both)*2)})
	if cache.tasks["tenant\x00b"] != nil || cache.tasks["tenant\x00a"] == nil {
		t.Fatal("oversize entry retained or evicted healthy tasks")
	}
	now = now.Add(cache.ttl + time.Second)
	_, _ = cache.selectCandidates("tenant\x00a", []string{"wiki"})
	if cache.currentSizeBytes != 0 || len(cache.tasks) != 0 {
		t.Fatal("expiry leaked bytes")
	}
	wikiCacheOwners.Lock()
	defer wikiCacheOwners.Unlock()
	for _, key := range []string{"tenant\x00a", "tenant\x00b"} {
		if _, ok := wikiCacheOwners.byTask[key][cache]; ok {
			t.Fatal("stale cache owner")
		}
	}
}

func TestWikiCacheByteBudgetShrinksAndConcurrentLifecycle(t *testing.T) {
	cache := newWikiCache()
	limit := int64(64 << 10)
	cache.byteLimit = func() int64 { return limit }
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("tenant\x00%d", i)
			cache.replace(key, "wiki", []wikiCandidate{byteCandidate("id", 4096)})
			cache.markFetched(key, []string{"id"})
			_, _ = cache.selectCandidates(key, []string{"id"})
		}(i)
	}
	wg.Wait()
	if cache.currentSizeBytes > limit {
		t.Fatal("concurrent writes exceeded budget")
	}
	limit = 1
	_, _ = cache.selectCandidates("unknown", []string{"id"})
	if cache.currentSizeBytes != 0 || len(cache.tasks) != 0 {
		t.Fatal("lowered budget not enforced")
	}
	if err := cache.reserveGraph("tiny"); err == nil {
		t.Fatal("metadata bypassed budget")
	}
	if cache.currentSizeBytes != 0 {
		t.Fatal("metadata leaked bytes")
	}
}

func TestWikiCacheOversizedSearchPreservesFullCache(t *testing.T) {
	cache := newWikiCache()
	cache.replace("healthy", "wiki", []wikiCandidate{byteCandidate("id", 4096)})
	size := cache.currentSizeBytes
	cache.byteLimit = func() int64 { return size }
	cache.maxTasks = 1
	if cache.replace("oversized", "wiki", []wikiCandidate{byteCandidate("large", int(size)*2)}) {
		t.Fatal("oversized result admitted")
	}
	if cache.tasks["healthy"] == nil || cache.currentSizeBytes != size {
		t.Fatal("oversized result displaced healthy cache")
	}
	cache.release("healthy")
}
