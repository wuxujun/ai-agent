package planner

import (
	"container/list"
	"context"
	"net/http"
	"sync"

	"google.golang.org/genai"
)

// geminiClientCacheCapacity bounds how many distinct (apiKey, baseURL) gemini
// clients are kept alive at once. Prior to this cap the map was unbounded: each
// hot-reloaded API key rotation leaked another client, since *genai.Client has
// no Close method to release on eviction. 8 is enough room for multi-tenant
// fan-out across a handful of keys without keeping every historical key alive.
const geminiClientCacheCapacity = 8

var (
	geminiClientsMu  sync.Mutex
	geminiClientLRU  = list.New()                        // front = MRU, back = LRU
	geminiClientIdx  = make(map[string]*list.Element, 8) // key → list element for O(1) lookup
	geminiHTTPClient *http.Client
)

// geminiClientEntry is the list element value. We carry the key alongside the
// client so eviction (back of list) can drop the matching index entry without
// scanning.
type geminiClientEntry struct {
	key    string
	client *genai.Client
}

// GetGeminiClient returns a bounded, LRU-cached gemini client for the given
// (apiKey, baseURL) pair. Successful lookups promote the entry to MRU; on miss
// a new client is constructed and inserted at the front, evicting the LRU
// entry if the cache is at capacity.
func GetGeminiClient(apiKey, baseURL string) (*genai.Client, error) {
	key := apiKey + "|" + baseURL

	geminiClientsMu.Lock()
	if el, ok := geminiClientIdx[key]; ok {
		geminiClientLRU.MoveToFront(el)
		client := el.Value.(*geminiClientEntry).client
		geminiClientsMu.Unlock()
		return client, nil
	}
	geminiClientsMu.Unlock()

	opts := &genai.ClientConfig{APIKey: apiKey}
	if baseURL != "" {
		opts.HTTPOptions = genai.HTTPOptions{BaseURL: baseURL}
	}
	if geminiHTTPClient != nil {
		opts.HTTPClient = geminiHTTPClient
	}
	client, err := genai.NewClient(context.Background(), opts)
	if err != nil {
		return nil, err
	}

	geminiClientsMu.Lock()
	defer geminiClientsMu.Unlock()

	// Re-check: another goroutine may have inserted while we were constructing.
	// Prefer the existing client and discard the one we just built (no Close
	// on *genai.Client; GC reclaims it).
	if el, ok := geminiClientIdx[key]; ok {
		geminiClientLRU.MoveToFront(el)
		return el.Value.(*geminiClientEntry).client, nil
	}

	el := geminiClientLRU.PushFront(&geminiClientEntry{key: key, client: client})
	geminiClientIdx[key] = el

	for geminiClientLRU.Len() > geminiClientCacheCapacity {
		oldest := geminiClientLRU.Back()
		if oldest == nil {
			break
		}
		geminiClientLRU.Remove(oldest)
		delete(geminiClientIdx, oldest.Value.(*geminiClientEntry).key)
	}

	return client, nil
}
