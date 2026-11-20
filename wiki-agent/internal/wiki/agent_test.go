package wiki

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/internal/validation"
	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

const testDims = 64

type memRepo struct {
	saved   []models.KnowledgeItem
	results []models.RetrievedChunk
}

func (m *memRepo) SaveKnowledge(_ context.Context, item models.KnowledgeItem, _ models.ValidationResult) (string, error) {
	m.saved = append(m.saved, item)
	return "doc-1", nil
}

func (m *memRepo) SearchSimilar(context.Context, []float32, models.SearchOptions) ([]models.RetrievedChunk, error) {
	return m.results, nil
}

func newTestAgent(repo Repository) *Agent {
	g := validation.NewGateway(validation.Config{MinConfidence: 0.7, MaxAge: time.Hour, EmbeddingDimensions: testDims})
	return NewAgent(repo, HashEmbedder{Dims: testDims}, ParagraphChunker{MaxChars: 40}, g, nil)
}

func TestIngestAcceptsAndChunks(t *testing.T) {
	repo := &memRepo{}
	res, err := newTestAgent(repo).Ingest(context.Background(), IngestRequest{
		Document:   models.Document{Source: "docs", URI: "doc://x", Kind: models.KindDoc, Content: "first paragraph here\n\nsecond paragraph is a bit longer"},
		Confidence: 0.9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.DocumentID != "doc-1" || !res.Validation.Accepted() {
		t.Fatalf("unexpected result: %+v", res)
	}
	item := repo.saved[0]
	if len(item.Chunks) != 2 || item.Document.ContentHash != models.HashContent(item.Document.Content) || len(item.Chunks[1].Embedding) != testDims {
		t.Fatalf("unexpected item: %+v", item)
	}
}

func TestIngestRejectsWithoutPersisting(t *testing.T) {
	repo := &memRepo{}
	res, err := newTestAgent(repo).Ingest(context.Background(), IngestRequest{
		Document:   models.Document{Source: "docs", URI: "doc://x", Kind: models.KindDoc, Content: "text"},
		Confidence: 0.2,
	})
	if !errors.Is(err, ErrRejected) || res.Validation.Accepted() || len(repo.saved) != 0 {
		t.Fatalf("err=%v res=%+v saved=%d", err, res, len(repo.saved))
	}
}

func TestRetrieveDropsUnverifiedChunks(t *testing.T) {
	fresh := models.Document{Source: "s", URI: "u", Kind: models.KindDoc, FetchedAt: time.Now()}
	stale := fresh
	stale.FetchedAt = time.Now().Add(-2 * time.Hour)
	repo := &memRepo{results: []models.RetrievedChunk{
		{Chunk: models.Chunk{ID: "ok", Content: "a"}, Document: fresh, Confidence: 0.9},
		{Chunk: models.Chunk{ID: "stale", Content: "b"}, Document: stale, Confidence: 0.9},
		{Chunk: models.Chunk{ID: "weak", Content: "c"}, Document: fresh, Confidence: 0.1},
	}}
	got, err := newTestAgent(repo).Retrieve(context.Background(), "q", models.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Chunk.ID != "ok" {
		t.Fatalf("got %+v", got)
	}
}

func TestSubmitFindings(t *testing.T) {
	repo := &memRepo{}
	now := time.Now()
	results, err := newTestAgent(repo).SubmitFindings(context.Background(), []models.Finding{
		{StepID: "s1", Source: "executor", URI: "finding://1", Content: "verified result", Confidence: 0.95, ObservedAt: now},
		{StepID: "s2", Source: "executor", URI: "finding://2", Content: "guess", Confidence: 0.3, ObservedAt: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || !results[0].Accepted() || results[1].Accepted() || len(repo.saved) != 1 {
		t.Fatalf("results=%+v saved=%d", results, len(repo.saved))
	}
	if repo.saved[0].Document.Kind != models.KindFinding || repo.saved[0].Document.Metadata["step_id"] != "s1" {
		t.Fatalf("unexpected saved finding: %+v", repo.saved[0].Document)
	}
}

func TestParagraphChunker(t *testing.T) {
	got := ParagraphChunker{MaxChars: 10}.Split("aaaa\n\nbbbb\n\n\n\ncccccccccccccc")
	want := []string{"aaaa\n\nbbbb", "cccccccccc", "cccc"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}
