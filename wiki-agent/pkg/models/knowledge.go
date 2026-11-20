// Package models defines the domain types shared by every Wiki Agent component.
package models

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// EmbeddingDimensions is the vector width enforced by the database schema
// (embeddings.embedding vector(1536)). Changing it requires a migration.
const EmbeddingDimensions = 1536

// DocumentKind classifies the origin of a piece of knowledge.
type DocumentKind string

const (
	KindDoc     DocumentKind = "doc"
	KindAPISpec DocumentKind = "api_spec"
	KindCode    DocumentKind = "code"
	KindFinding DocumentKind = "finding"
)

// Valid reports whether k is a known kind.
func (k DocumentKind) Valid() bool {
	switch k {
	case KindDoc, KindAPISpec, KindCode, KindFinding:
		return true
	}
	return false
}

// Document is a raw source of knowledge (documentation page, API spec, source file, finding).
type Document struct {
	ID          string            `json:"id,omitempty"`
	Source      string            `json:"source"`
	URI         string            `json:"uri"`
	Kind        DocumentKind      `json:"kind"`
	Title       string            `json:"title,omitempty"`
	Content     string            `json:"content,omitempty"`
	ContentHash string            `json:"content_hash,omitempty"`
	Version     string            `json:"version,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	FetchedAt   time.Time         `json:"fetched_at"`
}

// HashContent returns the canonical SHA-256 hex digest used for Document.ContentHash.
func HashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Chunk is a retrievable slice of a Document together with its embedding.
type Chunk struct {
	ID         string    `json:"id,omitempty"`
	DocumentID string    `json:"document_id,omitempty"`
	Ordinal    int       `json:"ordinal"`
	Content    string    `json:"content"`
	TokenCount int       `json:"token_count"`
	Embedding  []float32 `json:"-"`
}

// KnowledgeItem is the unit submitted to the Validation Gateway and persisted
// by the Wiki Agent: a document, its chunks and the producer's confidence.
type KnowledgeItem struct {
	Document       Document `json:"document"`
	Chunks         []Chunk  `json:"chunks"`
	EmbeddingModel string   `json:"embedding_model"`
	Confidence     float64  `json:"confidence"`
}

// RetrievedChunk is a chunk returned from similarity search along with the
// verification metadata recorded when it was accepted.
type RetrievedChunk struct {
	Chunk      Chunk     `json:"chunk"`
	Document   Document  `json:"document"`
	Distance   float64   `json:"distance"`
	Similarity float64   `json:"similarity"`
	Confidence float64   `json:"confidence"`
	VerifiedAt time.Time `json:"verified_at"`
}

// SearchOptions constrains similarity search.
type SearchOptions struct {
	Limit       int            `json:"limit"`
	MaxDistance float64        `json:"max_distance,omitempty"`
	Kinds       []DocumentKind `json:"kinds,omitempty"`
}
