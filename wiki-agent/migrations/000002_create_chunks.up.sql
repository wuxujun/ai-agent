CREATE TABLE IF NOT EXISTS chunks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id UUID        NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    ordinal     INTEGER     NOT NULL CHECK (ordinal >= 0),
    content     TEXT        NOT NULL,
    token_count INTEGER     NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chunks_document_ordinal_key UNIQUE (document_id, ordinal)
);
