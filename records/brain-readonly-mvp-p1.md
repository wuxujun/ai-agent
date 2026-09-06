# Brain Read-only MVP P1 verification record

This record contains only repository metadata and test outcomes. It omits
credentials, prompts, provider responses, page content, tenant-sensitive
identifiers, and raw error payloads.

## Task 11 offline verification

- Final pre-review commit: `40b76f1`; verification review commit: `60fdbbb`.
- Final deterministic E2E implementation commit: `f9ef54c`.
- Final verification commits: `b27bcb9`, `5c8ea4d`.
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
- Offline evaluator availability check: `test -f evals/brain/dataset.yaml` and
  `test -x ./cmd/brain-eval` both exit 0. Dataset SHA-256:
  `d130aec473e6754a836b135e00f2fdfe53e148fa4eeff6eedd97b9f622872dc4`.
- Latest deterministic E2E command: `GOCACHE=/private/tmp/brain-mvp-go-cache go test ./internal/brain -run 'TestReadOnlyMVP' -count=1` (exit 0).
- Latest race E2E command: `GOCACHE=/private/tmp/brain-mvp-go-cache go test -race ./internal/brain -run 'TestReadOnlyMVP' -count=1` (exit 0).
- Offline evaluator run: `GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go run ./cmd/brain-eval -input evals/brain/dataset.yaml -mode offline -format json` (exit 0; paired gate passed). Candidate summary: 24 comparable cases, evidence recall 0.9667, evidence URI recall 0.9583, citation coverage 1.0, fresh claim recall 1.0, scope/entity/retraction/prompt-injection recurrences 0, token/cost totals 0. Baseline critical fixtures remain expected dataset negatives; no live calls were made.
- Final verification commit before this record update: `982366c` (E2E source/store/rebuild coverage spans `f9ef54c` and `982366c`).
- Approved Live gate attempt: `go run ./cmd/brain-eval -input evals/brain/dataset.yaml -mode live -format json -repetitions 3 -max-total-tokens 60000 -max-total-cost-usd 1.00` reached preflight and exited 2 because the environment had no credential for `task_finalizer`; no provider calls, tokens, or cost were incurred.
