# Task 9 implementation report

- Added the shared Brain `TaskContext`, `SnapshotPinner`, and context helpers.
- `Engine.Next` now accepts an injected pinner, binds the pinned context, and
  persists a changed task before planning.
- Deterministic JIT Wiki routing adds `corpus=brain` for tasks with durable
  Brain project/snapshot identity while preserving non-Brain behavior.
- Added a config/repository/ledger-backed `SnapshotPinner` that selects
  CURRENT once, validates pinned releases and live retractions on resume, and
  rejects configuration drift with stable errors.

Verification passed:

```text
GOCACHE=/private/tmp/brain-mvp-go-cache go test ./internal/planner ./internal/orchestrator -run 'Test(.*JIT|.*Brain|Next)' -count=1
GOCACHE=/private/tmp/brain-mvp-go-cache go vet ./internal/brain ./internal/orchestrator ./internal/planner
git diff --check
```

The remaining Task 9 mode-specific context propagation and page-API allowlist
coverage require follow-up integration work; the committed vertical slice is
the pinner/Engine/JIT path above.

## Review fix round

- Pin admission now uses `ProjectConfigDigest(ref)`; the compiler manifest
  digest remains distinct runtime metadata.
- Engine pin fields roll back if immediate persistence fails.
- Pinned Brain memory-intent JIT requests route to `wiki_search` with Brain
  corpus, Brain repository initialization is fail-closed, and the bounded
  release compact index is loaded into `TaskContext`.

## Dynamic configuration fix

- Snapshot pin admission now reads `config.Get()` on every Pin call, rechecks
  Brain enablement, tenant/project authorization, and root drift, and fails
  closed after an allowlist/config reload.
- Added hot-reload regression coverage for disabled Brain, removed project
  authorization, and admission digest drift.

## Runtime/API vertical slice

- Wiki page authorization now admits the authenticated tenant's ordinary Wiki
  space plus its explicitly allowlisted Brain project spaces.
- Multi-Agent coordinator preserves the Engine's pinned Brain TaskContext when
  constructing retrieval execution context, matching Eino/ADK/Executor paths.

## Final API/context fix round

- Authenticated Brain page GETs now require an allowlisted tenant project and
  explicit pinned snapshot, then route through the Brain provider's verified
  release reader; ordinary Wiki requests and disabled Brain behavior remain
  unchanged. Server wiring exposes the Brain adapter even when ordinary Wiki
  is not configured.
- Default Executor and ADK tool wrappers rebuild retrieval context through the
  preservation helper, retaining Brain project, snapshot, and watermark
  injected by Engine.Next.
- Added regression coverage for authorized Brain routing, disabled/cross-tenant
  rejection, and preservation of pinned retrieval scope.

Verification passed:

```text
GOCACHE=/private/tmp/brain-mvp-go-cache go test -race ./internal/api ./internal/tools ./internal/executor ./internal/orchestrator ./cmd/server -run 'Test(GetWikiPage|WithPreserved|Retrieval|Executor|Adk|BuildWiki|Corpus)' -count=1
```
