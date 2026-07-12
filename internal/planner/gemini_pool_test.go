package planner

import (
	"container/list"
	"fmt"
	"testing"
)

// TestGeminiProviderIsRegistered guards the registry wiring for #4: PlanNext
// resolves providers through providerRegistry, so a Gemini lookup miss would
// silently fall back to "unsupported provider" instead of hitting the SDK
// path.
func TestGeminiProviderIsRegistered(t *testing.T) {
	p, err := lookupProvider(ProviderGemini)
	if err != nil {
		t.Fatalf("expected gemini provider in registry, got error: %v", err)
	}
	if p.Name() != ProviderGemini {
		t.Errorf("provider Name() = %q, want %q", p.Name(), ProviderGemini)
	}
	if _, ok := p.(*geminiProvider); !ok {
		t.Errorf("expected *geminiProvider, got %T", p)
	}
}

func TestLiteLLMProviderIsRegistered(t *testing.T) {
	p, err := lookupProvider(ProviderLiteLLM)
	if err != nil {
		t.Fatalf("expected litellm provider in registry: %v", err)
	}
	if p.Name() != ProviderLiteLLM {
		t.Fatalf("provider name = %q, want %q", p.Name(), ProviderLiteLLM)
	}
}

// resetGeminiPool snapshots the LRU + index state, clears them for the test,
// and restores them on cleanup. Required because the pool is package-global
// and other tests (TestLLMPlannerProviders/gemini) may have already populated
// it.
func resetGeminiPool(t *testing.T) {
	t.Helper()
	geminiClientsMu.Lock()
	prevLRU := geminiClientLRU
	prevIdx := geminiClientIdx
	geminiClientLRU = list.New()
	geminiClientIdx = make(map[string]*list.Element, geminiClientCacheCapacity)
	geminiClientsMu.Unlock()

	t.Cleanup(func() {
		geminiClientsMu.Lock()
		geminiClientLRU = prevLRU
		geminiClientIdx = prevIdx
		geminiClientsMu.Unlock()
	})
}

// TestGeminiClientPoolEviction covers #6: with capacity N, inserting N+k
// distinct (apiKey,baseURL) keys must keep exactly N entries and evict the
// oldest ones in LRU order. Before the bound, every rotated API key leaked
// another *genai.Client forever.
func TestGeminiClientPoolEviction(t *testing.T) {
	resetGeminiPool(t)

	// Fill past capacity. genai.NewClient does not validate the key at
	// construction time, so synthetic test-only keys are fine.
	total := geminiClientCacheCapacity + 3
	for i := 0; i < total; i++ {
		if _, err := GetGeminiClient(fmt.Sprintf("key-%d", i), ""); err != nil {
			t.Fatalf("GetGeminiClient(key-%d) failed: %v", i, err)
		}
	}

	geminiClientsMu.Lock()
	defer geminiClientsMu.Unlock()

	if got := geminiClientLRU.Len(); got != geminiClientCacheCapacity {
		t.Errorf("LRU length = %d, want %d", got, geminiClientCacheCapacity)
	}
	if got := len(geminiClientIdx); got != geminiClientCacheCapacity {
		t.Errorf("index length = %d, want %d", got, geminiClientCacheCapacity)
	}

	// First (total-capacity) keys must be gone; the last `capacity` must remain.
	for i := 0; i < total-geminiClientCacheCapacity; i++ {
		key := fmt.Sprintf("key-%d|", i)
		if _, ok := geminiClientIdx[key]; ok {
			t.Errorf("expected %s evicted, but still in cache", key)
		}
	}
	for i := total - geminiClientCacheCapacity; i < total; i++ {
		key := fmt.Sprintf("key-%d|", i)
		if _, ok := geminiClientIdx[key]; !ok {
			t.Errorf("expected %s in cache, but missing", key)
		}
	}
}

// TestGeminiClientPoolMRUPromotion verifies that re-fetching an existing key
// moves it to the front of the LRU, so a steadily-used key is never evicted
// when fresh keys arrive.
func TestGeminiClientPoolMRUPromotion(t *testing.T) {
	resetGeminiPool(t)

	// Insert exactly capacity entries.
	for i := 0; i < geminiClientCacheCapacity; i++ {
		if _, err := GetGeminiClient(fmt.Sprintf("key-%d", i), ""); err != nil {
			t.Fatalf("seed GetGeminiClient(key-%d): %v", i, err)
		}
	}

	// Touch key-0 — it becomes MRU. The next insertion should now evict key-1
	// (the new LRU), not key-0.
	if _, err := GetGeminiClient("key-0", ""); err != nil {
		t.Fatalf("re-fetch key-0: %v", err)
	}
	if _, err := GetGeminiClient("key-new", ""); err != nil {
		t.Fatalf("insert key-new: %v", err)
	}

	geminiClientsMu.Lock()
	defer geminiClientsMu.Unlock()

	if _, ok := geminiClientIdx["key-0|"]; !ok {
		t.Errorf("key-0 was promoted but got evicted")
	}
	if _, ok := geminiClientIdx["key-1|"]; ok {
		t.Errorf("expected key-1 evicted (it was LRU after promoting key-0); still in cache")
	}
	if _, ok := geminiClientIdx["key-new|"]; !ok {
		t.Errorf("key-new was not inserted")
	}

	// Cache is still bounded.
	if got := geminiClientLRU.Len(); got != geminiClientCacheCapacity {
		t.Errorf("LRU length = %d, want %d", got, geminiClientCacheCapacity)
	}
}
