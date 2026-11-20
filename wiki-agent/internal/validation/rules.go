package validation

import (
	"fmt"
	"math"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// ClockSkew is the tolerance for FetchedAt timestamps slightly in the future.
const ClockSkew = time.Minute

func violation(rule, format string, args ...any) models.Violation {
	return models.Violation{Rule: rule, Message: fmt.Sprintf(format, args...)}
}

// SchemaRule enforces structural integrity. At the ingest stage it also checks
// content hashes and embeddings; at the emit stage only the fields loaded by
// retrieval are checked.
type SchemaRule struct {
	EmbeddingDimensions int
}

func (SchemaRule) Name() string { return "schema" }

func (r SchemaRule) Check(item models.KnowledgeItem, stage models.ValidationStage, _ time.Time) []models.Violation {
	const name = "schema"
	var v []models.Violation
	d := item.Document
	if d.Source == "" {
		v = append(v, violation(name, "document.source is required"))
	}
	if d.URI == "" {
		v = append(v, violation(name, "document.uri is required"))
	}
	if !d.Kind.Valid() {
		v = append(v, violation(name, "document.kind %q is invalid", d.Kind))
	}
	if d.FetchedAt.IsZero() {
		v = append(v, violation(name, "document.fetched_at is required"))
	}
	if len(item.Chunks) == 0 {
		v = append(v, violation(name, "at least one chunk is required"))
	}

	if stage == models.StageEmit {
		for i, c := range item.Chunks {
			if c.ID == "" {
				v = append(v, violation(name, "chunk[%d].id is required", i))
			}
			if c.Content == "" {
				v = append(v, violation(name, "chunk[%d].content is empty", i))
			}
		}
		return v
	}

	if d.Content == "" {
		v = append(v, violation(name, "document.content is required"))
	} else if d.ContentHash != models.HashContent(d.Content) {
		v = append(v, violation(name, "document.content_hash does not match content"))
	}
	if item.EmbeddingModel == "" {
		v = append(v, violation(name, "embedding_model is required"))
	}
	seen := make(map[int]bool, len(item.Chunks))
	for i, c := range item.Chunks {
		if c.Content == "" {
			v = append(v, violation(name, "chunk[%d].content is empty", i))
		}
		if c.Ordinal < 0 || c.Ordinal >= len(item.Chunks) || seen[c.Ordinal] {
			v = append(v, violation(name, "chunk[%d].ordinal %d is out of range or duplicated", i, c.Ordinal))
		}
		seen[c.Ordinal] = true
		v = append(v, r.checkEmbedding(i, c.Embedding)...)
	}
	return v
}

func (r SchemaRule) checkEmbedding(i int, e []float32) []models.Violation {
	const name = "schema"
	if len(e) != r.EmbeddingDimensions {
		return []models.Violation{violation(name, "chunk[%d].embedding has %d dimensions, want %d", i, len(e), r.EmbeddingDimensions)}
	}
	var norm float64
	for _, x := range e {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return []models.Violation{violation(name, "chunk[%d].embedding contains NaN or Inf", i)}
		}
		norm += f * f
	}
	if norm == 0 {
		return []models.Violation{violation(name, "chunk[%d].embedding is a zero vector", i)}
	}
	return nil
}

// FreshnessRule rejects knowledge older than MaxAge or dated in the future.
type FreshnessRule struct {
	MaxAge time.Duration
}

func (FreshnessRule) Name() string { return "freshness" }

func (r FreshnessRule) Check(item models.KnowledgeItem, _ models.ValidationStage, now time.Time) []models.Violation {
	const name = "freshness"
	t := item.Document.FetchedAt
	if t.IsZero() {
		return nil // reported by SchemaRule
	}
	if t.After(now.Add(ClockSkew)) {
		return []models.Violation{violation(name, "fetched_at %s is in the future", t.Format(time.RFC3339))}
	}
	if r.MaxAge > 0 && now.Sub(t) > r.MaxAge {
		return []models.Violation{violation(name, "knowledge is %s old, max age is %s", now.Sub(t).Truncate(time.Second), r.MaxAge)}
	}
	return nil
}

// ConfidenceRule rejects knowledge below the configured confidence threshold.
type ConfidenceRule struct {
	Min float64
}

func (ConfidenceRule) Name() string { return "confidence" }

func (r ConfidenceRule) Check(item models.KnowledgeItem, _ models.ValidationStage, _ time.Time) []models.Violation {
	const name = "confidence"
	c := item.Confidence
	if math.IsNaN(c) || c < 0 || c > 1 {
		return []models.Violation{violation(name, "confidence %v must be within [0, 1]", c)}
	}
	if c < r.Min {
		return []models.Violation{violation(name, "confidence %.3f is below threshold %.3f", c, r.Min)}
	}
	return nil
}
