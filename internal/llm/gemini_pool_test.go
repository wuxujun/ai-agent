package llm

import (
	"container/list"
	"fmt"
	"testing"
)

func resetGeminiPool(t *testing.T) {
	geminiPool.Lock()
	oldLRU, oldIdx := geminiPool.lru, geminiPool.idx
	geminiPool.lru, geminiPool.idx = list.New(), make(map[string]*list.Element)
	geminiPool.Unlock()
	t.Cleanup(func() { geminiPool.Lock(); geminiPool.lru, geminiPool.idx = oldLRU, oldIdx; geminiPool.Unlock() })
}

func TestGeminiClientPoolBounded(t *testing.T) {
	resetGeminiPool(t)
	for i := 0; i < geminiClientCacheCapacity+3; i++ {
		if _, err := GetGeminiClient(fmt.Sprintf("key-%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	geminiPool.Lock()
	defer geminiPool.Unlock()
	if geminiPool.lru.Len() != geminiClientCacheCapacity || len(geminiPool.idx) != geminiClientCacheCapacity {
		t.Fatalf("pool size lru=%d index=%d", geminiPool.lru.Len(), len(geminiPool.idx))
	}
}
