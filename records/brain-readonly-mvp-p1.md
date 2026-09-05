# Brain Read-only MVP P1 verification record

This record contains only repository metadata and test outcomes. It omits
credentials, prompts, provider responses, page content, tenant-sensitive
identifiers, and raw error payloads.

## Task 11 offline verification

- Final pre-review commit: `40b76f1`; verification review commit: `60fdbbb`.
- Final verification commit: `38e6754`.
- Coverage: temporary-directory compile/publish/provider search-read/rollback;
  retraction after search; repository status and rollback rejection.
- Focused deterministic command: `GOCACHE=/private/tmp/brain-mvp-go-cache go test ./internal/brain -run 'TestReadOnlyMVP' -count=1` (exit 0).
- Focused race command: `GOCACHE=/private/tmp/brain-mvp-go-cache go test -race ./internal/brain -run 'TestReadOnlyMVP' -count=1` (exit 0).
- Vet command: `GOCACHE=/private/tmp/brain-mvp-go-cache go vet ./internal/brain` (exit 0).
- Diff command: `git diff --check` (exit 0).
- Offline-only: no live provider, network, or HTTP server command was run (not applicable; exit 0 by scope).
- Dataset/fixture state: no checked-in or persistent fixture; all data is created
  beneath `t.TempDir()` and removed by the test harness.
- Live verification: not run; the E2E uses a deterministic fake synthesizer
  and local repository only.
- Full-suite note: sandbox loopback restrictions prevented unrelated tests that
  create `httptest` listeners; this is an environment limitation, not a Brain
  assertion failure.
- Hash scope: assertions cover non-sensitive manifest file hashes, tenant/project
  scope, pinned snapshot IDs, and Brain URI provenance; no raw content is recorded.
- Fixture constructors: `testCompiler`, `repositoryWithLedger`, `stageVerified`,
  and `verifiedDraft` create all data under `t.TempDir()` with fake synthesis.
