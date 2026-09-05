# Task 7 implementation report

## Scope

- Added `store.Open(kind, dsn)` as the single Store backend factory and kept
  backend initialization errors bounded so DSNs are never returned.
- Refactored `cmd/server.buildStore` to delegate to that factory.
- Added the dependency-injected `brain-compile` CLI with `build`, `inspect`,
  `verify`, `publish`, `rollback`, and `status` commands.
- Enforced explicit tenant/project scope and required snapshot/CAS arguments
  for lifecycle commands. Build is the only command that opens the configured
  task Store; it stages through the existing Compiler and never publishes.
- CLI output is bounded metadata only and does not include source content,
  prompts, credentials, provider responses, or raw errors. Exit code 0 means
  success, 1 means a validation/CAS/revocation gate, and 2 means usage,
  configuration, or infrastructure failure.

## Verification

Passed:

```text
GOCACHE=/private/tmp/brain-mvp-go-cache go test ./internal/store ./cmd/brain-compile ./cmd/server -run 'Test(Open|Run.*Brain|RunPublish|BuildStore)' -count=1
GOCACHE=/private/tmp/brain-mvp-go-cache go build ./cmd/brain-compile
GOCACHE=/private/tmp/brain-mvp-go-cache go vet ./internal/store ./cmd/brain-compile ./cmd/server
git diff --check
```

The full `internal/store ./cmd/brain-compile ./cmd/server` package run was
also attempted; the existing sandbox restriction preventing `httptest` from
binding `[::1]:0` caused the unrelated server integration test
`TestWikiRuntimeStreamableHTTPEndToEnd` to panic.
