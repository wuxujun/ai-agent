// Package validation implements the Validation Gateway: the set of rules a
// KnowledgeItem must satisfy before the Wiki Agent accepts (ingest) or emits it.
package validation

import (
	"context"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// DefaultRuleSet identifies the built-in rules in persisted validation metadata.
const DefaultRuleSet = "default/v1"

// Rule is a single verification rule. Implementations must be pure and safe
// for concurrent use.
type Rule interface {
	Name() string
	Check(item models.KnowledgeItem, stage models.ValidationStage, now time.Time) []models.Violation
}

// Config configures the built-in rules.
type Config struct {
	// MinConfidence is the inclusive lower bound on KnowledgeItem.Confidence.
	MinConfidence float64
	// MaxAge is the maximum age of Document.FetchedAt. Zero disables the check.
	MaxAge time.Duration
	// EmbeddingDimensions is the required embedding width.
	EmbeddingDimensions int
}

// Gateway evaluates every configured rule and aggregates the violations.
type Gateway struct {
	rules   []Rule
	ruleSet string
	maxAge  time.Duration
	now     func() time.Time
}

// Option customises a Gateway.
type Option func(*Gateway)

// WithClock overrides the time source (useful in tests).
func WithClock(now func() time.Time) Option { return func(g *Gateway) { g.now = now } }

// WithRules appends additional rules after the built-ins.
func WithRules(rules ...Rule) Option { return func(g *Gateway) { g.rules = append(g.rules, rules...) } }

// WithRuleSet overrides the rule-set label stored with validation results.
func WithRuleSet(name string) Option { return func(g *Gateway) { g.ruleSet = name } }

// NewGateway builds a Gateway with the schema, freshness and confidence rules.
func NewGateway(cfg Config, opts ...Option) *Gateway {
	if cfg.EmbeddingDimensions <= 0 {
		cfg.EmbeddingDimensions = models.EmbeddingDimensions
	}
	g := &Gateway{
		rules: []Rule{
			SchemaRule{EmbeddingDimensions: cfg.EmbeddingDimensions},
			FreshnessRule{MaxAge: cfg.MaxAge},
			ConfidenceRule{Min: cfg.MinConfidence},
		},
		ruleSet: DefaultRuleSet,
		maxAge:  cfg.MaxAge,
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Validate runs all rules against item for the given stage.
func (g *Gateway) Validate(ctx context.Context, item models.KnowledgeItem, stage models.ValidationStage) models.ValidationResult {
	now := g.now().UTC()
	res := models.ValidationResult{
		Status:     models.StatusAccepted,
		Stage:      stage,
		Confidence: item.Confidence,
		RuleSet:    g.ruleSet,
		CheckedAt:  now,
	}
	if err := ctx.Err(); err != nil {
		res.Status = models.StatusRejected
		res.Violations = []models.Violation{{Rule: "context", Message: err.Error()}}
		return res
	}
	for _, r := range g.rules {
		res.Violations = append(res.Violations, r.Check(item, stage, now)...)
	}
	if len(res.Violations) > 0 {
		res.Status = models.StatusRejected
	}
	if g.maxAge > 0 && !item.Document.FetchedAt.IsZero() {
		exp := item.Document.FetchedAt.Add(g.maxAge).UTC()
		res.ExpiresAt = &exp
	}
	return res
}

// ValidateRetrieved re-checks a stored chunk before it is emitted to a consumer.
func (g *Gateway) ValidateRetrieved(ctx context.Context, rc models.RetrievedChunk) models.ValidationResult {
	return g.Validate(ctx, models.KnowledgeItem{
		Document:   rc.Document,
		Chunks:     []models.Chunk{rc.Chunk},
		Confidence: rc.Confidence,
	}, models.StageEmit)
}
