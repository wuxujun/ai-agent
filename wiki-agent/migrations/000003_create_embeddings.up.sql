CREATE EXTENSION IF NOT EXISTS vector;

-- Width must match models.EmbeddingDimensions.
CREATE TABLE IF NOT EXISTS embeddings (
    chunk_id   UUID PRIMARY KEY REFERENCES chunks (id) ON DELETE CASCADE,
    model      TEXT         NOT NULL,
    embedding  vector(1536) NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS embeddings_embedding_hnsw_idx
    ON embeddings USING hnsw (embedding vector_cosine_ops);
