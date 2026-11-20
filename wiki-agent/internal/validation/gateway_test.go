package validation

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

const dims = 4

var now = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func newGateway(opts ...Option) *Gateway {
	return NewGateway(Config{MinConfidence: 0.7, MaxAge: 24 * time.Hour, EmbeddingDimensions: dims},
		append([]Option{WithClock(func() time.Time { return now })}, opts...)...)
}

func validItem() models.KnowledgeItem {
	content := "GET /users returns a list of users."
	return models.KnowledgeItem{
		Document: models.Document{
			Source: "openapi", URI: "spec://users", Kind: models.KindAPISpec,
			Content: content, ContentHash: models.HashContent(content), FetchedAt: now.Add(-time.Hour),
		},
		Chunks:         []models.Chunk{{Ordinal: 0, Content: content, Embedding: []float32{1, 0, 0, 0}}},
		EmbeddingModel: "test",
		Confidence:     0.9,
	}
}

func TestGatewayIngest(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*models.KnowledgeItem)
		rules  []string
	}{
		{name: "valid", mutate: func(*models.KnowledgeItem) {}},
		{name: "missing source", mutate: func(i *models.KnowledgeItem) { i.Document.Source = "" }, rules: []string{"schema"}},
		{name: "invalid kind", mutate: func(i *models.KnowledgeItem) { i.Document.Kind = "blog" }, rules: []string{"schema"}},
		{name: "hash mismatch", mutate: func(i *models.KnowledgeItem) { i.Document.ContentHash = "x" }, rules: []string{"schema"}},
		{name: "no chunks", mutate: func(i *models.KnowledgeItem) { i.Chunks = nil }, rules: []string{"schema"}},
		{name: "missing embedding model", mutate: func(i *models.KnowledgeItem) { i.EmbeddingModel = "" }, rules: []string{"schema"}},
		{name: "wrong dimensions", mutate: func(i *models.KnowledgeItem) { i.Chunks[0].Embedding = []float32{1, 0} }, rules: []string{"schema"}},
		{name: "zero vector", mutate: func(i *models.KnowledgeItem) { i.Chunks[0].Embedding = make([]float32, dims) }, rules: []string{"schema"}},
		{name: "NaN embedding", mutate: func(i *models.KnowledgeItem) {
			i.Chunks[0].Embedding = []float32{float32(math.NaN()), 0, 0, 0}
		}, rules: []string{"schema"}},
		{name: "duplicate ordinal", mutate: func(i *models.KnowledgeItem) {
			i.Chunks = append(i.Chunks, models.Chunk{Ordinal: 0, Content: "x", Embedding: []float32{0, 1, 0, 0}})
		}, rules: []string{"schema"}},
		{name: "stale", mutate: func(i *models.KnowledgeItem) { i.Document.FetchedAt = now.Add(-48 * time.Hour) }, rules: []string{"freshness"}},
		{name: "future dated", mutate: func(i *models.KnowledgeItem) { i.Document.FetchedAt = now.Add(time.Hour) }, rules: []string{"freshness"}},
		{name: "within clock skew", mutate: func(i *models.KnowledgeItem) { i.Document.FetchedAt = now.Add(30 * time.Second) }},
		{name: "low confidence", mutate: func(i *models.KnowledgeItem) { i.Confidence = 0.5 }, rules: []string{"confidence"}},
		{name: "confidence at threshold", mutate: func(i *models.KnowledgeItem) { i.Confidence = 0.7 }},
		{name: "confidence out of range", mutate: func(i *models.KnowledgeItem) { i.Confidence = 1.5 }, rules: []string{"confidence"}},
		{name: "multiple failures", mutate: func(i *models.KnowledgeItem) {
			i.Document.URI = ""
			i.Confidence = 0.1
			i.Document.FetchedAt = now.Add(-72 * time.Hour)
		}, rules: []string{"schema", "freshness", "confidence"}},
	}
	g := newGateway()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := validItem()
			tt.mutate(&item)
			res := g.Validate(context.Background(), item, models.StageIngest)

			if res.Stage != models.StageIngest || res.RuleSet != DefaultRuleSet || !res.CheckedAt.Equal(now) {
				t.Fatalf("unexpected metadata: %+v", res)
			}
			if got := violatedRules(res); strings.Join(got, ",") != strings.Join(tt.rules, ",") {
				t.Fatalf("violated rules = %v, want %v (%+v)", got, tt.rules, res.Violations)
			}
			if wantAccepted := len(tt.rules) == 0; res.Accepted() != wantAccepted {
				t.Fatalf("accepted = %v, want %v", res.Accepted(), wantAccepted)
			}
		})
	}
}

func TestGatewayExpiresAt(t *testing.T) {
	item := validItem()
	res := newGateway().Validate(context.Background(), item, models.StageIngest)
	if res.ExpiresAt == nil || !res.ExpiresAt.Equal(item.Document.FetchedAt.Add(24*time.Hour)) {
		t.Fatalf("ExpiresAt = %v", res.ExpiresAt)
	}
	noAge := NewGateway(Config{MinConfidence: 0.5, EmbeddingDimensions: dims}, WithClock(func() time.Time { return now }))
	item.Document.FetchedAt = now.Add(-365 * 24 * time.Hour)
	if res := noAge.Validate(context.Background(), item, models.StageIngest); !res.Accepted() || res.ExpiresAt != nil {
		t.Fatalf("MaxAge=0 should disable freshness: %+v", res)
	}
}

func TestGatewayValidateRetrieved(t *testing.T) {
	g := newGateway()
	rc := models.RetrievedChunk{
		Chunk:      models.Chunk{ID: "c1", Content: "text"},
		Document:   models.Document{Source: "s", URI: "u", Kind: models.KindDoc, FetchedAt: now.Add(-time.Hour)},
		Confidence: 0.8,
	}
	if res := g.ValidateRetrieved(context.Background(), rc); !res.Accepted() || res.Stage != models.StageEmit {
		t.Fatalf("expected accepted emit result, got %+v", res)
	}

	stale := rc
	stale.Document.FetchedAt = now.Add(-25 * time.Hour)
	if res := g.ValidateRetrieved(context.Background(), stale); res.Accepted() {
		t.Fatal("stale chunk must not be emitted")
	}

	missingID := rc
	missingID.Chunk.ID = ""
	if res := g.ValidateRetrieved(context.Background(), missingID); res.Accepted() {
		t.Fatal("chunk without id must not be emitted")
	}
}

type denyRule struct{}

func (denyRule) Name() string { return "deny" }
func (denyRule) Check(models.KnowledgeItem, models.ValidationStage, time.Time) []models.Violation {
	return []models.Violation{{Rule: "deny", Message: "denied"}}
}

func TestGatewayCustomRulesAndCancellation(t *testing.T) {
	g := newGateway(WithRules(denyRule{}), WithRuleSet("custom"))
	res := g.Validate(context.Background(), validItem(), models.StageIngest)
	if res.Accepted() || res.RuleSet != "custom" || violatedRules(res)[0] != "deny" {
		t.Fatalf("custom rule not applied: %+v", res)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := newGateway().Validate(ctx, validItem(), models.StageIngest); res.Accepted() {
		t.Fatal("cancelled context must reject")
	}
}

func violatedRules(res models.ValidationResult) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range res.Violations {
		if !seen[v.Rule] {
			seen[v.Rule] = true
			out = append(out, v.Rule)
		}
	}
	return out
}
