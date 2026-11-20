CREATE TABLE IF NOT EXISTS validations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id UUID             NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    status      TEXT             NOT NULL CHECK (status IN ('accepted', 'rejected')),
    stage       TEXT             NOT NULL CHECK (stage IN ('ingest', 'emit')),
    confidence  DOUBLE PRECISION NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    rule_set    TEXT             NOT NULL,
    violations  JSONB            NOT NULL DEFAULT '[]'::jsonb,
    checked_at  TIMESTAMPTZ      NOT NULL,
    expires_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS validations_document_checked_idx
    ON validations (document_id, checked_at DESC);
