package wiki

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// DefaultSearchLimit is used when SearchOptions.Limit is unset.
const DefaultSearchLimit = 5

// Agent is the Wiki Agent. It is safe for concurrent use if its dependencies are.
type Agent struct {
	repo      Repository
	embedder  Embedder
	chunker   Chunker
	validator Validator
	log       *slog.Logger
}

// NewAgent wires the Wiki Agent. A nil logger discards output.
func NewAgent(repo Repository, embedder Embedder, chunker Chunker, validator Validator, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Agent{repo: repo, embedder: embedder, chunker: chunker, validator: validator, log: log.With("component", "wiki")}
}

// IngestRequest is the input to Ingest.
type IngestRequest struct {
	Document   models.Document
	Confidence float64
}

// IngestResult reports the outcome of an Ingest call.
type IngestResult struct {
	DocumentID string                  `json:"document_id,omitempty"`
	Validation models.ValidationResult `json:"validation"`
}

// Ingest chunks and embeds a document, runs the Validation Gateway, and
// persists it only if accepted. Rejections return ErrRejected with the result.
func (a *Agent) Ingest(ctx context.Context, req IngestRequest) (IngestResult, error) {
	doc := req.Document
	if doc.FetchedAt.IsZero() {
		doc.FetchedAt = time.Now().UTC()
	}
	if doc.ContentHash == "" {
		doc.ContentHash = models.HashContent(doc.Content)
	}

	texts := a.chunker.Split(doc.Content)
	var vectors [][]float32
	if len(texts) > 0 {
		var err error
		vectors, err = a.embedder.Embed(ctx, texts)
		if err != nil {
			return IngestResult{}, fmt.Errorf("wiki: embed: %w", err)
		}
		if len(vectors) != len(texts) {
			return IngestResult{}, fmt.Errorf("wiki: embedder returned %d vectors for %d chunks", len(vectors), len(texts))
		}
	}
	chunks := make([]models.Chunk, len(texts))
	for i, t := range texts {
		chunks[i] = models.Chunk{Ordinal: i, Content: t, TokenCount: approxTokens(t), Embedding: vectors[i]}
	}
	item := models.KnowledgeItem{
		Document:       doc,
		Chunks:         chunks,
		EmbeddingModel: a.embedder.Model(),
		Confidence:     req.Confidence,
	}

	res := a.validator.Validate(ctx, item, models.StageIngest)
	if !res.Accepted() {
		a.log.WarnContext(ctx, "knowledge rejected", "uri", doc.URI, "violations", res.Violations)
		return IngestResult{Validation: res}, ErrRejected
	}
	id, err := a.repo.SaveKnowledge(ctx, item, res)
	if err != nil {
		return IngestResult{Validation: res}, fmt.Errorf("wiki: save: %w", err)
	}
	a.log.InfoContext(ctx, "knowledge accepted", "uri", doc.URI, "document_id", id, "chunks", len(chunks))
	return IngestResult{DocumentID: id, Validation: res}, nil
}

// Retrieve returns verified chunks relevant to query. Every result is
// re-validated on the emit side; failing chunks are dropped.
func (a *Agent) Retrieve(ctx context.Context, query string, opts models.SearchOptions) ([]models.RetrievedChunk, error) {
	if opts.Limit <= 0 {
		opts.Limit = DefaultSearchLimit
	}
	vecs, err := a.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("wiki: embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("wiki: embedder returned %d vectors for 1 query", len(vecs))
	}
	found, err := a.repo.SearchSimilar(ctx, vecs[0], opts)
	if err != nil {
		return nil, fmt.Errorf("wiki: search: %w", err)
	}
	verified := found[:0]
	for _, rc := range found {
		if res := a.validator.ValidateRetrieved(ctx, rc); res.Accepted() {
			verified = append(verified, rc)
		} else {
			a.log.DebugContext(ctx, "dropping unverified chunk", "chunk_id", rc.Chunk.ID, "violations", res.Violations)
		}
	}
	return verified, nil
}

// SubmitFindings ingests executor findings through the Validation Gateway and
// returns one result per finding, in order.
func (a *Agent) SubmitFindings(ctx context.Context, findings []models.Finding) ([]models.ValidationResult, error) {
	results := make([]models.ValidationResult, 0, len(findings))
	for _, f := range findings {
		res, err := a.Ingest(ctx, IngestRequest{
			Document: models.Document{
				Source:    f.Source,
				URI:       f.URI,
				Kind:      models.KindFinding,
				Title:     f.Summary,
				Content:   f.Content,
				FetchedAt: f.ObservedAt,
				Metadata:  map[string]string{"step_id": f.StepID},
			},
			Confidence: f.Confidence,
		})
		if err != nil && !errors.Is(err, ErrRejected) {
			return results, err
		}
		results = append(results, res.Validation)
	}
	return results, nil
}

func approxTokens(s string) int { return (len(s) + 3) / 4 }

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
