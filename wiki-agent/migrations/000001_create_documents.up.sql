CREATE TABLE IF NOT EXISTS documents (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source       TEXT        NOT NULL,
    uri          TEXT        NOT NULL,
    kind         TEXT        NOT NULL CHECK (kind IN ('doc', 'api_spec', 'code', 'finding')),
    title        TEXT        NOT NULL DEFAULT '',
    content      TEXT        NOT NULL,
    content_hash TEXT        NOT NULL,
    version      TEXT        NOT NULL DEFAULT '',
    metadata     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    fetched_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT documents_uri_hash_key UNIQUE (uri, content_hash)
);

CREATE INDEX IF NOT EXISTS documents_kind_idx ON documents (kind);
