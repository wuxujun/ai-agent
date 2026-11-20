//go:build integration

package postgres_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/internal/storage/postgres"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/testdb"
	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// axis returns a unit vector along dimension i, optionally blended with dimension j.
func axis(i, j int, w float64) []float32 {
	v := make([]float32, models.EmbeddingDimensions)
	v[i] = float32(math.Sqrt(1 - w*w))
	if w != 0 {
		v[j] = float32(w)
	}
	return v
}

func item(uri string, kind models.DocumentKind, emb []float32, fetched time.Time) models.KnowledgeItem {
	content := "content of " + uri
	return models.KnowledgeItem{
		Document: models.Document{
			Source: "test", URI: uri, Kind: kind, Title: uri,
			Content: content, ContentHash: models.HashContent(content), FetchedAt: fetched,
		},
		Chunks:         []models.Chunk{{Ordinal: 0, Content: content, TokenCount: 3, Embedding: emb}},
		EmbeddingModel: "test-model",
		Confidence:     0.9,
	}
}

func accepted(at time.Time, expires *time.Time) models.ValidationResult {
	return models.ValidationResult{Status: models.StatusAccepted, Stage: models.StageIngest, Confidence: 0.9, RuleSet: "test", CheckedAt: at, ExpiresAt: expires}
}

func TestEmbeddingInsertAndCosineRetrieval(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	repo := postgres.NewRepository(pool)
	now := time.Now().UTC()
	expired := now.Add(-time.Minute)

	docs := []struct {
		uri  string
		kind models.DocumentKind
		emb  []float32
		res  models.ValidationResult
	}{
		{"doc://exact", models.KindDoc, axis(0, 1, 0), accepted(now, nil)},
		{"spec://near", models.KindAPISpec, axis(0, 1, 0.6), accepted(now, nil)}, // cos = 0.8
		{"doc://orthogonal", models.KindDoc, axis(1, 0, 0), accepted(now, nil)},  // cos = 0
		{"doc://expired", models.KindDoc, axis(0, 1, 0), accepted(now, &expired)},
	}
	for _, d := range docs {
		if _, err := repo.SaveKnowledge(ctx, item(d.uri, d.kind, d.emb, now), d.res); err != nil {
			t.Fatalf("save %s: %v", d.uri, err)
		}
	}

	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM embeddings WHERE vector_dims(embedding) = $1`, models.EmbeddingDimensions).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != len(docs) {
		t.Fatalf("stored embeddings = %d, want %d", stored, len(docs))
	}

	got, err := repo.SearchSimilar(ctx, axis(0, 1, 0), models.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	wantURIs := []string{"doc://exact", "spec://near", "doc://orthogonal"}
	wantDist := []float64{0, 0.2, 1}
	if len(got) != len(wantURIs) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(wantURIs), got)
	}
	for i, rc := range got {
		if rc.Document.URI != wantURIs[i] {
			t.Errorf("result %d uri = %s, want %s", i, rc.Document.URI, wantURIs[i])
		}
		if math.Abs(rc.Distance-wantDist[i]) > 1e-5 || math.Abs(rc.Similarity-(1-wantDist[i])) > 1e-5 {
			t.Errorf("result %d distance = %f similarity = %f, want distance %f", i, rc.Distance, rc.Similarity, wantDist[i])
		}
		if rc.Chunk.ID == "" || rc.Document.ID == "" || rc.Confidence != 0.9 {
			t.Errorf("result %d missing metadata: %+v", i, rc)
		}
	}

	filtered, err := repo.SearchSimilar(ctx, axis(0, 1, 0), models.SearchOptions{Limit: 10, MaxDistance: 0.5, Kinds: []models.DocumentKind{models.KindAPISpec}})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Document.URI != "spec://near" {
		t.Fatalf("filtered = %+v", filtered)
	}
}

func TestSaveKnowledgeIsIdempotentPerContent(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	repo := postgres.NewRepository(pool)
	now := time.Now().UTC()
	it := item("doc://same", models.KindDoc, axis(2, 0, 0), now)

	id1, err := repo.SaveKnowledge(ctx, it, accepted(now, nil))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := repo.SaveKnowledge(ctx, it, accepted(now.Add(time.Second), nil))
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("ids differ: %s vs %s", id1, id2)
	}
	var chunks, validations int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE document_id = $1`, id1).Scan(&chunks)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM validations WHERE document_id = $1`, id1).Scan(&validations)
	if chunks != 1 || validations != 2 {
		t.Fatalf("chunks=%d validations=%d", chunks, validations)
	}
}

func TestSaveKnowledgeRejectsUnaccepted(t *testing.T) {
	repo := postgres.NewRepository(testdb.New(t))
	_, err := repo.SaveKnowledge(context.Background(), item("doc://r", models.KindDoc, axis(0, 1, 0), time.Now()), models.ValidationResult{Status: models.StatusRejected})
	if err == nil {
		t.Fatal("expected error")
	}
}
