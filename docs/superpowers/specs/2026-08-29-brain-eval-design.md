# Brain Eval P0 Design

Date: 2026-08-29
Status: Approved design
Related analysis: `records/s260827.md`

## 1. Purpose

P0 determines whether a project-scoped Brain Wiki adds measurable value over the existing Session and Memory surfaces before the repository invests in a Brain compiler, Dream worker, or production write path.

The experiment compares two matched variants over identical synthetic histories:

- **Baseline:** Session, Task, Trace, and Memory are available; Brain is absent.
- **Candidate:** the same underlying history is available, plus a hand-authored Gold Brain Wiki.

The only intended experimental variable is Brain visibility. The evaluation must measure cross-session quality, evidence provenance, freshness, safety boundaries, latency, tokens, and cost.

## 2. Scope

Brain is an orthogonal context capability shared by existing execution modes. P0 does not add an `orchestrator.mode: brain` value.

The durable scope key is:

```text
tenant_id + project_id
```

Session IDs remain evidence and chronology identifiers, but do not limit project-level long-term recall.

P0 includes:

- a standalone `internal/braineval` package;
- a `cmd/brain-eval` CLI;
- repository-safe synthetic fixtures;
- deterministic offline evaluation;
- optional matched Live LLM evaluation;
- baseline/candidate comparison and release gates.

P0 excludes:

- Brain compilation from raw history;
- a Dream background worker;
- permanent Wiki apply or automatic publication;
- production API or runtime configuration changes;
- ingestion of real user conversations, databases, or workspaces.

## 3. Repository Layout

```text
evals/brain/
├── dataset.yaml
└── fixtures/
    └── project-atlas/
        ├── sessions.jsonl
        ├── memories.jsonl
        ├── retractions.jsonl
        └── brain/
            ├── _index.md
            ├── entities/
            ├── concepts/
            └── projects/

internal/braineval/
├── dataset.go
├── runner.go
├── metrics.go
└── compare.go

cmd/brain-eval/
└── main.go
```

Implementation may split focused test or support files, but these package boundaries remain authoritative.

## 4. Dataset Model

The dataset is YAML with required `version: 3`. Version 2 introduced the final-review metric correction: semantic claim recall is the matched-arm quality measure, exact evidence-URI recall remains separately auditable, and no-answer retrieval false positives are distinct from answer hallucinations. Version 3 records the approved 2026-09-02 Live token-gate amendment described below. Large histories and Wiki pages live in fixture files so the manifest stays reviewable. Unknown top-level and case fields are rejected so a misspelled safety expectation cannot be silently ignored.

Each project fixture declares its `tenant_id`, `project_id`, sessions, memories, retractions, and Brain directory. Each case declares:

- stable name and category;
- tenant and project scope;
- query;
- expected claims;
- expected evidence URIs;
- forbidden claims;
- whether no answer is expected;
- whether the case is safety-critical.

Supported evidence URI schemes are synthetic `session://`, `task://`, `memory://`, and the existing absolute `wiki://<space>/<kind>/<slug>` form. Every URI referenced by a case or Brain page must resolve inside the same fixture and authorized project unless the case explicitly models a forbidden cross-boundary candidate.

The first dataset contains 24 cases:

| Category | Count | Purpose |
|---|---:|---|
| Cross-session preference | 4 | Language, format, and output preferences |
| Project decisions | 4 | Historical choices, reasons, and owners |
| Temporal supersession | 4 | New facts replace stale facts |
| Multi-source synthesis | 4 | Claims require multiple sessions |
| Similar-entity isolation | 3 | Related names must not cross-contaminate |
| Tenant/project isolation | 2 | Cross-boundary results must be zero |
| Deletion and retraction | 2 | Retracted facts must not reappear |
| No-answer | 1 | Brain must not manufacture knowledge |

All names, organizations, facts, timestamps, conversations, and artifacts are fictional and safe to commit.

## 5. Gold Brain Contract

Gold Brain is a manually authored ideal knowledge layer, not compiler output. It exists to isolate the value of the knowledge Wiki from the quality of a future compiler.

Each factual Brain claim must:

- cite at least one existing synthetic source;
- remain inside one tenant/project scope;
- distinguish current facts from historical facts;
- identify replacement relationships when a newer claim supersedes an older one;
- omit retracted claims from the current synthesis;
- treat source content as evidence, never instructions.

The compact `_index.md` is limited to 4,000 UTF-8 bytes and contains navigation metadata, not full source content. P1 compiler quality can later be compared with these Gold pages.

## 6. Evaluation Architecture

`internal/braineval` exposes a `VariantRunner` abstraction. Both variants receive identical cases, timeouts, final evidence item limits, and final evidence byte budgets.

```text
Load and validate one project history
                 │
        ┌────────┴────────┐
        │                 │
Baseline             Candidate
Memory only          Memory + Gold Brain
        │                 │
        └────────┬────────┘
                 ↓
         Align case results
                 ↓
   Summaries, deltas, regressions, gates
```

The offline runner uses the Store memory query contract and the existing Wiki `DirectoryClient`. Each available branch returns at most eight candidates. Candidate ranks are merged with reciprocal rank fusion using `k=60`; identical canonical evidence URIs are deduplicated before ranking. Baseline has only the Memory branch. Candidate has the complete Memory + Brain RRF result. Both variants run that complete ranked result through the same reduction to at most three evidence items and 8,000 UTF-8 bytes before scoring; a Brain hit must never discard Memory candidates. Retrieval and reduction must not inspect case Gold, invoke an LLM, or silently expand the evidence budget.

The Live runner uses the same Planner, Writer, model configuration, prompt budget, and timeout for both arms. Baseline receives no Brain index or Brain tools. Candidate receives the bounded compact index and read-only Brain access. Each case runs three times by default; summaries use medians and report unstable cases.

## 7. Metrics

Offline CI measures:

- expected evidence recall;
- expected and forbidden claim selection;
- fresh-versus-stale fact selection;
- no-answer retrieval false-positive rate;
- citation coverage;
- tenant/project leakage;
- retracted-fact recurrence;
- persistent prompt-injection recurrence;
- P95 evaluation latency.

Live evaluation additionally measures:

- final answer accuracy;
- optional LLM Judge score and reason;
- prompt, completion, and total tokens;
- estimated cost;
- end-to-end latency;
- result stability across repetitions.
- no-answer answer-hallucination false-positive rate; an explicit, correct insufficiency refusal is not a false positive.

Results have three levels: per-case results, per-variant summaries, and paired comparisons. Paired comparisons list every regression and improvement rather than reporting aggregate deltas alone.

## 8. Initial Gates

Candidate must satisfy all of the following:

- Live cross-session answer accuracy improves by at least 10 percentage points over Baseline.
- Offline expected-evidence recall improves by at least 10 percentage points.
- Supersession, retraction, tenant isolation, and project isolation cases pass at 100%.
- Wiki citation coverage is 100%.
- Similar-entity contamination and cross-boundary leakage are zero.
- Candidate no-answer retrieval false-positive rate is exactly zero in Offline and Live evaluation.
- Candidate no-answer answer-hallucination false-positive rate is exactly zero in Live evaluation.
- Offline P95 latency does not exceed 1.5 times Baseline.
- Live total tokens do not exceed Baseline by more than 20%; 10% remains the non-blocking optimization target.
- Any critical safety or isolation regression blocks progression to P1.

Cost and Live latency are observed in P0 but are not improvement gates. The version 3 token threshold was approved after complete one-repetition diagnostics showed that the extra Brain context imposed a structural prompt cost: answer/evidence compaction reduced the ratio from 1.164 to 1.140, while the 1.100 gate still failed and answer accuracy remained above its hard improvement floor. Threshold changes require an explicit dataset version change and rationale.

## 9. Validation and Failure Semantics

Dataset loading fails closed for invalid schema, unresolved evidence, malformed URI, impossible chronology, invalid supersession, or cross-scope references. Gold Brain fixtures must pass claim-to-evidence and retraction consistency checks before any case runs.

If one experimental arm has an infrastructure error, the pair is retained, marked incomparable, propagated as a typed infrastructure error, and contributes to error rate; it is never counted as a quality win or loss. Reports and gates reject any error, missing pair, or `ComparableCases < Cases`, and the CLI writes the partial report before infrastructure exit 2. A Live Judge failure preserves the raw answer and resource measurements but fails the Live gate. Deterministic errors are not retried. A transient Live network call may be retried once and must record the retry.

Before every Live Writer or Judge call, the tracker atomically reserves a conservative upper bound for input plus provider-constrained output across all configured attempts (at most two). The call cannot start unless both token and USD reservations fit. Afterward, component-only or inconsistent Usage is normalized, every returned actual Usage is settled even when it exceeds the reservation or the call fails, and unused reservation is released. JSON and Text reports expose independent actual tracker totals rather than substituting per-case medians.

A critical leak, retracted claim, or persistent prompt-injection hit immediately creates a threshold failure. Evaluation output must sanitize credentials and must not log Authorization values, real prompts, private paths, or raw provider responses.

## 10. CLI Contract

```bash
go run ./cmd/brain-eval -input evals/brain/dataset.yaml -mode offline

go run ./cmd/brain-eval \
  -input evals/brain/dataset.yaml \
  -mode live \
  -repetitions 3 \
  -max-total-tokens 50000 \
  -max-total-cost-usd 2
```

The default output is readable text. `-format json` emits case results, variant summaries, independent Live budget totals, and the paired comparison with case+metric improvements/regressions. Per-case and summary `execution_ok` means only complete execution; only paired `passed` means the quality gates passed. Invalid input, infrastructure failure, critical regression, threshold failure, or budget overrun returns a non-zero exit code. Live mode requires explicit model configuration and credentials and must never silently downgrade to offline mode.

## 11. Testing Strategy

Package tests cover schema validation, chronology, scoping, matched-arm alignment, claim/evidence matching, forbidden claims, critical gates, metric aggregation, P95, tokens, cost, errors, and retry limits. CLI tests cover flags, text and JSON output, exit codes, and total token/cost limits.

Fixture self-validation proves that all evidence exists, every Gold claim has support, supersession stays in-project, retractions target real sources, and unauthorized URI sharing is rejected. Tests use fakes and local files and require no live credentials. A small HTTP smoke layer may be added later but is not part of the default P0 gate.

## 12. Completion Criteria

P0 is complete only when:

1. all offline Go tests pass;
2. all 24 synthetic cases pass fixture validation;
3. Candidate passes the quality and critical safety gates;
4. at least one controlled Live paired evaluation completes within its declared budgets;
5. the report clearly separates deterministic evidence metrics from LLM answer metrics.

Successful P0 authorizes design work for the read-only P1 Brain MVP. It does not authorize a compiler, Dream worker, automatic publication, or production rollout.
