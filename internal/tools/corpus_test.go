package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type corpusFixture struct{}

func (corpusFixture) CurrentWatermark(context.Context, WikiScope) (string, error) { return "wm-1", nil }

func (corpusFixture) SearchCorpus(context.Context, string, int, string, WikiScope) ([]wiki.Document, error) {
	return []wiki.Document{{URI: "wiki://brain/concepts/one", Score: 0.9}}, nil
}
func (corpusFixture) ReadCorpus(_ context.Context, document wiki.Document, _ string, _ WikiScope) (wiki.Document, error) {
	document.Content = "brain content"
	return document, nil
}

func TestCorpusRouterAllIsBoundedAndDeduplicated(t *testing.T) {
	router := &CorpusRouter{Ordinary: &fakeWikiReader{}, Brain: corpusFixture{}}
	documents, err := router.SearchAll(t.Context(), "q", 2, "space", WikiScope{TenantID: "tenant-a", BrainProjectID: "atlas", BrainSnapshotID: "snap-1"})
	if err != nil || len(documents) > 2 {
		t.Fatalf("documents = %#v, error = %v", documents, err)
	}
}

func TestWikiSearchSchemaAddsCorpusOnlyWhenConfigured(t *testing.T) {
	without := (&wikiSearchTool{client: &fakeWikiReader{}}).Parameters()
	if _, ok := without["corpus"]; ok {
		t.Fatal("disabled corpus unexpectedly changed schema")
	}
	with := (&wikiSearchTool{client: &fakeWikiReader{}, corpus: corpusFixture{}}).Parameters()
	if _, ok := with["corpus"]; !ok {
		t.Fatal("enabled corpus missing schema")
	}
}

func TestBrainWatermarkSeparatesWikiCacheScope(t *testing.T) {
	first, _ := retrievalExecutionFromContext(WithRetrievalExecutionContext(t.Context(), "task", "tenant", WithBrainScope("atlas", "snap"), WithBrainWatermark("wm-1")))
	second, _ := retrievalExecutionFromContext(WithRetrievalExecutionContext(t.Context(), "task", "tenant", WithBrainScope("atlas", "snap"), WithBrainWatermark("wm-2")))
	if wikiTaskKey(first) == wikiTaskKey(second) {
		t.Fatal("watermark did not isolate wiki cache scope")
	}
}

func TestWikiCacheCorpusPartitionsDoNotOverwrite(t *testing.T) {
	cache := newWikiCache()
	cache.replace("tenant\x00task", "wiki", []wikiCandidate{{ID: "wiki-1", Corpus: "wiki"}})
	cache.replace("tenant\x00task", "brain", []wikiCandidate{{ID: "brain-1", Corpus: "brain"}})
	selected, err := cache.selectCandidates("tenant\x00task", []string{"wiki-1", "brain-1"})
	if err != nil || len(selected) != 2 {
		t.Fatalf("selected = %#v, err=%v", selected, err)
	}
}

func TestCorpusRouterAllSearchFetchPreservesBothSources(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.Wiki.DefaultSpace = "tenant-a"
		cfg.Wiki.FetchMaxItems = 3
		cfg.Wiki.FetchMaxBytes = 4096
	}))
	ordinary := &fakeWikiReader{}
	router := &CorpusRouter{Ordinary: ordinary, Brain: corpusFixture{}, MaxMerge: 4}
	search := &wikiSearchTool{client: ordinary, corpus: router, cache: newWikiCache(), guard: &wikiBackendGuard{}}
	ctx := WithRetrievalExecutionContext(t.Context(), "all-task", "tenant-a", WithBrainScope("atlas", "snap-1"))
	result, err := search.Execute(ctx, "", map[string]any{"query": "q", "top_k": 4, "corpus": "all"})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Results []wikiCandidate `json:"results"`
	}
	if err := json.Unmarshal([]byte(result.Observation), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) < 2 {
		t.Fatalf("results = %#v", payload.Results)
	}
	fetch := &wikiFetchTool{client: ordinary, corpus: router, cache: search.cache, guard: &wikiBackendGuard{}}
	ids := []string{payload.Results[0].ID, payload.Results[1].ID}
	if _, err := fetch.Execute(ctx, "", map[string]any{"ids": ids}); err != nil {
		t.Fatal(err)
	}
}

type mixedGraphCorpus struct{ corpusFixture }

func (mixedGraphCorpus) BrainSpace(WikiScope) string { return "brain-atlas" }

func TestGraphFetchRoutesMixedOrdinaryAndBrainProvenance(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.Wiki.DefaultSpace = "tenant-a" }))
	ordinary := &fakeWikiReader{}
	fetch := &wikiGraphFetchTool{client: ordinary, corpus: mixedGraphCorpus{}, guard: &wikiBackendGuard{}}
	ctx := WithRetrievalExecutionContext(t.Context(), "graph-task", "tenant-a", WithBrainScope("atlas", "snap-1"))
	result, err := fetch.Execute(ctx, "", map[string]any{"uris": []any{"wiki://tenant-a/concepts/moe", "wiki://brain-atlas/concepts/one"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) != 2 {
		t.Fatalf("evidence = %#v", result.Evidence)
	}
}
