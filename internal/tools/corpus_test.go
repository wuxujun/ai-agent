package tools

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/wiki"
)

type corpusFixture struct{}

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
