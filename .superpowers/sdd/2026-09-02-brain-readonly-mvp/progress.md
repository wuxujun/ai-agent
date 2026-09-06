# SDD ledger — plan: docs/superpowers/plans/2026-09-02-brain-readonly-mvp.md

## Setup

- Branch: `feat/brain-readonly-mvp`
- Worktree: `/Users/xujunwu/Documents/IDEAProject/ai-agent/.worktrees/brain-readonly-mvp`
- Merge base: `ed4a70fce35e91c48a3fa59ec379ecdbeba457ab`
- Baseline: `GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./...` passed outside the filesystem/network sandbox. The sandboxed run failed only where `httptest` could not bind `[::1]:0`.
- Spec authority: `docs/superpowers/specs/2026-09-02-brain-readonly-mvp-design.md`

## Preflight dependency and conflict scan

| Tasks | Shared file/interface | Finding |
|---|---|---|
| 1 → 2 | `ResolveProject`, tenant `brain_projects` | Compatible: Task 2 consumes the explicit tenant/project authorization contract. |
| 1 ↔ 6 | `internal/config/config.go`, compiler configuration | Compatible if Task 6 extends the Task 1 config without changing its defaults, validation, clone, or reload semantics. |
| 1 ↔ 10 | `config.yaml`, `config_zh.yml` | Compatible: Task 1 defines values; Task 10 completes operator documentation without redefining them. |
| 2 → 3 | durable `types.Task` Brain identity | Compatible: Task 3 filters exact tenant/project using Task 2 fields. |
| 2 → 9 | durable snapshot/config fields | Compatible: Task 9 fills and persists the fields introduced by Task 2. |
| 2 ↔ 10 | `internal/api/handler.go` and tests | Compatible: admission changes precede additive readiness/metrics output. |
| 3 → 4 | `Manifest`, `Retraction`, `SnapshotDraft`, `Release` | Compatible: Task 4 owns storage lifecycle for Task 3 models. |
| 3 → 5 | evidence/claim/page models | Compatible: Task 5 renders and validates the same canonical contracts. |
| 3 → 6 | `SourceReader`, `SourceSet`, `Synthesis` | Gap: the plan's `SourceSet` omitted discovery hints even though the spec permits sanitized Task Memories and FinalAnswer for topic discovery only. See Ruling 1. |
| 4 → 5 | `RetractionView` and draft/manifest integrity | Compatible: live retractions remain a hard validation gate. |
| 4 → 6 | `Repository`, `RetractionView` | Compatible: compiler stages only after deterministic validation. |
| 4 → 7 | repository lifecycle methods | Compatible: CLI is the only publication caller. |
| 4 → 8 | release reader and live ledger | Compatible: provider reads verified pinned releases and rechecks retractions. |
| 4 → 11 | publish/rollback/retraction lifecycle | Compatible: end-to-end fixtures cover the complete lifecycle. |
| 5 → 6 | `Render` and `Validate` | Compatible: signatures consistently use `Render(Synthesis, int)` and `Validate(context.Context, ProjectRef, SnapshotDraft, RetractionView)`. |
| 6 → 7 | `Compiler.Build` | Compatible: only CLI build invokes compilation. |
| 6 → 11 | fake synthesis and compiler | Compatible after using test-only deterministic snapshot fixture IDs; see Ruling 2. |
| 7 self | `go build ./cmd/brain-compile` | Conflict with clean-worktree discipline because it emits a root binary. See Ruling 3. |
| 8 ↔ 9 | `internal/brain/provider.go` | Compatible: Task 8 creates read routing; Task 9 adds task pinning/context. |
| 8 ↔ 10 | `cmd/server/wiki_builder.go` | Compatible: Task 8 wires the router; Task 10 adds bounded health reporting. |
| 8 → 11 | corpus-aware Search/Fetch/provider | Compatible: end-to-end assertions exercise the same existing Wiki tool surface. |
| 9 → 11 | pinned task context | Compatible: old tasks keep the pinned release while retractions override it. |
| 10 → 11 | metrics/readiness evidence | Compatible: final record captures sanitized aggregate results only. |

## Task internal-consistency scan

| Task | Tests vs implementation; files vs later use | Finding |
|---|---|---|
| 1 | Disabled defaults, resolver isolation, rejected reload preservation; config/resolver files align. | Consistent. |
| 2 | API denial and Store round-trip drive Task fields, SQL migrations, and Redis JSON persistence. | Consistent. |
| 3 | Eligibility and trace-evidence tests drive bounded source normalization. | Consistent with Ruling 1. |
| 4 | CAS, rollback, revocation, crash/path/race cases drive repository and ledger. | Consistent; no physical erasure or recursive cleanup. |
| 5 | Byte-stable render and hard-gate tests match renderer/validator signatures. | Consistent. |
| 6 | Fake structured caller exercises pre-call and post-call bounds; compiler cannot publish. | Consistent; fake plugs into the existing structured runtime boundary. |
| 7 | Store factory and dependency-injected CLI tests match CLI-only lifecycle. | Consistent with Ruling 3. |
| 8 | Default Wiki behavior, corpus routing, candidate scope, watermark and cache tests match provider/router. | Consistent. |
| 9 | Pin-once, resume, drift, mode and authorization tests match one shared task context path. | Consistent. |
| 10 | Cardinality and readiness tests match metrics and documentation changes. | Consistent. |
| 11 | End-to-end lifecycle, deterministic suite, offline gate and separately approved Live gate align. | Consistent with Ruling 2; execution must stop for explicit Live budget approval. |

## Rulings

- Ruling 1: Task 3 will add `DiscoveryHints []string` to `SourceSet`, populated only from bounded sanitized Task `Memories` and `FinalAnswer`; claims must still cite eligible successful Trace evidence — this preserves the spec's discovery-only role; if wrong, topic discovery may differ but claim provenance remains fail-closed.
- Ruling 2: Task 11 may use a test-only deterministic snapshot-ID hook/fixture and must not add a production caller-controlled snapshot ID solely for tests — this keeps the production trust boundary narrow; if wrong, the fixture may need adaptation without changing publication semantics.
- Ruling 3: Task 7 will run `go build -o /private/tmp/brain-compile ./cmd/brain-compile` instead of emitting `./brain-compile` in the worktree — this verifies the same binary while keeping Git state clean; if wrong, only the build artifact location changes.
- Ruling 4: The SDD scratch workspace will not be recursively deleted at finish because repository instructions prohibit bulk directory deletion; it will be left ignored and its exact path reported for manual cleanup — if wrong, only ignored scratch artifacts remain on disk.

## Task execution

- Task 1 dispatched to `/root/brain_p1_task1_impl`; base `ed4a70fce35e91c48a3fa59ec379ecdbeba457ab`; brief `task-1-brief.md`; report `task-1-report.md`.
- Task 1 review at `a1dadfe`: one Important config-validation gap; one Critical/Minor pair disputed by the approved storage layout.
- Ruling 5: `ProjectRef.StorageKey` remains the stable hash of `tenant_id` only because the approved layout is exactly `data/brain/<tenant-key>/<project-id>/`; scope uniqueness is the tuple `StorageKey + ProjectID`, not either field alone — if wrong, Task 4's repository path contract would need a migration before release.
- Ruling 6: the review's derivative Minor request for a cross-project-unique `StorageKey` test is not adopted; Task 4 must instead test that full resolved project roots differ for two projects under one tenant — if wrong, Task 1 may under-test a storage-key invariant the spec does not state.
- Task 1: fix round 1/5 (1 addressed, 0 open; commit `39ae9aa`).
- Task 1: complete (commits `ed4a70f..39ae9aa`, review clean).
- Task 2 dispatched to `/root/brain_p1_task2_impl`; base `39ae9aae679b7e669ece82d09a63ecd2d892e56a`; brief `task-2-brief.md`; report `task-2-report.md`.
- Task 2 suspended before edits: implementer stopped on account usage limit; no `task-2-report.md`, no working-tree changes, and no commit were produced. Resume Task 2 from its RED test step with a fresh implementer.
- Session handoff requested; durable summary: `records/brain-readonly-mvp-p1-handoff.md`.
- Task 2 resumed with fresh implementer `/root/brain_p1_task2_impl_v2`; base `ba458d32b7e3e7b698db58669adfa8af242c8067`; report `task-2-report.md`; implementation commit `966f8bd`.
- Task 2 review: spec compliant, task quality approved, no Critical/Important/Minor findings. Reviewer could not verify live Postgres/Redis because services were unconfigured; this is an explicit optional-test skip, not an implementation gap.
- Task 2 controller verification: `go test ./internal/api -run '^TestCreateTaskRejectsUnauthorizedBrainProject$' -count=1` and `go test ./internal/store -run '^(TestTaskBrainFieldsRoundTrip|TestSQLiteMigrationUpgradesLegacySchema|TestSQLiteMigrationRollsBackOnIndexFailure)$' -count=1` passed; `git diff --check` clean.
- Task 2: complete (commits `ba458d3..966f8bd`, review clean).
- Task 3 dispatched to `/root/brain_p1_task3_impl`; base `966f8bd550595d7ef5422a66f832473dbc8a71af`; report `task-3-report.md`; implementation commit `c0acacc`.
- Task 3 review: one Critical (persistent SQL list rows omitted traces) and two Important findings (newline byte-bound off-by-one; sparse Redis page premature EOF).
- Task 3: fix round 1/5 (3 addressed, 0 open; commit `42fe4b6`).
- Task 3 controller verification: `go test ./internal/brain -run '^TestSourceReader' -count=1` and `go test ./internal/brain -count=1` passed; worktree clean and `git diff --check` clean.
- Task 3: complete (commits `966f8bd..42fe4b6`, review clean).
- Task 4 dispatched to `/root/brain_p1_task4_impl`; base `42fe4b649674dbdf1892e7b05b4b2b5ce08d8018`; initial implementation commit `4b53340`.
- Task 4 review: two Critical and six Important findings covering descriptor anchoring/no-replace, exact hash-set validation, lock inode safety, preflight bounds, ledger truncation, commit-time retraction coupling, durability-unknown semantics, and genuine regression coverage.
- Task 4: fix round 1/5 (7 production/security findings addressed; regression coverage finding remained open; commit `f765f70`).
- Task 4: fix round 2/5 (1 regression-coverage finding addressed; commit `5c3abc8`).
- Task 4 controller verification: focused race suite, full Brain package tests, vet, and `git diff --check` passed; worktree clean.
- Task 4: complete (commits `42fe4b6..5c3abc8`, review clean).
- Task 5 dispatched to `/root/brain_p1_task5_impl`; base `5c3abc818e2de2fa0da8b39334881310686a25dd`; initial implementation commit `25f0656`.
- Task 5 review: three Critical and four Important findings (file-hash fail-open, zero-evidence ledger bypass, sensitive finding paths, body/frontmatter drift, bounds mismatch, tautological ordering test, and missing regressions); compact-index overflow was a documented Minor/fail-closed decision.
- Task 5: fix round 1/5 (7 Critical/Important findings addressed; commit `0b9bad8`).
- Task 5: fix round 2/5 (body canonical binding and literal-heading parser finding addressed; bounds mismatch remained; commit `0e0e215`).
- Task 5: fix round 3/5 (validator/CreateStage canonical bounds mismatch addressed; commit `1aadc0d`).
- Task 5 controller verification: Brain/Wiki focused `-count=2`, package tests, vet, and `git diff --check` passed; worktree clean.
- Task 5: complete (commits `5c3abc8..1aadc0d`, review clean).
- Task 6: complete (commits `1aadc0d..38d9a69`, review clean after two fix rounds).
- Task 6 controller verification: Brain/config package tests, strict parser/compiler/config targeted tests, vet, and `git diff --check` passed. Native Gemini fake-server test remains sandbox-blocked by IPv6 loopback bind (`[::1]:0`); no provider call was made.
- Task 7 initial commit `62944a4`; review found 4 Important findings.
- Task 7 fix round 1 committed as `6080c46`; staging inspect/verify, status metadata, exact Store dispatch, and lifecycle/security coverage addressed. Final review pending.
- Task 7 fix round 2 committed as `3de224c`; bounded status metadata and staging/status behavior tests added.
- Task 7 fix round 3 committed as `f7c3c79`; directory enumeration is capped before allocation. Final review clean; controller targeted tests/vet/diff-check passed.
- Task 7: complete (commits `1aadc0d..f7c3c79`, review clean).
- Task 8: complete (commits `f842d28..0596d95`, review clean after multiple routing/cache/test fix rounds).
- Task 8 controller verification: focused Brain/tools/server race tests, vet, and diff-check passed; broader package runs remain subject to sandbox IPv6 loopback restrictions.
- Task 9 vertical slice committed `6fe341f`, with review fixes `7f833a9`, `e8a6e54`, `c2839e8`, and `4d169ca`; SnapshotPinner/Engine/JIT/config-drift path reviewed clean. Mode-specific context propagation and read-only page API allowlist remain.
- Task 9 follow-up commits `972b06e`, `17684ad`, `4b9d6f3`, and `1ded095` completed runtime context propagation, Brain page provider wiring, root-drift protection, cross-tenant isolation, and request-level config snapshot. Final review clean.
- Task 10 initial commit `4790c16`; fixes `cd94b6a`, `cf4f039`, `8c9a105`, and `eecdf06` wired lifecycle metrics/OTel, readiness status, cache attribution, retraction counters, docs, and config. Final review found no implementation blocker; focused tests/vet/diff-check pass.
- Task 11 deterministic/offline verification complete (commits `40b76f1..982366c`, including E2E source/store/retraction rebuild coverage and dataset hash record). Focused Brain race tests, vet, and diff-check pass; full-suite unrelated IPv6 httptest listeners remain sandbox-blocked. Live evaluator remains intentionally pending explicit approval for the brief's budget gate (max 288 calls, 60,000 tokens, $1.00 USD).
- Live budget approval received; evaluator preflight attempted with the approved limits but stopped before any provider call because `task_finalizer` credential was absent from the environment. Live gate remains pending credential provisioning; no token/cost usage recorded.
