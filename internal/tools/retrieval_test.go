package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	state := RetrievalStateForTask(taskID)
	if state.NetworkSearchCalls["rag"] != 1 || state.RetrievalCycles["rag"] != 1 || len(state.Searches) != 1 {
		t.Fatalf("equivalent query created another retrieval cycle: %+v", state)
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

func TestRetrievalKeysEquivalentDetectsSemanticQueryVariants(t *testing.T) {
	tests := []struct {
		left, right string
		want        bool
	}{
		{"rag:数学科学术顾问有哪些", "rag:数学 学术顾问", false},
		{"rag:数学学术顾问有哪些?", "rag:数学 学术顾问", true},
		{"rag:学术顾问有哪些?", "rag:学术顾问 成员 姓名 老师 团队", false},
		{"rag:数学科学术顾问有哪些", "rag:数学科学术顾问 数学 成员 老师 顾问名单", false},
		{"rag:数学科学术顾问有哪些", "rag:最近台风路径", false},
		{"memory:数学科学术顾问", "rag:数学科学术顾问", false},
	}
	for _, tc := range tests {
		if got := retrievalKeysEquivalent(tc.left, tc.right, "rag"); got != tc.want {
			t.Errorf("retrievalKeysEquivalent(%q, %q)=%v, want %v", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestRetrievalSearchEnforcesUniqueQueryCallLimit(t *testing.T) {
	taskID := "retrieval-limit-test"
	ClearRetrievalContext(taskID)
	t.Cleanup(func() { ClearRetrievalContext(taskID) })
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.JITSearchMaxCalls = 2
		cfg.RAG.JITRetrievalMaxCycles = 10
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

func TestRetrievalSearchEnforcesCycleLimitSeparatelyFromNetworkLimit(t *testing.T) {
	taskID := "retrieval-cycle-limit-test"
	ClearRetrievalContext(taskID)
	t.Cleanup(func() { ClearRetrievalContext(taskID) })
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.JITSearchMaxCalls = 10
		cfg.RAG.JITRetrievalMaxCycles = 2
	}))
	search := &retrievalSearchTool{kind: "rag", deps: RetrievalDependencies{SearchRAG: func(_ context.Context, query string) ([]types.Memory, error) {
		return []types.Memory{{Goal: query, FinalAnswer: query}}, nil
	}}}
	ctx := WithRetrievalExecutionContext(context.Background(), taskID, "default")
	for _, query := range []string{"alpha", "beta"} {
		if _, err := search.Execute(ctx, "", map[string]any{"query": query}); err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
	}
	if _, err := search.Execute(ctx, "", map[string]any{"query": "gamma"}); err == nil || !strings.Contains(err.Error(), "cycle limit") {
		t.Fatalf("third cycle error=%v, want cycle limit", err)
	}
	state := RetrievalStateForTask(taskID)
	if state.NetworkSearchCalls["rag"] != 2 || state.RetrievalCycles["rag"] != 2 {
		t.Fatalf("unexpected separate counters: %+v", state)
	}
}

func TestRetrievalFetchReturnsOnlyPreviouslyUnfetchedCandidates(t *testing.T) {
	taskID := "retrieval-fetch-once-test"
	ClearRetrievalContext(taskID)
	t.Cleanup(func() { ClearRetrievalContext(taskID) })
	candidate := retrievalCandidate{ID: "rag-once", Kind: "rag", Memory: types.Memory{KeyFindings: "evidence"}}
	defaultRetrievalCache.completeSearch(taskID, "rag:query", &retrievalSearchFlight{done: make(chan struct{})}, []retrievalCandidate{candidate}, nil)
	ctx := WithRetrievalExecutionContext(context.Background(), taskID, "default")
	tool := &retrievalFetchTool{kind: "rag"}
	first, err := tool.Execute(ctx, "", map[string]any{"ids": []string{candidate.ID}})
	if err != nil || len(first.Evidence) != 1 {
		t.Fatalf("first fetch result=%+v err=%v", first, err)
	}
	second, err := tool.Execute(ctx, "", map[string]any{"ids": []string{candidate.ID}})
	if err != nil || len(second.Evidence) != 0 {
		t.Fatalf("duplicate fetch result=%+v err=%v", second, err)
	}
	state := RetrievalStateForTask(taskID)
	if len(state.Searches) != 1 || len(state.Searches[0].FetchedIDs) != 1 || len(state.Searches[0].PendingIDs) != 0 {
		t.Fatalf("unexpected fetch state: %+v", state)
	}
}

func TestRetrievalSearchCoalescesConcurrentIdenticalQueries(t *testing.T) {
	taskID := "retrieval-singleflight-test"
	ClearRetrievalContext(taskID)
	t.Cleanup(func() { ClearRetrievalContext(taskID) })

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	search := &retrievalSearchTool{kind: "rag", deps: RetrievalDependencies{
		SearchRAG: func(_ context.Context, query string) ([]types.Memory, error) {
			calls.Add(1)
			startOnce.Do(func() { close(started) })
			<-release
			return []types.Memory{{Goal: query, FinalAnswer: "one provider response"}}, nil
		},
	}}
	ctx := WithRetrievalExecutionContext(context.Background(), taskID, "default")
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := search.Execute(ctx, "", map[string]any{"query": "same query"})
			results <- err
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent search: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls=%d, want one coalesced call", got)
	}
}

func TestRetrievalFetchUsesConfiguredIndependentByteBudgets(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.JITRAGFetchMaxBytes = 80
		cfg.RAG.JITMemoryFetchMaxBytes = 40
	}))
	for _, tc := range []struct {
		kind  string
		limit int
	}{
		{kind: "rag", limit: 80},
		{kind: "memory", limit: 40},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			taskID := "retrieval-budget-" + tc.kind
			ClearRetrievalContext(taskID)
			t.Cleanup(func() { ClearRetrievalContext(taskID) })
			candidate := retrievalCandidate{ID: tc.kind + "-candidate", Kind: tc.kind, Memory: types.Memory{KeyFindings: strings.Repeat("资料", 100)}}
			defaultRetrievalCache.completeSearch(taskID, tc.kind+":query", &retrievalSearchFlight{done: make(chan struct{})}, []retrievalCandidate{candidate}, nil)
			ctx := WithRetrievalExecutionContext(context.Background(), taskID, "default")
			result, err := (&retrievalFetchTool{kind: tc.kind}).Execute(ctx, "", map[string]any{"ids": []string{candidate.ID}})
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if len(result.Evidence) != 1 || len(result.Evidence[0].Lines[0]) > tc.limit {
				t.Fatalf("evidence bytes=%d, limit=%d", len(result.Evidence[0].Lines[0]), tc.limit)
			}
		})
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
