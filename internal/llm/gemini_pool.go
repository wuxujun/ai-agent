package llm

import (
	"container/list"
	"context"
	"sync"

	"github.com/wuxujun/ai-agent/internal/telemetry"
	"google.golang.org/genai"
)

const geminiClientCacheCapacity = 8

type geminiClientEntry struct {
	key    string
	client *genai.Client
}

var geminiPool = struct {
	sync.Mutex
	lru *list.List
	idx map[string]*list.Element
}{lru: list.New(), idx: make(map[string]*list.Element, geminiClientCacheCapacity)}

func GetGeminiClient(apiKey, baseURL string) (*genai.Client, error) {
	key := apiKey + "|" + baseURL
	geminiPool.Lock()
	if item, ok := geminiPool.idx[key]; ok {
		geminiPool.lru.MoveToFront(item)
		client := item.Value.(*geminiClientEntry).client
		geminiPool.Unlock()
		return client, nil
	}
	geminiPool.Unlock()
	opts := &genai.ClientConfig{APIKey: apiKey, HTTPClient: telemetry.InstrumentHTTPClient(nil)}
	if baseURL != "" {
		opts.HTTPOptions = genai.HTTPOptions{BaseURL: baseURL}
	}
	client, err := genai.NewClient(context.Background(), opts)
	if err != nil {
		return nil, err
	}
	geminiPool.Lock()
	defer geminiPool.Unlock()
	if item, ok := geminiPool.idx[key]; ok {
		geminiPool.lru.MoveToFront(item)
		return item.Value.(*geminiClientEntry).client, nil
	}
	item := geminiPool.lru.PushFront(&geminiClientEntry{key: key, client: client})
	geminiPool.idx[key] = item
	for geminiPool.lru.Len() > geminiClientCacheCapacity {
		oldest := geminiPool.lru.Back()
		geminiPool.lru.Remove(oldest)
		delete(geminiPool.idx, oldest.Value.(*geminiClientEntry).key)
	}
	return client, nil
}
