# Wiki Agent

Production-oriented Go scaffold for a modular multi-agent system for AI-assisted
software development. Knowledge is only accepted, stored, or handed to the
planner after it passes the **Validation Gateway**.

This is a standalone Go module (`github.com/wuxujun/ai-agent/wiki-agent`)
and does not share code with the parent runtime.

## Architecture

```
            ┌──────────────── Orchestrator (state machine + messaging) ───────────────┐
            │                                                                          │
 Task ──▶ pending ─▶ planning ─▶ executing ─▶ verifying ─▶ completed                   │
            │          ▲  │          │            │  └──▶ planning (re-plan w/ feedback)│
            │          │  ▼          ▼            ▼                                     │
            │       Planner      Executor     Wiki Agent ──▶ Validation Gateway         │
            │          │        (contract)        │              (schema, freshness,    │
            │          └── Retrieve verified ─────┤               confidence)           │
            │                                     ▼                                     │
            │                         PostgreSQL + pgvector (HNSW, cosine)              │
            └──────────────────────────────────────────────────────────────────────────┘
```

| Component | Package | Responsibility |
|-----------|---------|----------------|
| Orchestrator | `internal/orchestrator` | Drives `pending → planning → executing → verifying → completed`, re-plans with verification feedback (bounded by `MaxIterations`), handles failure/cancellation, and logs inter-agent messages on the `Workflow`. |
| Planner | `internal/planner` | Retrieves verified knowledge and generates steps. It rejects plans with no knowledge, or with steps that cite chunks outside the verified set. The `Generator` is pluggable (template now, LLM later). |
| Wiki Agent | `internal/wiki` | Chunks, embeds, validates, persists, and retrieves knowledge. Re-validates every chunk before returning it, and ingests executor findings through the gateway. |
| Executor | `orchestrator.Executor` | Contract only in v1: `Execute(ctx, task, step) ([]Finding, error)`. |
| Validation Gateway | `internal/validation` | `SchemaRule`, `FreshnessRule`, `ConfidenceRule`, plus custom `Rule`s, checked at both `ingest` and `emit`. |
| Storage | `internal/storage/postgres` | pgx/v5 pool with pgvector types, embedded golang-migrate migrations, and a repository that searches by cosine distance. |
| Models | `pkg/models` | Shared domain types. |

Interfaces are defined where they are used (`orchestrator.Planner/Executor/KnowledgeBase`,
`planner.KnowledgeRetriever/Generator`, `wiki.Repository/Embedder/Chunker/Validator`).
Any component can be replaced or mocked.

### Schema (`migrations/`)

| Table | Purpose |
|-------|---------|
| `documents` | Raw sources (`doc`, `api_spec`, `code`, `finding`). Unique on `(uri, content_hash)`. |
| `chunks` | Ordered slices of a document. |
| `embeddings` | `vector(1536)` per chunk, with an HNSW `vector_cosine_ops` index. |
| `validations` | Gateway verdict, confidence, rule set, violations, and `expires_at`. Only the latest accepted, unexpired record makes a document retrievable. |

The embedding width is `models.EmbeddingDimensions` (1536). Changing it requires a new migration.

## Quick start

```bash
cp .env.example .env
make up                 # Postgres+pgvector → migrations → server on :8080
curl localhost:8080/readyz

curl -XPOST localhost:8080/v1/knowledge -d '{
  "document": {"source":"openapi","uri":"spec://users","kind":"api_spec",
               "title":"Users API","content":"GET /users lists users with cursor pagination."},
  "confidence": 0.9}'                                   # 201 accepted / 422 with violations

curl -XPOST localhost:8080/v1/knowledge/search -d '{"query":"users pagination","limit":3}'
curl -XPOST localhost:8080/v1/plans -d '{"id":"t1","goal":"add cursor pagination to users"}'
```

To run on the host instead: `make db-up && make run`.

> The default `HashEmbedder` is a deterministic feature-hashing embedder for
> development and tests. It only measures word overlap, not meaning. For real
> use, implement `wiki.Embedder` with an embedding model.

## Make targets

| Target | Description |
|--------|-------------|
| `make build` / `make run` | Build `bin/server` / run on host |
| `make test` | Unit tests with `-race` |
| `make test-integration` | Starts compose Postgres, then runs the pgvector and end-to-end tests (`-tags integration`) |
| `make lint` | `gofmt` check + `go vet` (including integration files) |
| `make up` / `make down` | Full docker compose stack |
| `make db-up` / `make db-reset` | Start Postgres and migrate / wipe the volume |
| `make migrate-up` / `migrate-down` / `migrate-version` | golang-migrate CLI via the compose `migrate` service |
| `make migrate-create name=add_x` | New sequential migration pair |

Each integration test creates its own throwaway database from `TEST_DATABASE_URL`,
applies the embedded migrations, and drops the database afterwards.

## Configuration

All settings come from environment variables. See [`.env.example`](.env.example).
The main knobs are `DATABASE_URL`, `VALIDATION_MIN_CONFIDENCE`, `VALIDATION_MAX_AGE`,
`SEARCH_LIMIT`, `SEARCH_MAX_DISTANCE`, `ORCHESTRATOR_MAX_ITERATIONS`, `LOG_LEVEL`, and `LOG_FORMAT`.
Set `MIGRATE_ON_START=true` to apply the embedded migrations at boot when compose's `migrate` service isn't used.

## Roadmap

- Model-backed `Embedder` and LLM `Generator`
- Concrete `Executor` implementations (sandboxed code and tool runners)
- Workflow persistence and an HTTP endpoint for running workflows
- Source-trust rules and re-validation jobs for expiring knowledge
