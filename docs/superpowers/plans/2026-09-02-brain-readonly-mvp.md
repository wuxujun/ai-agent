# Brain Read-Only MVP P1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a project-scoped, read-only Brain Wiki that compiles eligible durable task evidence into validated immutable snapshots and publishes them only through an explicit operator CLI.

**Architecture:** A new `internal/brain` package resolves tenant/project scope, reads and normalizes eligible Store evidence, optionally asks Gemini for structured page proposals, validates a complete staging snapshot, and publishes it with CAS plus an atomic `CURRENT` pointer. Existing Wiki tools gain a corpus selector and read Brain through a composite provider; each task durably pins one Brain snapshot while a live retraction ledger remains authoritative.

**Tech Stack:** Go 1.25, Gin, existing Store backends (memory/SQLite/Postgres/Redis), existing structured LLM runtime with Gemini, local Wiki directory reader, OpenTelemetry, YAML/JSON/JSONL.

**Spec:** `docs/superpowers/specs/2026-09-02-brain-readonly-mvp-design.md`

## Global Constraints

- Brain is disabled by default and must cause zero API, prompt, routing, or tool-schema behavior change when disabled.
- The scope key is exactly `tenant_id + brain_project_id`; never infer project identity from `workspace`.
- HTTP remains read-only. Build, verify, publish, rollback, and status are CLI-only.
- Runtime content is immutable by snapshot; publish and rollback require expected-current CAS.
- Retraction checks override task snapshot pinning, cache entries, and rollback eligibility.
- Gemini uses the existing structured LLM runtime and `GEMINI_API_KEY`; unit and integration tests use fakes and spend no provider money.
- Never log prompts, raw provider responses, API keys, full evidence bodies, authorization data, or private paths.
- Do not add a Brain orchestrator mode or duplicate `brain_search`/`brain_fetch` tools.
- Preserve the user's unrelated `build.sh` modification and never bulk-delete files or directories.
- Every test helper named in a snippet is implemented locally in that task's `_test.go`; do not add production-only helpers solely to support tests.

---

## File Map

**Create:**

- `internal/brain/model.go`: project, evidence, claim, page, manifest, and validation contracts.
- `internal/brain/project.go`: tenant/project resolution and configuration digest.
- `internal/brain/source.go`: paged Store reader and source eligibility rules.
- `internal/brain/retraction.go`: append-only ledger reader, watermark, and revocation checks.
- `internal/brain/snapshot.go`: safe layout, staging/release reads, CAS publish, and rollback.
- `internal/brain/render.go`: deterministic Markdown and compact-index rendering.
- `internal/brain/validate.go`: structural, provenance, isolation, link, and injection gates.
- `internal/brain/compiler.go`: bounded structured synthesis and staging orchestration.
- `internal/brain/provider.go`: snapshot-backed read-only Wiki provider and task pinning.
- `internal/brain/metrics.go`: bounded local and OpenTelemetry Brain metrics.
- `cmd/brain-compile/main.go`: operator CLI and exit-code contract.
- Focused `*_test.go` files beside every new implementation file.

**Modify:**

- `internal/config/config.go`, `internal/config/config_test.go`, `config.yaml`, `config_zh.yml`.
- `internal/types/task.go`.
- `internal/store/sqlite.go`, `internal/store/postgres.go`, `internal/store/store_test.go`, `internal/store/migration_test.go`, `internal/store/external_integration_test.go`.
- `internal/api/handler.go`, `internal/api/handler_test.go`, `internal/api/wiki.go`, `internal/api/wiki_test.go`.
- `internal/tools/retrieval.go`, `internal/tools/retrieval_test.go`, `internal/tools/wiki.go`, `internal/tools/wiki_test.go`, `internal/tools/wiki_resilience.go`, `internal/tools/wiki_cache_test.go`.
- `internal/orchestrator/engine.go`, `internal/orchestrator/rag_test.go`, `internal/orchestrator/eino.go`, `internal/orchestrator/adk.go`.
- `internal/executor/executor.go`, `internal/multiagent/coordinator.go`, `internal/multiagent/jit_retrieval_test.go`.
- `internal/planner/jit_policy.go`, `internal/planner/jit_policy_test.go`, `internal/planner/prompt.go`.
- `internal/wiki/writeback.go`, `internal/wiki/writeback_test.go`.
- `cmd/server/build.go`, `cmd/server/main.go`, `cmd/server/wiki_builder.go`, and focused server tests.
- `Sample/agent-api.http`, `README.md`, and the final verification record under `records/`.

---

### Task 1: Brain Configuration and Project Resolution

**Files:**
- Create: `internal/brain/project.go`
- Create: `internal/brain/project_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.yaml`
- Modify: `config_zh.yml`

**Interfaces:**
- Produces: `config.BrainConfig`, `config.BrainCompilerConfig`, `config.BrainProjectConfig`.
- Produces: `brain.ResolveProject(*config.Config, string, string) (brain.ProjectRef, error)` and `brain.ProjectConfigDigest(ProjectRef) string`.

- [ ] **Step 1: Write failing configuration and resolver tests**

```go
func TestBrainDefaultsDisabled(t *testing.T) {
    cfg := loadTestConfig(t, "")
    if cfg.Brain.Enabled || cfg.Brain.CompactIndexMaxBytes != 4000 { t.Fatalf("brain defaults = %+v", cfg.Brain) }
}

func TestResolveProjectRejectsCrossTenantAndInvalidSlug(t *testing.T) {
    cfg := brainConfigFixture()
    if _, err := brain.ResolveProject(cfg, "tenant-b", "atlas"); err == nil { t.Fatal("expected tenant isolation error") }
    if _, err := brain.ResolveProject(cfg, "tenant-a", "../atlas"); err == nil { t.Fatal("expected slug error") }
}

func TestReloadRejectsInvalidBrainCandidateAndPreservesPrevious(t *testing.T) {
    before := config.Get()
    err := applyReloadedFixture(t, `brain: {enabled: true, root: ""}`)
    if err == nil || config.Get() != before { t.Fatal("invalid reload replaced active config") }
}
```

- [ ] **Step 2: Run tests and confirm the missing contracts fail**

Run: `go test ./internal/config ./internal/brain -run 'TestBrain|TestResolveProject|TestReload.*Brain'`
Expected: FAIL because the Brain config and package do not exist.

- [ ] **Step 3: Add the minimal config and resolver contracts**

```go
type BrainCompilerConfig struct {
    Provider, Model string
    MaxInputBytes, MaxOutputTokens int
    MaxCostUSD float64
}
type BrainConfig struct { Enabled bool; Root string; CompactIndexMaxBytes int; Compiler BrainCompilerConfig }
type BrainProjectConfig struct { WikiSpace string `mapstructure:"wiki_space"` }
// APITenantConfig gains BrainProjects map[string]BrainProjectConfig.

type ProjectRef struct { TenantID, ProjectID, WikiSpace, StorageKey string }
func ResolveProject(cfg *config.Config, tenantID, projectID string) (ProjectRef, error)
func ProjectConfigDigest(ref ProjectRef) string
```

Validate non-negative limits, finite cost, `gemini` provider, strict project slugs, unique non-empty Brain Wiki spaces per tenant, and a non-empty root when enabled. Add defaults and clone/change-detection support. Document `brain.root` as restart-required.

- [ ] **Step 4: Run focused config tests**

Run: `go test ./internal/config ./internal/brain -run 'TestBrain|TestResolveProject|TestReload.*Brain'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/brain/project.go internal/brain/project_test.go config.yaml config_zh.yml
git commit -m "feat: add brain project configuration"
```

### Task 2: Durable Task Identity and API Admission

**Files:**
- Modify: `internal/types/task.go`
- Modify: `internal/api/handler.go`
- Modify: `internal/api/handler_test.go`
- Modify: `internal/store/sqlite.go`
- Modify: `internal/store/postgres.go`
- Modify: `internal/store/store_test.go`
- Modify: `internal/store/migration_test.go`
- Modify: `internal/store/external_integration_test.go`
- Modify: `Sample/agent-api.http`

**Interfaces:**
- Consumes: `brain.ResolveProject` from Task 1.
- Produces: durable `Task.BrainProjectID`, `Task.BrainSnapshotID`, and `Task.BrainConfigDigest` fields.
- Produces: request JSON field `brain_project_id`.

- [ ] **Step 1: Add failing API and Store round-trip tests**

```go
func TestCreateTaskRejectsUnauthorizedBrainProject(t *testing.T) {
    w := createTask(t, "tenant-a-key", `{"goal":"x","workspace":"./testdata","brain_project_id":"orbit"}`)
    if w.Code != http.StatusForbidden { t.Fatalf("status = %d", w.Code) }
}

func TestTaskBrainFieldsRoundTrip(t *testing.T) {
    task := &types.Task{ID: "brain-task", TenantID: "tenant-a", BrainProjectID: "atlas", BrainSnapshotID: "snap-1", BrainConfigDigest: "sha256:test"}
    assertTaskRoundTrip(t, task, func(got *types.Task) { if got.BrainSnapshotID != "snap-1" { t.Fatalf("got %+v", got) } })
}
```

- [ ] **Step 2: Run tests and observe missing JSON/columns**

Run: `go test ./internal/api ./internal/store -run 'TestCreateTask.*Brain|TestTaskBrainFields|TestMigration'`
Expected: FAIL because request, Task, and SQL columns are absent.

- [ ] **Step 3: Implement fields, admission, and persistence**

```go
// Append these fields to the existing types.Task definition.
BrainProjectID    string `json:"brain_project_id,omitempty"`
BrainSnapshotID   string `json:"brain_snapshot_id,omitempty"`
BrainConfigDigest string `json:"brain_config_digest,omitempty"`
```

Trim and validate `CreateTaskRequest.BrainProjectID`; call `ResolveProject` using the authenticated tenant; return 403 for unknown/foreign projects. Add three `TEXT NOT NULL DEFAULT ''` columns to SQLite/Postgres schema, transactional migrations, every insert/update/select/scan path, and SQL fixtures. Redis needs no schema change but must have a JSON round-trip regression test. Do not infer from Workspace.

- [ ] **Step 4: Verify old rows and all backends**

Run: `go test ./internal/api ./internal/store -run 'TestCreateTask.*Brain|TestTaskBrainFields|TestMigration'`
Expected: PASS, including empty values for pre-P1 rows.

When dedicated external services are configured, also run:

```bash
AI_AGENT_RUN_EXTERNAL_INTEGRATION=true go test ./internal/store -run TestExternalStoresSessionLeaseAndIsolation
```

Expected: Postgres and Redis subtests PASS; an unconfigured backend is explicitly skipped.

- [ ] **Step 5: Commit**

```bash
git add internal/types/task.go internal/api/handler.go internal/api/handler_test.go internal/store/sqlite.go internal/store/postgres.go internal/store/store_test.go internal/store/migration_test.go internal/store/external_integration_test.go Sample/agent-api.http
git commit -m "feat: persist brain task identity"
```

### Task 3: Provenance Model and Eligible Source Reader

**Files:**
- Create: `internal/brain/model.go`
- Create: `internal/brain/source.go`
- Create: `internal/brain/source_test.go`

**Interfaces:**
- Consumes: `store.Store.ListTasks`, `types.Task`, and `brain.ProjectRef`.
- Produces: `SourceReader.Read(context.Context, ProjectRef, time.Time) (SourceSet, error)`.
- Produces: `EvidenceRecord`, `Claim`, `Page`, `Manifest`, `ValidationReport`, and `Retraction` JSON contracts.

- [ ] **Step 1: Write failing eligibility and provenance tests**

```go
func TestSourceReaderKeepsOnlyPublishableCompletedProjectTasks(t *testing.T) {
    got, err := newFixtureReader(t).Read(t.Context(), atlasRef(), cutoff)
    if err != nil { t.Fatal(err) }
    ids := sourceTaskIDs(got)
    if len(ids) != 1 || ids[0] != "eligible" { t.Fatalf("task ids = %v", ids) }
}

func TestSourceReaderRequiresTraceEvidenceForClaims(t *testing.T) {
    got, _ := newFixtureReaderWithAnswerOnly(t).Read(t.Context(), atlasRef(), cutoff)
    if len(got.Evidence) != 0 { t.Fatalf("answer became evidence: %+v", got.Evidence) }
}
```

Define `newFixtureReader`, `atlasRef`, and `sourceTaskIDs` as test-local helpers in `source_test.go`; use only the standard library and existing dependencies.

- [ ] **Step 2: Confirm tests fail**

Run: `go test ./internal/brain -run 'TestSourceReader'`
Expected: FAIL because source contracts do not exist.

- [ ] **Step 3: Implement bounded paging and stable evidence IDs**

```go
type TaskSourceStore interface { ListTasks(context.Context, store.ListFilter) ([]*types.Task, error) }
type SourceReader struct { Store TaskSourceStore; PageSize int }
func NewSourceReader(source TaskSourceStore) *SourceReader
func (r *SourceReader) Read(ctx context.Context, ref ProjectRef, cutoff time.Time) (SourceSet, error)

type EvidenceRecord struct {
    ID, URI, TaskID, TraceStep, Content, ContentHash string
    ObservedAt time.Time
}
type Claim struct {
    ID, Text, Confidence, State string
    EvidenceIDs []string
    ObservedAt time.Time
}
type Page struct { Kind, Slug, Title, Summary string; Claims []Claim; Links []string }
type Synthesis struct { Pages []Page }
type SourceSet struct { Evidence []EvidenceRecord; SourceIDs, SourceHashes []string; Cutoff time.Time }
type ValidationFinding struct { Code, Path, Message string; Hard bool }
type ValidationReport struct { Publishable bool; Findings []ValidationFinding }
type Manifest struct {
    SnapshotID, ParentID, TenantID, ProjectID, ExpectedCurrent string
    SourceCutoff time.Time
    SourceIDs, SourceHashes []string
    RetractionWatermark, Model, PromptVersion, ConfigDigest string
    Usage types.TokenUsage
    EstimatedCostUSD float64
    FileHashes map[string]string
    Validation ValidationReport
}
type SnapshotDraft struct { Files map[string][]byte; Evidence []EvidenceRecord; Manifest Manifest }
type Release struct { Root string; Manifest Manifest }
type Retraction struct { EvidenceURI, Reason string; RetractedAt time.Time }
```

Page through completed tasks with a maximum page size of 500; filter exact Tenant/Project, `UpdatedAt <= cutoff`, non-nil publishable audit, successful eligible Trace entries, non-empty Evidence, and configured byte/count bounds. Use `evidencefilter.Eligible`, `sanitize.Secrets`, canonical URI validation, stable SHA-256 IDs, deterministic sorting, and copies rather than caller-owned slices. Memory and FinalAnswer remain discovery hints only.

- [ ] **Step 4: Run source tests, including cancellation and bounds**

Run: `go test ./internal/brain -run 'TestSourceReader'`
Expected: PASS with no cross-project source and no answer-only evidence.

- [ ] **Step 5: Commit**

```bash
git add internal/brain/model.go internal/brain/source.go internal/brain/source_test.go
git commit -m "feat: normalize eligible brain evidence"
```

### Task 4: Retraction Ledger and Immutable Snapshot Repository

**Files:**
- Create: `internal/brain/retraction.go`
- Create: `internal/brain/retraction_test.go`
- Create: `internal/brain/snapshot.go`
- Create: `internal/brain/snapshot_test.go`

**Interfaces:**
- Consumes: `ProjectRef`, `Manifest`, and `Retraction` from Tasks 1 and 3.
- Produces: `RetractionView.Watermark`, `RetractionView.Contains`, and `Repository` lifecycle methods.

- [ ] **Step 1: Write failing ledger, CAS, rollback, and path-safety tests**

```go
func TestPublishUsesExpectedCurrentCAS(t *testing.T) {
    repo := newTestRepository(t)
    stageVerified(t, repo, "snap-2", "snap-1")
    if _, err := repo.Publish(t.Context(), atlasRef(), "snap-2", "wrong"); !errors.Is(err, ErrCurrentConflict) { t.Fatalf("err = %v", err) }
}

func TestRetractionRevokesOldRelease(t *testing.T) {
    repo, ledger := publishedFixture(t, "task://tenant-a/eligible")
    appendRetractionFixture(t, ledger, "task://tenant-a/eligible")
    if _, err := repo.OpenRelease(t.Context(), atlasRef(), "snap-1"); !errors.Is(err, ErrSnapshotRevoked) { t.Fatalf("err = %v", err) }
}
```

Also test malformed/truncated JSONL, duplicate events, symlink components, `..`, absolute child paths, cross-filesystem rename rejection, concurrent publishers with one winner, and a failed `CURRENT` replacement preserving the old value.

- [ ] **Step 2: Run the failing repository tests**

Run: `go test ./internal/brain -run 'Test(Publish|Rollback|Retraction|Repository|Snapshot)'`
Expected: FAIL because repository and ledger types are missing.

- [ ] **Step 3: Implement the read-only ledger and safe repository**

```go
type RetractionView interface {
    Watermark(context.Context, ProjectRef) (string, error)
    Contains(context.Context, ProjectRef, string) (bool, error)
}
type FileRetractionLedger struct { root string }
func NewFileRetractionLedger(root string) *FileRetractionLedger
type Repository struct { root string; ledger RetractionView; rename func(string, string) error }
func NewRepository(root string, ledger RetractionView) (*Repository, error)
func (r *Repository) Current(context.Context, ProjectRef) (string, error)
func (r *Repository) CreateStage(context.Context, ProjectRef, SnapshotDraft) (Manifest, error)
func (r *Repository) OpenRelease(context.Context, ProjectRef, string) (Release, error)
func (r *Repository) Publish(context.Context, ProjectRef, string, string) (Manifest, error)
func (r *Repository) Rollback(context.Context, ProjectRef, string, string) (Manifest, error)
```

Read the operator-managed append-only `retractions.jsonl` at the resolved Project root; hash canonical valid records for the watermark. Check every path component with `Lstat`, reject symlinks and escapes, use `0600` files/`0700` directories where newly created, fsync written files and parent directories, rename only within one project filesystem, and replace `CURRENT` through a temporary file plus atomic rename. Do not add cleanup that recursively deletes staging or releases.

- [ ] **Step 4: Run repository tests and the race detector**

Run: `go test -race ./internal/brain -run 'Test(Publish|Rollback|Retraction|Repository|Snapshot)'`
Expected: PASS; concurrent CAS has one success and one `ErrCurrentConflict`.

- [ ] **Step 5: Commit**

```bash
git add internal/brain/retraction.go internal/brain/retraction_test.go internal/brain/snapshot.go internal/brain/snapshot_test.go
git commit -m "feat: add immutable brain snapshots"
```

### Task 5: Deterministic Rendering and Validation Gates

**Files:**
- Create: `internal/brain/render.go`
- Create: `internal/brain/render_test.go`
- Create: `internal/brain/validate.go`
- Create: `internal/brain/validate_test.go`
- Modify: `internal/wiki/writeback.go`
- Modify: `internal/wiki/writeback_test.go`

**Interfaces:**
- Consumes: `EvidenceRecord`, `Claim`, `Page`, `Manifest`, and `RetractionView`.
- Produces: `Render(Synthesis, int) (SnapshotDraft, error)` and `Validate(context.Context, ProjectRef, SnapshotDraft, RetractionView) ValidationReport`.

- [ ] **Step 1: Write failing deterministic renderer and hard-gate tests**

```go
func TestRenderIsByteStableAndBuildsCompactIndex(t *testing.T) {
    first, _ := Render(validSynthesis(), 4000)
    second, _ := Render(validSynthesis(), 4000)
    if !reflect.DeepEqual(first, second) { t.Fatal("render is not deterministic") }
    if len(first.Files["_index.md"]) > 4000 { t.Fatal("compact index exceeded limit") }
}

func TestValidateRejectsMissingEvidenceAndRetraction(t *testing.T) {
    report := Validate(t.Context(), atlasRef(), invalidDraft(), fixtureLedger(t))
    if report.Publishable || !hasCodes(report, "missing_evidence", "retracted_source") { t.Fatalf("report = %+v", report) }
}
```

- [ ] **Step 2: Run tests and confirm missing rendering/gates**

Run: `go test ./internal/brain ./internal/wiki -run 'Test(Render|Validate|BuildWriteProposal)'`
Expected: FAIL for the new contracts.

- [ ] **Step 3: Implement canonical pages, index, and validation**

```go
func Render(input Synthesis, compactLimit int) (SnapshotDraft, error)
func Validate(ctx context.Context, ref ProjectRef, draft SnapshotDraft, ledger RetractionView) ValidationReport
```

Sort kinds/slugs/claims/evidence links; normalize LF and one trailing newline; require safe `concepts`, `entities`, `projects`, or `sources` paths; render dual Wiki/Markdown links and claim evidence metadata; derive `_index.md` within the byte cap. Extend `writableWikiKinds` with `projects` and run `wiki.BuildWriteProposal` for every page. Validation must reject malformed frontmatter, duplicate IDs, unsafe links, cross-space URIs, unknown/retracted evidence, unreferenced claims, hash mismatch, oversize output, and deterministic prompt-injection/secret findings.

- [ ] **Step 4: Run focused tests twice to prove stability**

Run: `go test ./internal/brain ./internal/wiki -run 'Test(Render|Validate|BuildWriteProposal)' -count=2`
Expected: PASS on both runs with identical hashes.

- [ ] **Step 5: Commit**

```bash
git add internal/brain/render.go internal/brain/render_test.go internal/brain/validate.go internal/brain/validate_test.go internal/wiki/writeback.go internal/wiki/writeback_test.go
git commit -m "feat: validate brain wiki snapshots"
```

### Task 6: Bounded Gemini Synthesis and Compiler Orchestration

**Files:**
- Create: `internal/brain/compiler.go`
- Create: `internal/brain/compiler_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `SourceReader`, `Repository`, `Render`, `Validate`, and `llm.Runtime`.
- Produces: `Compiler.Build(context.Context, BuildRequest) (Manifest, error)`.

- [ ] **Step 1: Write failing fake-LLM budget and trust-boundary tests**

```go
type fakeCaller struct { calls int; output Synthesis; usage types.TokenUsage }
func (f *fakeCaller) CallJSON(_ context.Context, _ llm.Config, _, _ string, _ map[string]any, dest any) (types.TokenUsage, error) {
    f.calls++
    out, ok := dest.(*Synthesis)
    if !ok { return types.TokenUsage{}, errors.New("unexpected destination") }
    *out = f.output
    return f.usage, nil
}

func TestCompilerRejectsInputBeforeLLMCall(t *testing.T) {
    fake := &fakeCaller{}
    _, err := testCompiler(t, fake, withInputBytes(200001)).Build(t.Context(), buildRequest())
    if !errors.Is(err, ErrCompileBudget) || fake.calls != 0 { t.Fatalf("calls=%d err=%v", fake.calls, err) }
}
```

Add tests for missing Gemini credentials, output-token and estimated-cost overflow, malformed structured output, prompt-injection findings, retraction watermark change before staging, context cancellation, and successful manifests recording scene/model/prompt/config hashes without prompts or secrets.

- [ ] **Step 2: Run compiler tests and see missing implementation**

Run: `go test ./internal/brain -run 'TestCompiler'`
Expected: FAIL.

- [ ] **Step 3: Implement the structured synthesis boundary**

```go
type BuildRequest struct { Ref ProjectRef; Cutoff time.Time; ExpectedCurrent string }
type Synthesizer interface { Synthesize(context.Context, SourceSet) (Synthesis, types.TokenUsage, error) }
type Compiler struct { Sources *SourceReader; Synthesis Synthesizer; Repository *Repository; Ledger RetractionView }
func (c *Compiler) Build(ctx context.Context, req BuildRequest) (Manifest, error)
```

Build `llm.Config{Scene: "brain_compiler"}` from the immutable Brain config snapshot, resolve only the configured Gemini credential, call `llm.Runtime.CallJSONExact`, and apply `llm.ConservativeInputTokenUpperBound`, `llm.WithMaxOutputTokens`, and `llm.EstimateCostUSD`. Use a strict JSON schema with `additionalProperties: false`, bounded pages/claims/strings, evidence-ID enums, and no raw Markdown path supplied by the model. The compiler assigns safe paths, renders, validates, and creates staging only after all gates pass.

- [ ] **Step 4: Verify compiler behavior with fakes**

Run: `go test ./internal/brain -run 'TestCompiler' -count=1`
Expected: PASS and no test reads `GEMINI_API_KEY`.

- [ ] **Step 5: Commit**

```bash
git add internal/brain/compiler.go internal/brain/compiler_test.go internal/config/config.go internal/config/config_test.go
git commit -m "feat: compile bounded brain proposals"
```

### Task 7: Operator `brain-compile` CLI

**Files:**
- Create: `internal/store/open.go`
- Create: `internal/store/open_test.go`
- Create: `cmd/brain-compile/main.go`
- Create: `cmd/brain-compile/main_test.go`
- Modify: `cmd/server/build.go`
- Modify: `cmd/server/build_test.go`

**Interfaces:**
- Consumes: Tasks 1-6.
- Produces: `store.Open(kind, dsn string) (store.Store, error)`.
- Produces: CLI subcommands `build`, `inspect`, `verify`, `publish`, `rollback`, and `status`.

- [ ] **Step 1: Write failing store-factory and CLI contract tests**

```go
func TestRunPublishRequiresScopeSnapshotAndExpectedCurrent(t *testing.T) {
    code := run([]string{"publish", "--tenant", "tenant-a"}, io.Discard, io.Discard, fakeDeps())
    if code != 2 { t.Fatalf("code = %d", code) }
}

func TestRunPublishConflictReturnsGateExit(t *testing.T) {
    deps := fakeDepsWithPublishError(brain.ErrCurrentConflict)
    if code := run(validPublishArgs(), io.Discard, io.Discard, deps); code != 1 { t.Fatalf("code = %d", code) }
}
```

- [ ] **Step 2: Run tests and confirm missing CLI/factory**

Run: `go test ./internal/store ./cmd/brain-compile ./cmd/server -run 'Test(Open|Run.*Brain|RunPublish|BuildStore)'`
Expected: FAIL.

- [ ] **Step 3: Implement reusable Store opening and dependency-injected CLI**

```go
func run(args []string, stdout, stderr io.Writer, deps dependencies) int
// Exit 0: success; 1: validation/CAS/revocation gate; 2: usage/config/infrastructure error.
```

Every subcommand must require explicit `--tenant` and `--project`; snapshot commands also require their snapshot/current arguments. Load one config snapshot, resolve the project, open the configured Store only for `build`, and never print source content, prompts, credentials, or raw provider errors. `inspect` prints bounded manifest/diff metadata; `verify` reruns gates; `status` reports current/staging/release IDs and revocation state. Refactor `cmd/server.buildStore` to delegate to `store.Open` without behavior changes.

- [ ] **Step 4: Run CLI tests and build the binary**

Run: `go test ./internal/store ./cmd/brain-compile ./cmd/server -run 'Test(Open|Run.*Brain|RunPublish|BuildStore)'`
Expected: PASS.

Run: `go build ./cmd/brain-compile`
Expected: exit 0.

- [ ] **Step 5: Commit**

```bash
git add internal/store/open.go internal/store/open_test.go cmd/brain-compile/main.go cmd/brain-compile/main_test.go cmd/server/build.go cmd/server/build_test.go
git commit -m "feat: add brain compilation cli"
```

### Task 8: Snapshot Wiki Provider and Corpus-Aware Tools

**Files:**
- Create: `internal/brain/provider.go`
- Create: `internal/brain/provider_test.go`
- Modify: `internal/tools/retrieval.go`
- Modify: `internal/tools/retrieval_test.go`
- Modify: `internal/tools/wiki.go`
- Modify: `internal/tools/wiki_test.go`
- Modify: `internal/tools/wiki_resilience.go`
- Modify: `internal/tools/wiki_cache_test.go`
- Modify: `cmd/server/wiki_builder.go`
- Modify: `cmd/server/wiki_builder_test.go`
- Modify: `cmd/server/wiki_builder_integration_test.go`

**Interfaces:**
- Consumes: `Repository`, `RetractionView`, and the existing `wiki.DirectoryClient`.
- Produces: `tools.WikiScope`, `tools.CorpusWikiReader`, and a composite Wiki router.

- [ ] **Step 1: Write failing default/corpus/isolation tests**

```go
func TestWikiSearchDefaultsToExistingWiki(t *testing.T) {
    result := executeWikiSearch(t, scopedContext("", ""), map[string]any{"query":"course"})
    assertSourcesFromSpace(t, result, "ordinary-space")
}

func TestBrainCandidateCannotCrossSnapshot(t *testing.T) {
    candidate := searchBrain(t, scopedContext("atlas", "snap-1"))
    _, err := fetchBrain(t, scopedContext("atlas", "snap-2"), candidate)
    if err == nil { t.Fatal("cross-snapshot candidate accepted") }
}
```

Add tests for `corpus=wiki|brain|all`, invalid corpus, absent project/snapshot, deterministic bounded merge, same Task ID across tenants/projects, retraction watermark changes between Search and Fetch, graph/read routing by Brain space, and Brain-disabled schemas omitting `corpus`.

- [ ] **Step 2: Run focused tests and verify they fail**

Run: `go test -race ./internal/brain ./internal/tools ./cmd/server -run 'Test(WikiSearchDefaults|BrainCandidate|Corpus|BrainProvider|BuildWiki)'`
Expected: FAIL.

- [ ] **Step 3: Implement neutral corpus interfaces and provider routing**

```go
type WikiScope struct { TaskID, TenantID, BrainProjectID, BrainSnapshotID string }
type CorpusWikiReader interface {
    SearchCorpus(context.Context, string, int, string, WikiScope) ([]wiki.Document, error)
}
func WithRetrievalExecutionContext(ctx context.Context, taskID, tenantID string, options ...RetrievalContextOption) context.Context
func WithBrainScope(projectID, snapshotID string) RetrievalContextOption

type Provider struct { Repository *Repository; Ledger RetractionView }
func NewProvider(repository *Repository, ledger RetractionView) *Provider
```

`brain.Provider` opens only the pinned verified release, checks the live ledger on every operation, and delegates page/search/graph behavior to a release-local `wiki.DirectoryClient`. The server composite router preserves the ordinary client for `wiki`, uses Brain for `brain`, and merges two independently bounded result sets for `all`. Add `corpus` to the tool schema only when Brain is enabled. Include corpus/project/snapshot/retraction-watermark in cache keys and candidate hashes; Fetch must recheck the current watermark before reading.

- [ ] **Step 4: Run tool/provider race tests**

Run: `go test -race ./internal/brain ./internal/tools ./cmd/server -run 'Test(WikiSearchDefaults|BrainCandidate|Corpus|BrainProvider|BuildWiki)'`
Expected: PASS with default Wiki behavior byte-for-byte unchanged when Brain is disabled.

- [ ] **Step 5: Commit**

```bash
git add internal/brain/provider.go internal/brain/provider_test.go internal/tools/retrieval.go internal/tools/retrieval_test.go internal/tools/wiki.go internal/tools/wiki_test.go internal/tools/wiki_resilience.go internal/tools/wiki_cache_test.go cmd/server/wiki_builder.go cmd/server/wiki_builder_test.go cmd/server/wiki_builder_integration_test.go
git commit -m "feat: expose brain through wiki tools"
```

### Task 9: Runtime Snapshot Pinning, JIT Context, and Read-Only API

**Files:**
- Modify: `internal/brain/provider.go`
- Modify: `internal/brain/provider_test.go`
- Modify: `internal/orchestrator/engine.go`
- Modify: `internal/orchestrator/rag_test.go`
- Modify: `internal/orchestrator/eino.go`
- Modify: `internal/orchestrator/adk.go`
- Modify: `internal/executor/executor.go`
- Modify: `internal/multiagent/coordinator.go`
- Modify: `internal/multiagent/jit_retrieval_test.go`
- Modify: `internal/planner/jit_policy.go`
- Modify: `internal/planner/jit_policy_test.go`
- Modify: `internal/planner/prompt.go`
- Modify: `internal/api/wiki.go`
- Modify: `internal/api/wiki_test.go`
- Modify: `cmd/server/engine_builder.go`
- Modify: `cmd/server/engine_builder_test.go`

**Interfaces:**
- Consumes: durable Brain Task fields and `brain.Provider`.
- Produces: `brain.SnapshotPinner.Pin(context.Context, *types.Task) (brain.TaskContext, bool, error)` and `brain.WithTaskContext`.

- [ ] **Step 1: Write failing pin/resume/mode/API tests**

```go
func TestNextPinsBrainSnapshotOnce(t *testing.T) {
    task := newBrainTask("atlas")
    engine := engineWithBrainCurrent(t, "snap-1")
    if err := engine.Next(t.Context(), task); err != nil { t.Fatal(err) }
    publishCurrentFixture(t, engine, "snap-2")
    if err := engine.Next(t.Context(), task); err != nil { t.Fatal(err) }
    if task.BrainSnapshotID != "snap-1" { t.Fatalf("snapshot = %q", task.BrainSnapshotID) }
}
```

Add tests for immediate persistence before planning, resume after restart, missing/revoked/config-drift failure, no project meaning no Brain, compact index byte cap, `wiki_search(corpus=brain)` routing for long-term-memory goals, all three execution modes receiving identical scope, and page API allowing only the authenticated tenant's ordinary/Brain spaces.

- [ ] **Step 2: Run the focused runtime suite and observe failures**

Run: `go test -race ./internal/orchestrator ./internal/planner ./internal/multiagent ./internal/api ./cmd/server -run 'Test.*Brain|TestNextPinsBrainSnapshotOnce'`
Expected: FAIL.

- [ ] **Step 3: Implement one shared pinning and context path**

```go
type TaskContext struct { Ref ProjectRef; SnapshotID, ConfigDigest, CompactIndex string }
type SnapshotPinner interface { Pin(context.Context, *types.Task) (TaskContext, bool, error) }
func WithTaskContext(context.Context, TaskContext) context.Context
func TaskContextFrom(context.Context) (TaskContext, bool)
```

At the start of `Engine.Next`, resolve/pin `CURRENT`, persist the Task immediately when fields first change, load the bounded compact index, and bind one context used by Eino, ADK, Executor, and Multi-Agent. Existing pinned values win on resume. Reject missing/corrupt/revoked releases and digest changes with stable sanitized error codes. Adjust deterministic JIT routing so a pinned Brain task requesting long-term memory emits `wiki_search` with `corpus=brain`; leave all disabled/non-Brain decisions unchanged. Authorize page reads against a set containing the tenant Wiki space and allowlisted Brain spaces.

- [ ] **Step 4: Run runtime and API race tests**

Run: `go test -race ./internal/orchestrator ./internal/planner ./internal/multiagent ./internal/api ./cmd/server -run 'Test.*Brain|TestNextPinsBrainSnapshotOnce'`
Expected: PASS across `eino`, `step`, and `multiagent` fixtures.

- [ ] **Step 5: Commit**

```bash
git add internal/brain/provider.go internal/brain/provider_test.go internal/orchestrator/engine.go internal/orchestrator/rag_test.go internal/orchestrator/eino.go internal/orchestrator/adk.go internal/executor/executor.go internal/multiagent/coordinator.go internal/multiagent/jit_retrieval_test.go internal/planner/jit_policy.go internal/planner/jit_policy_test.go internal/planner/prompt.go internal/api/wiki.go internal/api/wiki_test.go cmd/server/engine_builder.go cmd/server/engine_builder_test.go
git commit -m "feat: pin brain snapshots per task"
```

### Task 10: Observability, Readiness, and Operator Documentation

**Files:**
- Create: `internal/brain/metrics.go`
- Create: `internal/brain/metrics_test.go`
- Modify: `internal/api/handler.go`
- Modify: `internal/api/handler_test.go`
- Modify: `cmd/server/main.go`
- Modify: `cmd/server/wiki_builder.go`
- Modify: `cmd/server/wiki_builder_test.go`
- Modify: `README.md`
- Modify: `config.yaml`
- Modify: `config_zh.yml`

**Interfaces:**
- Consumes: compiler, repository, provider, and runtime outcomes.
- Produces: `brain.CurrentMetrics() MetricsSnapshot` and Brain health/status in `/ready` and `/api/metrics`.

- [ ] **Step 1: Write failing bounded-metric and readiness tests**

```go
func TestBrainMetricsExcludeHighCardinalityLabels(t *testing.T) {
    recorder := newMetricRecorder(t)
    ObserveSearch(t.Context(), "brain", "success")
    assertAttributeKeys(t, recorder, "corpus", "outcome")
    assertAttributeKeysAbsent(t, recorder, "tenant_id", "project_id", "task_id", "snapshot_id")
}

func TestReadyReportsBrainWithoutContent(t *testing.T) {
    body := readyResponse(t, brainStatus{Enabled:true, Healthy:true, CurrentProjects:0})
    if !body.Brain.Healthy || body.Brain.CurrentProjects != 0 { t.Fatalf("brain = %+v", body.Brain) }
}
```

- [ ] **Step 2: Run tests and confirm the surface is absent**

Run: `go test ./internal/brain ./internal/api ./cmd/server -run 'TestBrainMetrics|TestReadyReportsBrain|TestMetrics.*Brain'`
Expected: FAIL.

- [ ] **Step 3: Add low-cardinality metrics and health wiring**

```go
type MetricsSnapshot struct {
    CompileTotal, PublishTotal, PublishConflicts, RetractionBlocked int64
    SearchTotal, FetchTotal, CacheHits int64
    CompileAverageDurationMS, SnapshotAgeSeconds float64
}
func CurrentMetrics() MetricsSnapshot
```

Emit `agent.brain.compile.total`, `.compile.duration_ms`, `.publish.total`, `.publish.conflicts`, `.snapshot.age_seconds`, `.retraction.blocked`, `.search.total`, `.fetch.total`, and `.cache.hits` with only `outcome`, `corpus`, and `provider` labels. Extend `/api/metrics` and `/ready` with sanitized Brain snapshots. Brain disabled reports `configured=false` and does not affect readiness. When enabled, invalid/unreadable root initialization fails startup; an allowed project with no release is reported but does not make unrelated server traffic unready.

- [ ] **Step 4: Document operation and reload boundaries**

Document CLI examples, exit codes, staging review, CAS publish, rollback, revocation behavior, alert suggestions, `GEMINI_API_KEY`, and the fact that `brain.root` changes require restart. State explicitly that logical revocation is not physical erasure and that no HTTP write route exists.

- [ ] **Step 5: Run focused tests**

Run: `go test ./internal/brain ./internal/api ./cmd/server -run 'TestBrainMetrics|TestReadyReportsBrain|TestMetrics.*Brain'`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/brain/metrics.go internal/brain/metrics_test.go internal/api/handler.go internal/api/handler_test.go cmd/server/main.go cmd/server/wiki_builder.go cmd/server/wiki_builder_test.go README.md config.yaml config_zh.yml
git commit -m "feat: observe brain snapshot health"
```

### Task 11: End-to-End Verification and P1 Release Evidence

**Files:**
- Create: `internal/brain/e2e_test.go`
- Create: `records/brain-readonly-mvp-p1.md`

**Interfaces:**
- Consumes: the complete P1 runtime and existing version-3 Brain Eval dataset.
- Produces: production-shaped end-to-end verification and a sanitized P1 evidence record; no new runtime API.

- [ ] **Step 1: Write failing production-shaped end-to-end tests**

```go
func TestCompilePublishSearchFetchRollbackEndToEnd(t *testing.T) {
    env := newE2EEnvironment(t)
    first := env.BuildVerifyPublish("snap-1")
    task := env.NewTask("atlas")
    env.AssertSearchFetch(t, task, "runtime decision", "wiki://brain-atlas/projects/decisions")
    env.PublishUpdated("snap-2")
    env.AssertPinned(t, task, first.ID)
    env.Rollback("snap-1", "snap-2")
}
```

Add a second test that appends a retraction after Search and proves Fetch fails, the release is revoked, rollback is rejected, and a clean rebuild removes the claim. Use a fake structured caller and `t.TempDir()` only.

- [ ] **Step 2: Run the end-to-end test and confirm integration defects are visible**

Run: `go test -race ./internal/brain -run 'TestCompilePublishSearchFetchRollbackEndToEnd|TestRetractionAfterSearch'`
Expected: FAIL because the production-shaped `e2eEnvironment` fixture and its boundary adapters do not exist yet.

- [ ] **Step 3: Complete the deterministic end-to-end fixture**

```go
func newE2EEnvironment(t *testing.T) *e2eEnvironment {
    t.Helper()
    root := t.TempDir()
    taskStore := store.NewMemoryStore()
    ledger := NewFileRetractionLedger(root)
    repository, err := NewRepository(root, ledger)
    if err != nil { t.Fatal(err) }
    synthesizer := &fixtureSynthesizer{output: validSynthesis()}
    compiler := &Compiler{
        Sources: NewSourceReader(taskStore),
        Synthesis: synthesizer,
        Repository: repository,
        Ledger: ledger,
    }
    return &e2eEnvironment{
        Store: taskStore,
        Ledger: ledger,
        Repository: repository,
        Provider: NewProvider(repository, ledger),
        Compiler: compiler,
    }
}
```

Define `e2eEnvironment`, `fixtureSynthesizer`, and the called assertion/mutation methods in `e2e_test.go` around the exact constructors produced by Tasks 3, 4, 6, and 8; do not create persistent fixture output. Assert exact Wiki URI, snapshot ID, manifest/file hashes, and stable errors. Keep the checked-in P0 dataset, thresholds, paired-arm logic, and safety gates unchanged.

- [ ] **Step 4: Run all deterministic verification**

Run:

```bash
gofmt -w internal/brain/*.go cmd/brain-compile/*.go internal/config/config.go internal/config/config_test.go internal/types/task.go internal/api/handler.go internal/api/handler_test.go internal/api/wiki.go internal/api/wiki_test.go internal/store/sqlite.go internal/store/postgres.go internal/store/store_test.go internal/store/migration_test.go internal/store/external_integration_test.go internal/tools/retrieval.go internal/tools/retrieval_test.go internal/tools/wiki.go internal/tools/wiki_test.go internal/tools/wiki_resilience.go internal/tools/wiki_cache_test.go internal/orchestrator/engine.go internal/orchestrator/rag_test.go internal/orchestrator/eino.go internal/orchestrator/adk.go internal/executor/executor.go internal/multiagent/coordinator.go internal/multiagent/jit_retrieval_test.go internal/planner/jit_policy.go internal/planner/jit_policy_test.go internal/planner/prompt.go internal/wiki/writeback.go internal/wiki/writeback_test.go cmd/server/build.go cmd/server/build_test.go cmd/server/main.go cmd/server/wiki_builder.go cmd/server/wiki_builder_test.go cmd/server/wiki_builder_integration_test.go
```
Expected: exit 0.

Run: `go test ./...`
Expected: PASS.

Run: `go test -race ./internal/brain ./internal/tools ./internal/orchestrator ./internal/multiagent ./internal/api ./cmd/server ./cmd/brain-compile ./cmd/brain-eval`
Expected: PASS.

Run: `go vet ./...`
Expected: PASS.

Run: `git diff --check`
Expected: no output and exit 0.

Run: `go run ./cmd/brain-eval -input evals/brain/dataset.yaml -mode offline -format json`
Expected: offline paired gate PASS with zero scope leak, entity contamination, retraction recurrence, prompt-injection recurrence, and retrieval false positive.

- [ ] **Step 5: Stop and obtain explicit Live Eval budget approval**

Request approval for the exact P0-matched run: 288 maximum Writer/Judge calls, a 60,000-token admission ceiling, and a $1.00 USD ceiling. Do not run Live Eval until the user approves all three limits. Never print or persist `GEMINI_API_KEY`.

- [ ] **Step 6: Run the approved matched Live gate**

Run after approval:

```bash
go run ./cmd/brain-eval \
  -input evals/brain/dataset.yaml \
  -mode live -format json -repetitions 3 \
  -max-total-tokens 60000 \
  -max-total-cost-usd 1.00
```

Expected: answer-accuracy delta `>= +0.10`, token ratio `<= 1.20`, P95 latency ratio `<= 1.20`, and every safety/no-answer/Judge hard gate PASS. Record actual token and cost totals; do not store prompts, answers, credentials, paths, or raw provider responses.

- [ ] **Step 7: Write and verify the P1 evidence record**

Record commit, dataset/manifest hashes, sanitized commands, exit codes, deterministic metrics, matched Live aggregates, budgets, and known non-blocking regressions in `records/brain-readonly-mvp-p1.md`.

Run: `rg -n 'GEMINI_API_KEY=|Authorization:|api[_ -]?key\s*[:=]' records/brain-readonly-mvp-p1.md`
Expected: no output.

- [ ] **Step 8: Commit final verification artifacts**

```bash
git add internal/brain/e2e_test.go records/brain-readonly-mvp-p1.md
git commit -m "test: verify brain read-only mvp"
```

---

## Spec Coverage Map

| Spec area | Implemented and verified by |
|---|---|
| Purpose, scope, non-goals | Global Constraints; Tasks 7-10 |
| Project identity and configuration | Tasks 1-2 |
| Source eligibility and provenance | Task 3 |
| Immutable snapshots, CAS, rollback, retraction | Task 4 |
| Page/index rendering and deterministic gates | Task 5 |
| Gemini synthesis and budgets | Task 6 |
| CLI-only operator workflow | Task 7 |
| Existing Wiki tools and corpus isolation | Task 8 |
| Snapshot pinning, JIT context, modes, read-only API | Task 9 |
| Metrics, readiness, reload/operations documentation | Task 10 |
| Full suite, race, vet, offline/Live gates, evidence record | Task 11 |

---

## Final Review Checklist

- [ ] Every spec section maps to at least one task above.
- [ ] Brain-disabled API, prompt, planner, and tool behavior is regression-tested.
- [ ] No task derives project identity from Workspace or Session ID.
- [ ] Every model-produced claim has a valid, same-project evidence reference.
- [ ] Publish/rollback CAS and live retraction precedence are race-tested.
- [ ] No HTTP write route, background worker, or automatic publish path exists.
- [ ] All changed Go files alone are formatted; unrelated files remain untouched.
- [ ] Live Gemini execution has separate explicit budget approval and sanitized evidence.
