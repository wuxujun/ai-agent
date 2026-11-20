// Package wiki implements the Wiki Agent: the knowledge engine that chunks,
// embeds, validates, stores and retrieves verified knowledge.
package wiki

import (
	"context"
	"errors"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// ErrRejected is returned when the Validation Gateway rejects an item.
var ErrRejected = errors.New("wiki: knowledge rejected by validation gateway")

// Repository persists accepted knowledge and performs vector similarity search.
type Repository interface {
	// SaveKnowledge stores an accepted item and its validation metadata and
	// returns the persisted document ID.
	SaveKnowledge(ctx context.Context, item models.KnowledgeItem, result models.ValidationResult) (string, error)
	// SearchSimilar returns accepted, unexpired chunks ordered by ascending
	// cosine distance to query.
	SearchSimilar(ctx context.Context, query []float32, opts models.SearchOptions) ([]models.RetrievedChunk, error)
}

// Embedder converts text into fixed-width vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dimensions() int
}

// Chunker splits document content into retrievable chunks.
type Chunker interface {
	Split(content string) []string
}

// Validator is the subset of the Validation Gateway used by the Wiki Agent.
type Validator interface {
	Validate(ctx context.Context, item models.KnowledgeItem, stage models.ValidationStage) models.ValidationResult
	ValidateRetrieved(ctx context.Context, rc models.RetrievedChunk) models.ValidationResult
}
