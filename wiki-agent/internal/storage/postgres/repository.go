package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// Repository implements wiki.Repository on PostgreSQL + pgvector.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository wraps a pool created by NewPool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// SaveKnowledge stores the document, chunks and embeddings (once per
// uri+content_hash) and appends a validation record, in one transaction.
func (r *Repository) SaveKnowledge(ctx context.Context, item models.KnowledgeItem, res models.ValidationResult) (id string, err error) {
	if !res.Accepted() {
		return "", errors.New("postgres: refusing to store rejected knowledge")
	}
	d := item.Document
	metadata := d.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	violations := res.Violations
	if violations == nil {
		violations = []models.Violation{}
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	var inserted bool
	err = tx.QueryRow(ctx, `
		INSERT INTO documents (source, uri, kind, title, content, content_hash, version, metadata, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (uri, content_hash) DO UPDATE
		SET fetched_at = GREATEST(documents.fetched_at, EXCLUDED.fetched_at), updated_at = now()
		RETURNING id::text, (xmax = 0)`,
		d.Source, d.URI, string(d.Kind), d.Title, d.Content, d.ContentHash, d.Version, metadata, d.FetchedAt,
	).Scan(&id, &inserted)
	if err != nil {
		return "", fmt.Errorf("postgres: upsert document: %w", err)
	}

	if inserted {
		batch := &pgx.Batch{}
		for _, c := range item.Chunks {
			batch.Queue(`
				WITH c AS (
					INSERT INTO chunks (document_id, ordinal, content, token_count)
					VALUES ($1, $2, $3, $4) RETURNING id
				)
				INSERT INTO embeddings (chunk_id, model, embedding) SELECT id, $5, $6 FROM c`,
				id, c.Ordinal, c.Content, c.TokenCount, item.EmbeddingModel, pgvector.NewVector(c.Embedding))
		}
		if err = tx.SendBatch(ctx, batch).Close(); err != nil {
			return "", fmt.Errorf("postgres: insert chunks: %w", err)
		}
	}

	if _, err = tx.Exec(ctx, `
		INSERT INTO validations (document_id, status, stage, confidence, rule_set, violations, checked_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, string(res.Status), string(res.Stage), res.Confidence, res.RuleSet, violations, res.CheckedAt, res.ExpiresAt,
	); err != nil {
		return "", fmt.Errorf("postgres: insert validation: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("postgres: commit: %w", err)
	}
	return id, nil
}

const searchSQL = `
SELECT c.id::text, c.document_id::text, c.ordinal, c.content, c.token_count,
       d.source, d.uri, d.kind, d.title, d.content_hash, d.version, d.metadata, d.fetched_at,
       v.confidence, v.checked_at,
       e.embedding <=> $1 AS distance
FROM embeddings e
JOIN chunks c    ON c.id = e.chunk_id
JOIN documents d ON d.id = c.document_id
JOIN LATERAL (
    SELECT status, confidence, checked_at, expires_at
    FROM validations
    WHERE document_id = d.id
    ORDER BY checked_at DESC
    LIMIT 1
) v ON true
WHERE v.status = 'accepted'
  AND (v.expires_at IS NULL OR v.expires_at > now())
  AND (coalesce(cardinality($2::text[]), 0) = 0 OR d.kind = ANY($2::text[]))
  AND ($3::float8 <= 0 OR e.embedding <=> $1 <= $3::float8)
ORDER BY e.embedding <=> $1
LIMIT $4`

// SearchSimilar returns the nearest accepted, unexpired chunks by cosine distance.
func (r *Repository) SearchSimilar(ctx context.Context, query []float32, opts models.SearchOptions) ([]models.RetrievedChunk, error) {
	if len(query) != models.EmbeddingDimensions {
		return nil, fmt.Errorf("postgres: query has %d dimensions, want %d", len(query), models.EmbeddingDimensions)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 5
	}
	kinds := make([]string, len(opts.Kinds))
	for i, k := range opts.Kinds {
		kinds[i] = string(k)
	}

	rows, err := r.pool.Query(ctx, searchSQL, pgvector.NewVector(query), kinds, opts.MaxDistance, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: search: %w", err)
	}
	defer rows.Close()

	var out []models.RetrievedChunk
	for rows.Next() {
		var (
			rc       models.RetrievedChunk
			kind     string
			verified time.Time
		)
		if err := rows.Scan(
			&rc.Chunk.ID, &rc.Chunk.DocumentID, &rc.Chunk.Ordinal, &rc.Chunk.Content, &rc.Chunk.TokenCount,
			&rc.Document.Source, &rc.Document.URI, &kind, &rc.Document.Title, &rc.Document.ContentHash,
			&rc.Document.Version, &rc.Document.Metadata, &rc.Document.FetchedAt,
			&rc.Confidence, &verified, &rc.Distance,
		); err != nil {
			return nil, fmt.Errorf("postgres: scan: %w", err)
		}
		rc.Document.ID = rc.Chunk.DocumentID
		rc.Document.Kind = models.DocumentKind(kind)
		rc.VerifiedAt = verified
		rc.Similarity = 1 - rc.Distance
		out = append(out, rc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: rows: %w", err)
	}
	return out, nil
}
