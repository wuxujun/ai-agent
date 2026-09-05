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
