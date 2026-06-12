# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Run the server (port 8080). Loads .env from cwd or ../../
go run ./cmd/server

# Build a release binary
go build -o server ./cmd/server

# Run all tests
go test ./...

# Run a single package's tests
go test ./internal/orchestrator/...

# Run a specific test (regex match)
go test ./internal/planner -run TestValidateDecision

# Race detector (recommended when touching orchestrator / coordinator)
go test -race ./internal/orchestrator/... ./internal/multiagent/...

# Hot-reload config without restart: SIGHUP, file save (viper watcher), or:
curl -X POST http://127.0.0.1:8080/api/config/reload
```

The HTTP API surface (tasks, SSE, approvals, metrics, config reload) is
documented in `README.md` §"接口 API 指南" — refer there before adding endpoints.
`Sample/agent-api.http` contains JetBrains-style example requests.

## Architecture

### The orchestration mode selector

`orchestrator.Engine` is **the** task driver. The `Mode` field (set from
`AI_AGENT_ORCHESTRATOR` / `orchestrator.mode`) picks one of five strategies for
`Engine.Next`:

| Mode         | File                              | What it does                                                          |
| ------------ | --------------------------------- | --------------------------------------------------------------------- |
| `eino`       | `orchestrator/eino.go`            | Default. Compiles a CloudWeGo Eino `compose.Chain` once and caches it via `sync.RWMutex` double-check. Each Next invokes the runner for one Plan→Execute step. |
| `legacy`     | `orchestrator/engine.go` (`runLegacyNext`) | Direct Planner→Executor call, no Eino wrapping. Useful for debugging the loop. |
| `step`       | `orchestrator/step.go`            | Discrete step-state machine variant.                                  |
| `adk`        | `orchestrator/adk.go`             | Google ADK-for-Go runner (`google.golang.org/adk`). Compiled once via `sync.Once`. |
| `multiagent` | `orchestrator/multiagent.go` + `internal/multiagent/coordinator.go` | Plan → Research (×N) → Write pipeline that completes the **entire task in one Next call** (not one step). Requires `eng.Coordinator` to be wired. |

Adding a mode means: add the `Mode` constant, the `run<Mode>Next` method, and a
case in `Engine.Next`. The mode-agnostic concerns (RAG memory prefetch, budget
checks, callbacks, persistence after each step) live in `Engine.Next` /
`Engine.RunAll` and apply to all modes.

### Planner ↔ Tool registry: the three-way invariant

`tools.DefaultRegistry`, `planner.PlannerDecisionSchema`,
`planner.PlannerDecisionGenAISchema`, and `planner.ValidateDecision` must stay
in lock-step. The schemas are **generated** from the registry — registering a
new tool via `tools.Register(t)` automatically adds it to the action enum and
merges its `Parameters()` into the schema's parameter object. Do not hand-edit
the schemas; change the tool. If you touch any of these four, search for the
other three and verify consistency. The `code-review` skill in `skills/`
documents this invariant explicitly.

`tools.Registry.Register` wraps each tool in `toolMiddleware` (timeout, retry
via `retryPolicyProvider`, span attributes). Tools declare a `RiskLevel()`;
any `RiskLevelHigh` action triggers `Engine.SuspendForApproval`, which blocks
on an approval channel registered in `orchestrator/approval.go` and surfaces
via `POST /api/tasks/:id/approve|reject`.

### Skills as first-class capabilities

`skills/<name>/SKILL.md` files (frontmatter + markdown) are loaded at startup
by `skills.Registry`, then exposed to the planner through the `use_skill` tool
(`tools/use_skill.go`). **`RegisterUseSkill` must run before the planner first
compiles its schema** — `cmd/server/main.go` enforces this ordering and the
comment there explains why. A missing/empty skills dir is non-fatal.

### LLM providers are pluggable

`planner.LLMProvider` is an interface; `planner.RegisterProvider` adds one to
`providerRegistry`. Built-ins (`provider.go`): `openai-responses` (default,
non-streaming structured output), `openai` (chat completions, SSE streaming),
`ollama`, plus `gemini` via `gemini_pool.go`. `config.ResolveLLMProvider`
auto-detects from which API key is present (`OPENAI_API_KEY` →
`openai-responses`; `GEMINI_API_KEY`/`GOOGLE_API_KEY` → `gemini`).

`FallbackPlanner` (`planner/fallback.go`) wraps the real planner; on error it
falls back to `MockPlanner` and records the failure in metrics. Wired in
`main.go` — preserve this when changing planner construction.

### Storage backends

`store.Store` interface, four implementations selected by `store.type`:
`sqlite` (default, modernc/sqlite, file at `data/agent.db`), `postgres`,
`redis`, `memory`. All persist the full `Task` (including `Trace`, `Memories`,
`Status`) via `SaveFullTask`. Concurrency for `run-all`: the store does an
atomic status transition (`created`/`running` → `running`) to prevent two
workers from racing on the same task; conflict → HTTP 409.

### Memory / RAG retrieval

Before each task's first step, `Engine.Next` pre-fetches up to 5 candidate
memories from each of: a third-party HTTP RAG endpoint (`rag.search_url`,
optional) and the local `Store.QueryMemories` (vector search via
`memory.GetEmbedding`). `memory.DeduplicateMemories` trims to 3 before
attaching to `task.Memories`. Skip this work by ensuring `task.Memories` is
already populated — `Engine.Next` only pre-fetches when `StepCount == 0 &&
len(Memories) == 0`.

### Telemetry

`telemetry.InitOTel` exports OTLP to `127.0.0.1:4318` (traces + metrics). All
orchestrator/engine entry points open a span named `engine.next*` /
`engine.run_all` with `agent.task.*` attributes. If no OTLP collector is
running, `[OTel Error]` logs appear but the app functions normally — don't
add `OTEL_*` env-var checks to silence them; they're informational.

### Event streaming back to clients

`Engine` exposes four callbacks (`EventCallback`, `ApprovalCallback`,
`StepCallback`, `TokenCallback`). `main.go` wires them all to `api.GetBus()`
which fans events out over the SSE endpoint (`/api/tasks/:id/stream`). SSE is
**best-effort** — the handler also polls the store every 15s as a fallback so
terminal events always land. Clients needing authoritative state should poll
`GET /api/tasks/:id`.

## Conventions specific to this repo

- **Module path**: `github.com/wuxujun/ai-agent` (Go 1.25). All internal
  packages live under `internal/` and are not importable externally.
- **Logger**: use `logger.Component("<pkg>")` rather than `log.Printf`. Each
  package owns a package-level logger (named `log`, `slog`, or `<pkg>Log` —
  search the file you're editing for the local convention before adding one).
- **Config access**: always call `config.Get()` at the point of use, **never
  cache the pointer**. The config is hot-reloadable and a cached pointer will
  miss updates. The `Resolve*` helpers on `*Config` encapsulate provider
  fallback logic — prefer them over reading raw fields.
- **Env vars**: prefixed `AI_AGENT_` and dot-to-underscore mapped
  (`llm.provider` → `AI_AGENT_LLM_PROVIDER`). API keys are explicitly bound
  to their conventional names (`OPENAI_API_KEY`, `GEMINI_API_KEY`,
  `GOOGLE_API_KEY`) so no prefix is required.
- **Policy**: `policy.Policy` enforces workspace sandboxing (directory
  traversal prevention, URL allowlist). Any new tool that reads/writes paths
  or fetches URLs must route through it.
- **Tool failures are non-fatal**: `Executor.Execute` records errors into the
  trace as observations rather than aborting; only context cancellation
  propagates as an error. Preserve this so the planner can see and recover.
- **`patch_tools.py`**: one-off codegen script that backfilled `RiskLevel()`
  onto existing tools. Not part of the build — don't rerun unless adding
  another bulk Tool-interface change.
