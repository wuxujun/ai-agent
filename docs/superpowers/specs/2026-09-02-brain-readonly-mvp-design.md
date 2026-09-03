# Brain Read-Only MVP P1 Design

Date: 2026-09-02
Status: Approved design
Related analysis: `records/s260827.md`
P0 evidence: `records/brain-eval-p0.md`

## 1. Purpose

P1 turns the successful hand-authored Brain experiment into a production-shaped,
read-only capability. It compiles eligible Task, Trace, Memory, and retraction data
into a project-scoped Brain Wiki, stages a complete immutable snapshot, validates
it deterministically, and publishes it only through an explicit operator CLI.

Brain remains an orthogonal context layer shared by `eino`, `step`, and
`multiagent`. P1 does not add an `orchestrator.mode: brain` value.

The durable scope key is:

```text
tenant_id + brain_project_id
```

Session IDs remain provenance and chronology identifiers. They do not limit
project-level long-term recall.

## 2. Decisions and Non-Goals

P1 uses these approved decisions:

- publication is CLI-only; HTTP remains read-only;
- tasks carry an explicit `brain_project_id` instead of deriving identity from a
  workspace path;
- publication uses complete immutable snapshots, not page-by-page mutation or a
  runtime overlay;
- Gemini may synthesize proposals through the existing LLM abstraction, using
  `GEMINI_API_KEY`, but it can never publish;
- deterministic validation and explicit human action gate every publication;
- existing `wiki_search`, `wiki_fetch`, and graph tools expose Brain pages; P1
  does not register duplicate Brain tools.

P1 excludes Dream/background scheduling, incremental compilation, autonomous
Agent writes, Connector refresh, low-risk auto-publish, and physical storage
erasure automation. Those require separate design and approval.

## 3. Architecture

```text
Task Store / Trace / Memory / Retraction
                  |
                  v
           Brain Source Reader
                  |
                  v
        Normalizer + Claim Builder
                  |
                  v
        Immutable Staging Snapshot
                  |
                  v
 Validator -> Proposal Manifest -> CLI Publish
                                      |
                                      v
                         Published Brain Snapshot
                                      |
                                      v
                    Existing wiki_search/wiki_fetch
```

### 3.1 `ProjectResolver`

`ProjectResolver` resolves an authenticated `TenantID + BrainProjectID` through
the tenant allowlist. It returns the managed storage key, published Wiki space,
and policy snapshot. Unknown projects, cross-tenant references, invalid IDs, and
missing configuration fail closed. Workspace paths are never project identity.

### 3.2 `SourceReader`

`SourceReader` obtains a bounded source snapshot from the Store. It accepts only
completed tasks bound to the same Tenant and Brain project, with a present Answer
Audit whose result is publishable. Failed, partial, unaudited, and legacy
unassigned tasks are excluded by default.

Memory and FinalAnswer may discover candidate topics, but neither can prove a
claim. Every emitted claim must reference successful Trace evidence that passed
the existing Evidence Filter. Retractions are applied before any model call.

### 3.3 `Compiler`

The compiler first creates deterministic, sanitized evidence records. An optional
Gemini synthesis stage then proposes pages under `concepts/`, `entities/`,
`projects/`, and `sources/`. Every claim records evidence references, observation
time, confidence, and supersession/retraction state. `_index.md` is compact and
derived; it is never a primary source.

### 3.4 `Validator`

The validator checks Markdown/frontmatter, content bounds, claim-to-evidence
coverage, evidence existence, project isolation, retraction recurrence, link
integrity, prompt-injection findings, and all hashes. Any hard-gate failure leaves
the snapshot non-publishable.

### 3.5 `SnapshotRepository`

The repository owns immutable staging and release trees, manifests, content
hashes, parent versions, and the `CURRENT` pointer. Publish and rollback use an
expected-current compare-and-swap and an atomic pointer replacement on the same
filesystem. Old verified releases remain available unless revoked.

### 3.6 `BrainWikiProvider`

The provider reads only a verified release and adapts it to the existing Wiki
client/tool contracts. It does not write. Candidate caches are scoped by task,
tenant, project, snapshot, and corpus.

## 4. Configuration and Task Contract

Brain is disabled by default:

```yaml
brain:
  enabled: false
  root: ./data/brain
  compact_index_max_bytes: 4000
  compiler:
    provider: gemini
    model: gemini-3.5-flash-lite
    max_input_bytes: 200000
    max_output_tokens: 12000
    max_cost_usd: 0.25
```

Each tenant explicitly allows projects and their public Wiki spaces:

```yaml
api:
  auth:
    tenants:
      tenant-a:
        brain_projects:
          atlas:
            wiki_space: brain-atlas
```

Task creation accepts optional `brain_project_id`. The durable Task additionally
stores `BrainProjectID`, `BrainSnapshotID`, and `BrainConfigDigest`. All Store
backends must round-trip these fields. A missing project ID means no Brain is
loaded. An unknown or unauthorized project ID rejects task creation.

## 5. Snapshot and Provenance Model

The managed layout is:

```text
data/brain/<tenant-key>/<project-id>/
|-- CURRENT
|-- staging/<snapshot-id>/
|   |-- wiki/
|   |-- evidence.jsonl
|   `-- manifest.json
`-- releases/<snapshot-id>/
    |-- wiki/
    |-- evidence.jsonl
    `-- manifest.json
```

`tenant-key` is a stable hash of the tenant identifier, preventing traversal and
avoiding tenant names in storage paths. Project IDs use a strict slug grammar.

The manifest records snapshot and parent IDs, tenant/project identity, source
cutoff, source IDs and hashes, retraction watermark, model and prompt versions,
configuration digest, token/cost usage, file hashes, validation results, and the
expected current release. Evidence records use immutable
`brain-evidence://<tenant>/<project>/tasks/<task>#trace/<step>` identifiers and
contain only the bounded, sanitized material needed for audit.

Final answers cite full `wiki://<brain-space>/<kind>/<slug>` page URIs. Claims in
those pages link to Brain evidence records, giving a two-hop chain from answer to
page to source. Raw Brain evidence URIs are not final-answer citations.

## 6. CLI and Publication Flow

The CLI surface is:

```text
brain-compile build --tenant <tenant> --project <project>
brain-compile inspect --tenant <tenant> --project <project> --snapshot <id>
brain-compile verify --tenant <tenant> --project <project> --snapshot <id>
brain-compile publish --tenant <tenant> --project <project> --snapshot <id> --expected-current <parent-id>
brain-compile rollback --tenant <tenant> --project <project> --to <release-id> --expected-current <current-id>
brain-compile status --tenant <tenant> --project <project>
```

`build` captures a source cutoff and exact source hashes, applies retractions,
normalizes and sanitizes evidence, invokes Gemini within configured call/token/
cost/time/output limits, renders a complete staging Wiki, and writes a proposal
manifest. Model output is always untrusted.

`verify` runs all deterministic gates. `publish` repeats integrity validation,
checks the current release CAS and latest retraction watermark, moves the verified
tree into releases on the same filesystem, then atomically updates `CURRENT`.
Any mismatch requires a new build. `rollback` accepts only a verified, non-revoked
release and uses the same CAS rule.

Build and verify failures affect only staging. A failed pointer update leaves the
old release active. Startup ignores incomplete staging and never resumes or
publishes it automatically.

## 7. Foreground Retrieval and Snapshot Pinning

`wiki_search` gains an optional `corpus` argument:

- `wiki` searches the existing Wiki and remains the default;
- `brain` searches the task's pinned Brain release;
- `all` queries both independently and performs a bounded deterministic merge.

`wiki_fetch` continues to accept only candidate IDs created by a prior search in
the same execution scope. Cache keys bind `task_id`, `tenant_id`,
`brain_project_id`, `brain_snapshot_id`, and `corpus`.

Before first execution, the runtime resolves `CURRENT` and durably pins the
snapshot ID and Brain configuration digest on the Task. Every later step and
resume uses that release. A missing, corrupt, revoked, or policy-incompatible
pinned release fails closed. Only a new Task observes a newly published release.

The planner initially receives only the bounded compact index, pinned snapshot
ID, and corpus instructions. Full pages are loaded progressively through Search
then Fetch. `eino`, `step`, and `multiagent` use the same pinning and tool context.

The read-only page API authorizes the tenant's ordinary Wiki space plus Brain
spaces in its project allowlist. It provides no build, publish, or rollback route.

## 8. Security, Retraction, and Failure Policy

Evidence is sanitized and bounded before model input. Existing prompt-injection
and secret detectors run before synthesis and again on generated pages. Instruction
override content, credentials, private paths, cross-scope URIs, missing citations,
and unsupported links fail validation. Logs, errors, metrics, and manifests do not
contain prompts, raw model responses, credentials, or full evidence bodies.

A project-level append-only retraction ledger overrides snapshot pinning and
rollback. Every fetch consults the live ledger. A new retraction invalidates
affected candidate caches, blocks affected claims even for running tasks, and
marks releases containing the source as revoked. Revoked releases cannot become
`CURRENT` or rollback targets. The next build must omit the claim.

P1 guarantees that retracted data cannot be retrieved, cited, or restored through
rollback. Physical-media erasure remains an explicit retention/destruction
workflow outside P1 and must not be represented as completed by logical revocation.

Concurrent builds may exist, but publish is serialized per Tenant/Project and
uses CAS. Reads reject symlink escapes, traversal, oversized files, and release
trees outside the configured root. Cross-filesystem publication is rejected.

## 9. Observability

P1 adds bounded metrics for compile counts/duration, publish outcomes/conflicts,
snapshot age, retraction blocks, search/fetch counts, and cache hits. Labels are
limited to low-cardinality values such as outcome, corpus, and provider; tenant,
project, task, snapshot, prompts, and content are forbidden labels.

Structured logs preserve request/trace identifiers where available and emit only
sanitized IDs, state transitions, hashes, counts, duration, and error categories.

## 10. Verification and Acceptance Gates

Tests must cover configuration validation and rejected reload preservation; Task
field persistence and migration in SQLite/Postgres/Redis; resolver isolation;
source eligibility; deterministic normalization; validation failures; CLI build,
verify, publish, rollback, concurrent CAS, crash injection, and symlink boundaries;
snapshot pinning and resume in every execution mode; cache scope; read-only API
authorization; retraction precedence; and metrics cardinality.

Automated tests use a fake Gemini client and must not require credentials or incur
cost. Live evaluation reads `GEMINI_API_KEY` only after explicit approval.

The release gate requires:

- zero behavior change when Brain is disabled;
- zero scope leak, entity contamination, retraction recurrence, prompt-injection
  recurrence, and no-answer hallucination;
- matched Live answer-accuracy delta at least `+0.10`;
- Brain/Baseline token ratio at most `1.20`, with `1.10` as a non-blocking target;
- Brain/Baseline P95 latency ratio at most `1.20`;
- passing `go test ./...`, targeted race tests, `go vet ./...`, and
  `git diff --check`.

Successful P1 authorizes a separate design discussion for the P2 Dream Proposal
Worker. It does not authorize background scheduling or automatic publication.
