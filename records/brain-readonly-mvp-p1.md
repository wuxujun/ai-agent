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

## Live verification follow-up

- After the user supplied Gemini configuration in the worktree-root
  `config.yaml`, the same two scenes resolved to Gemini with credentials,
  disabled fallback, and configured pricing. The configuration remains a
  user-owned uncommitted change and is not included in repository commits.
- Runtime hardening commit: `d725727` applies each scene timeout to the
  context passed into the structured caller; focused timeout, race, evaluator,
  vet, and diff checks passed.
- Full Live command (run in a network-enabled environment):
  `GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go run ./cmd/brain-eval -input evals/brain/dataset.yaml -mode live -format text -repetitions 3 -max-total-tokens 60000 -max-total-cost-usd 1.00`.
  It completed 21 cases before the `retraction_vendor` Writer request ended
  with a sanitized transient `unexpected EOF`. The report recorded 253 calls,
  43,491 total tokens, and $0.043055; the full Live gate did not pass because
  both arms of that case were marked incomparable.
- Root-cause evidence: the direct Gemini endpoint returned valid structured JSON
  with a 64-token cap, while the effective Live scene retry count was 0;
  generic transport errors are classified retryable but therefore had no
  retry opportunity. The worktree config now sets `max_retries: 1` for both
  `task_finalizer` and `answer_verifier` (still uncommitted user config).
- Targeted recovery check used an ephemeral one-case dataset, removed after
  the run, with three matched repetitions. `retraction_vendor` completed all
  Writer/Judge calls as `execution_ok=true` and `comparable=true`; 12 calls,
  1,945 tokens, and $0.001952 were recorded. This is recovery evidence only,
  not a replacement for a fresh full-dataset Live gate.
- Fresh full Live command rerun after the retry configuration:
  `GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go run ./cmd/brain-eval -input evals/brain/dataset.yaml -mode live -format text -repetitions 3 -max-total-tokens 60000 -max-total-cost-usd 1.00`.
  The report recorded 24/24 comparable cases, zero infrastructure errors,
  zero judge failures, 288 calls, 48,929 total tokens, and $0.048079. The
  comparison reported `passed=true`, with p95 latency ratio 1.049 and total
  token ratio 1.129. Candidate summary: evidence recall 0.967, evidence URI
  recall 0.958, citation coverage 1.0, fresh claim recall 1.0, answer
  accuracy 0.258, and no-answer false-positive rates 0.0.
- Full Live gate status: passed. The uncommitted `config.yaml` remains
  user-owned; no credentials or provider response bodies were committed.

## Post-review hardening

- Implementation commit: `f1f7fe0`.
- Configuration reload now rejects Brain runtime changes (enablement, root, or
  compiler bounds) and tenant Brain project-allowlist changes, preserving the
  active snapshot until restart. Regression coverage also proves unrelated
  tenant fields do not trigger this restart-required guard.
- Brain-only runtime configuration now registers `wiki_search`/`wiki_fetch`
  with an explicit `corpus` selector and a fail-closed ordinary Wiki stub;
  ordinary Wiki calls remain unavailable instead of silently falling back.
- Brain provider operations re-open and re-verify the pinned release after
  directory initialization and after search/read/graph operations. A mutated
  release is rejected before content is returned.
- Retraction ledger records are required to use canonical
  `brain-evidence://<tenant>/<project>/tasks/<task>#trace/<step>` URIs matching
  the resolved project; cross-scope records now invalidate the ledger.
- Production-shaped Engine.Next coverage now exercises registered read-only
  Brain search/fetch, restart/resume retaining the original snapshot after a
  newer CURRENT release, watermark change rejection between search and fetch,
  and Eino/Legacy/Step/ADK/Multi-Agent mode admission.
- Targeted verification after hardening:
  `go test -race ./internal/brain`, targeted config/tools/orchestrator/server
  race tests, `go vet ./internal/brain ./internal/config ./internal/tools
  ./internal/orchestrator ./cmd/server`, `git diff --check`, and the offline
  evaluator package tests all exited 0. The worktree-root `config.yaml`
  remains an intentionally unstaged user change.
