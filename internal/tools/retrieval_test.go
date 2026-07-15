package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestRAGSearchCachesQueriesAndFetchesStableCandidates(t *testing.T) {
	taskID := "retrieval-cache-test"
	ClearRetrievalContext(taskID)
	t.Cleanup(func() { ClearRetrievalContext(taskID) })

	calls := 0
	search := &retrievalSearchTool{kind: "rag", deps: RetrievalDependencies{
		SearchRAG: func(_ context.Context, query string) ([]types.Memory, error) {
			calls++
			return []types.Memory{{Goal: "result " + query, KeyFindings: "details " + query}}, nil
		},
	}}
	fetch := &retrievalFetchTool{kind: "rag"}
	ctx := WithRetrievalExecutionContext(context.Background(), taskID, "default")

	first, err := search.Execute(ctx, "", map[string]any{"query": "alpha", "top_k": 1})
	if err != nil {
		t.Fatalf("first search: %v", err)
	}
	firstID := candidateID(t, first)

	repeated, err := search.Execute(ctx, "", map[string]any{"query": "  ALPHA  ", "top_k": 1})
	if err != nil {
		t.Fatalf("repeated search: %v", err)
	}
	if calls != 1 || !strings.Contains(repeated.Observation, `"cached":true`) {
		t.Fatalf("repeated query calls=%d observation=%s", calls, repeated.Observation)
	}

	second, err := search.Execute(ctx, "", map[string]any{"query": "beta", "top_k": 1})
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if secondID := candidateID(t, second); secondID == firstID {
		t.Fatalf("different queries reused candidate ID %q", firstID)
	}

	result, err := fetch.Execute(ctx, "", map[string]any{"ids": []any{firstID}})
	if err != nil {
		t.Fatalf("fetch first candidate after second search: %v", err)
	}
	if len(result.Evidence) != 1 || !strings.Contains(result.Evidence[0].Lines[0], "details alpha") {
		t.Fatalf("unexpected fetched evidence: %+v", result.Evidence)
	}
}

func TestRetrievalSearchEnforcesUniqueQueryCallLimit(t *testing.T) {
	taskID := "retrieval-limit-test"
	ClearRetrievalContext(taskID)
	t.Cleanup(func() { ClearRetrievalContext(taskID) })
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.JITSearchMaxCalls = 2
	}))

	calls := 0
	search := &retrievalSearchTool{kind: "rag", deps: RetrievalDependencies{
		SearchRAG: func(_ context.Context, query string) ([]types.Memory, error) {
			calls++
			return []types.Memory{{Goal: query, FinalAnswer: query}}, nil
		},
	}}
	ctx := WithRetrievalExecutionContext(context.Background(), taskID, "default")
	for _, query := range []string{"one", "two"} {
		if _, err := search.Execute(ctx, "", map[string]any{"query": query}); err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
	}
	if _, err := search.Execute(ctx, "", map[string]any{"query": "three"}); err == nil || !strings.Contains(err.Error(), "call limit") {
		t.Fatalf("third unique search error=%v, want call limit", err)
	}
	if calls != 2 {
		t.Fatalf("provider calls=%d, want 2", calls)
	}
}

func TestMemorySearchUsesEmbeddingAndStoreThenMemoryGet(t *testing.T) {
	taskID := "memory-search-test"
	ClearRetrievalContext(taskID)
	t.Cleanup(func() { ClearRetrievalContext(taskID) })

	store := &fakeMemoryQueryStore{memories: []*types.Memory{{TaskID: "old-task", Goal: "historic fact", FinalAnswer: "stored answer"}}}
	search := &retrievalSearchTool{kind: "memory", deps: RetrievalDependencies{
		GetEmbedding: func(_ context.Context, query string) ([]float32, error) {
			if query != "historic" {
				return nil, fmt.Errorf("unexpected query %q", query)
			}
			return []float32{1, 2, 3}, nil
		},
		MemoryStore: store,
	}}
	ctx := WithRetrievalExecutionContext(context.Background(), taskID, "tenant-a")
	result, err := search.Execute(ctx, "", map[string]any{"query": "historic", "top_k": 1})
	if err != nil {
		t.Fatalf("memory search: %v", err)
	}
	if store.query != "historic" || len(store.embedding) != 3 || store.limit != 1 {
		t.Fatalf("store call query=%q embedding=%v limit=%d", store.query, store.embedding, store.limit)
	}

	got, err := (&retrievalFetchTool{kind: "memory"}).Execute(ctx, "", map[string]any{"ids": []string{candidateID(t, result)}})
	if err != nil {
		t.Fatalf("memory get: %v", err)
	}
	if len(got.Evidence) != 1 || !strings.Contains(got.Evidence[0].Lines[0], "stored answer") {
		t.Fatalf("unexpected memory evidence: %+v", got.Evidence)
	}
}

func candidateID(t *testing.T, result *ToolResult) string {
	t.Helper()
	var payload struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(result.Observation), &payload); err != nil {
		t.Fatalf("decode search result: %v", err)
	}
	if len(payload.Results) != 1 || payload.Results[0].ID == "" {
		t.Fatalf("unexpected candidates: %s", result.Observation)
	}
	return payload.Results[0].ID
}

type fakeMemoryQueryStore struct {
	memories  []*types.Memory
	query     string
	embedding []float32
	limit     int
}

func (f *fakeMemoryQueryStore) QueryMemories(_ context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	f.query = query
	f.embedding = append([]float32(nil), embedding...)
	f.limit = limit
	return f.memories, nil
}
