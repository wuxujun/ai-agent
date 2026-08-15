# AI Agent — Go Runtime Engine

> A production-grade, multi-LLM AI Agent execution runtime built in Go. It orchestrates autonomous research, code analysis, and task execution within a secure sandboxed workspace, powered by a dual Multi-Agent workflow engine and a comprehensive answer quality pipeline.

[![Go 1.25](https://img.shields.io/badge/Go-1.25-blue)](https://go.dev/) [![License: MIT](https://img.shields.io/badge/License-MIT-green)](LICENSE)

---

## ✨ Highlights

- **Dual Multi-Agent Workflows** — `planner_researcher_writer` for open research tasks; `planner_critic_executor_verifier` for high-risk, correctness-critical execution with mandatory plan review and result verification.
- **Adaptive Routing** — Automatically routes tasks to the reviewed topology based on complexity, intent, plan size, or the presence of high-risk tools — no extra LLM call required.
- **Multi-Provider LLM** — OpenAI (Responses API & Chat), Gemini, Ollama, LiteLLM gateway, Google ADK. Per-scene model overrides, circuit breaker, retry budget, and cost tracking.
- **Answer Quality Pipeline** — Parallel audits: fact freshness, numeric consistency, uncertainty calibration, and safety guard — all configurable per tenant.
- **RAG / Memory** — JIT or prefetch retrieval via MCP or REST; vector search (in-process cosine or pgvector); long-term task memory with conflict resolution.
- **Multi-MCP Tools** — Discover tools from multiple MCP Streamable HTTP services, namespace them per server, and isolate optional-service failures.
- **Structured Workspace Tools** — Read-only SQLite/JSON queries plus approval-gated patch application and constrained Go test execution, all routed through the shared policy/middleware layer.
- **Full Observability** — OpenTelemetry traces + metrics (OTLP or stdout), structured JSON logs with daily rotation, local metrics endpoint.
- **Security** — Workspace boundary enforcement, high-risk tool approval flow, prompt-injection detection, secret sanitization, URL allowlist.
- **Hot Reload** — Config and API keys reload without restart via `POST /api/config/reload`.

---

## 🏗️ Architecture

```
┌─────────────┐   POST /api/tasks   ┌──────────────────────────────────────────────────┐
│   Client    │ ──────────────────▶ │                  API Layer (Gin)                  │
└─────────────┘                     │  tasks · run · run-all · stream · approve · SSE   │
                                    └────────────────────┬─────────────────────────────┘
                                                         │
                                    ┌────────────────────▼─────────────────────────────┐
                                    │              Orchestrator Engine                  │
                                    │  Eino Chain · Legacy · ADK · Multi-Agent modes   │
                                    └──┬──────────────────────────────────────┬─────────┘
                                       │ Plan                                 │ Execute
                               ┌───────▼──────┐                      ┌───────▼──────────┐
                               │   Planner    │                      │  Researcher /    │
                               │  LLM · Mock  │                      │  Executor Agent  │
                               │  Fallback    │                      └───────┬──────────┘
                               └───────┬──────┘                             │
                                       │ (Reviewed workflow)        ┌───────▼──────────┐
                               ┌───────▼──────┐                    │  Security Policy  │
                               │    Critic    │                    │  Tool Registry    │
                               │  Plan Review │                    │  Approval Gate    │
                               └───────┬──────┘                    └───────┬──────────┘
                                       │                           ┌───────▼──────────┐
                               ┌───────▼──────┐                   │  Answer Pipeline  │
                               │   Verifier   │                   │  Fact · Numeric   │
                               │ Dual-call    │                   │  Safety · Uncert. │
                               └──────────────┘                   └──────────────────┘
                                                         │
                               ┌─────────────────────────▼────────────────────────────┐
                               │          Store (SQLite / Postgres / Redis / Memory)   │
                               │          OTel Telemetry · Local Metrics · JSON Logs   │
                               └──────────────────────────────────────────────────────┘
```

### Core Packages

| Package | Role |
|:---|:---|
| `internal/api` | Gin-based REST routes: task CRUD, SSE streaming, approval, memory, metrics, config reload |
| `internal/orchestrator` | Task run loop (Eino Chain / legacy / ADK / multi-agent); budget, planner, executor, answer pipeline |
| `internal/multiagent` | Coordinator + dual-workflow engine: Planner, Critic, Researcher/Executor, Writer/Verifier |
| `internal/planner` | LLM / Mock / Fallback planners; JIT retrieval router; tool-argument repairer |
| `internal/executor` | Single-agent action dispatcher |
| `internal/tools` | Tool registry: `find_files`, `search_text`, `read_file`, `write_file`, `execute_code`, `git_diff`, `http_fetch`, `web_search`, `rag_search`, `analyze_image`, … |
| `internal/policy` | Workspace boundary, URL allowlist, risk levels, approval gate |
| `internal/answerpipeline` | Parallel answer audits: citation, fact freshness, numeric consistency, uncertainty, safety |
| `internal/plancritic` | Structured plan review: completeness, ordering, risk, feasibility |
| `internal/llm` | Provider abstraction (OpenAI · Gemini · Ollama · LiteLLM · ADK), circuit breaker, retry, cost tracking |
| `internal/memory` | Long-term memory store with embedding-based retrieval and conflict resolution |
| `internal/store` | SQLite / Postgres / Redis / in-memory backend; pgvector support |
| `internal/telemetry` | OTel SDK init, OTLP or stdout export |
| `internal/promptmanager` | Langfuse prompt fetching with 3-tier fallback (Langfuse → system_prompt → code default) |
| `internal/evidencefilter` | LLM-based evidence relevance filtering |
| `internal/evidenceconflict` | Evidence conflict detection and resolution |
| `internal/sourcecredibility` | Source credibility scoring |
| `internal/promptguard` | Prompt injection detection |
| `internal/vision` | Image analysis via multimodal LLM |
| `internal/skills` | File-based skill discovery (`skills/<name>/SKILL.md`) |

---

## 📂 Project Layout

```text
├── cmd/server/main.go          # Entry point — DB init, OTel, HTTP server
├── internal/
│   ├── api/                    # REST handlers, SSE, middleware
│   ├── orchestrator/           # Task run loop (Eino / legacy / ADK / multiagent)
│   ├── multiagent/             # Dual-workflow coordinator (Research + Reviewed)
│   ├── planner/                # LLM / Mock / Fallback planners
│   ├── executor/               # Single-agent action dispatcher
│   ├── tools/                  # Tool registry and implementations
│   ├── policy/                 # Security policy, approval gate
│   ├── answerpipeline/         # Answer quality audits
│   ├── plancritic/             # Plan review (Critic)
│   ├── llm/                    # LLM provider abstraction
│   ├── llmprovider/            # Provider-specific clients
│   ├── memory/                 # Long-term memory with vector search
│   ├── store/                  # Persistence (SQLite / Postgres / Redis)
│   ├── telemetry/              # OpenTelemetry initialisation
│   ├── promptmanager/          # Langfuse prompt management
│   ├── evidencefilter/         # Evidence relevance filter
│   ├── evidenceconflict/       # Evidence conflict resolver
│   ├── sourcecredibility/      # Source credibility scorer
│   ├── promptguard/            # Prompt injection detector
│   ├── vision/                 # Multimodal image analysis
│   ├── skills/                 # Skill discovery and loading
│   ├── config/                 # Hot-reloadable config
│   ├── logger/                 # Structured JSON logger
│   ├── metrics/                # Local metrics collector
│   ├── types/                  # Shared types (Task, StepTrace, Evidence, …)
│   └── workspace/              # Workspace management
├── config.yaml                 # Main configuration (hot-reloadable)
├── teams.yaml                  # Multi-agent team and workflow configuration
├── teams_zh.yml                # teams.yaml — Chinese-annotated version
├── skills/                     # Built-in skills (code-review, …)
├── Sample/agent-api.http       # JetBrains HTTP request examples
├── records/                    # Design notes and changelogs
└── go.mod
```

---

## ⚡ Quick Start

### Prerequisites

- Go 1.25+
- Optional: `ripgrep` (`rg`) and `find` for local file tools
- Optional: OTel Collector / Jaeger on `127.0.0.1:4318` for traces

### Set API Keys

```bash
# At least one of:
export OPENAI_API_KEY="sk-..."
export GEMINI_API_KEY="..."
export GOOGLE_API_KEY="..."
```

### Run

```bash
go run ./cmd/server
# Server listens on http://127.0.0.1:8088 (configurable via AI_AGENT_API_ADDR)
```

### Build

```bash
go build -o server ./cmd/server
./server
```

### Test

```bash
go test ./...
go test -race ./internal/multiagent/... ./internal/orchestrator/...
```

---

## ⚙️ Configuration

All settings live in [`config.yaml`](config.yaml) and can be overridden by `AI_AGENT_*` environment variables without restart.

| Section | Key settings |
|:---|:---|
| `api` | `addr`, authentication, tenant workspace roots, budgets and pipeline enforcement |
| `orchestrator` | `mode` (`eino` / `legacy` / `adk` / `step` / `multiagent`), `max_concurrent_tasks` |
| `llm` | `provider`, `model`, per-scene overrides, circuit breaker, retry budget, cost caps |
| `store` | `type`, `dsn`, `vector_search`, `pgvector_dimensions`, ParadeDB ranking/slow-query settings, memory candidate/decay settings |
| `rag` | `search_url`, `search_method` (`MCP` / `POST`), `context_mode` (`jit` / `prefetch`) |
| `mcp` | `servers[]` with URL, credential environment variable, tool prefix, risk level, and failure policy (restart required) |
| `wiki` | read-only LLM Wiki MCP endpoint, tenant space, search/fetch limits, and startup failure policy (restart required) |
| `embedding` | `model` (used for memory vector search) |
| `answer_pipeline` | `enabled`, `enforcement`, required audit stages |
| `langfuse` | credentials, runtime fetching, and optional startup prompt bootstrap |
| `telemetry` | `enabled`, `endpoint`, `exporter` (`otlp` / `stdout`) |
| `log` | `level`, `console`, `file_enabled`, `access_enabled`, `directory`, `retention_days` |
| `approval` | `ttl_seconds` (default 86400), `retention_days` (default 30); `0` disables the corresponding maintenance action |
| `search` | `url`, `api_key` (Firecrawl or compatible) |
| `tool` | `timeout_seconds` |
| `skill` | `root` (skill discovery directory) |

Durable approval recovery is enabled when a 32-byte AES key is supplied as
base64. Keep the same key on every instance and in the secret manager; losing
it makes pending approval payloads unrecoverable.

```bash
export AI_AGENT_APPROVAL_ENCRYPTION_KEY="$(openssl rand -base64 32)"
```

To rotate keys, move the old primary into the comma-separated previous-key
list and install a new primary. New payloads use only the new key; v1 and v2
payloads written by previous keys remain readable. Keep previous keys until
all related approvals have been consumed/expired and passed retention cleanup.

```bash
export AI_AGENT_APPROVAL_ENCRYPTION_KEY="<new-base64-key>"
export AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS="<old-base64-key>[,<older-base64-key>]"
```

Without this variable, approvals remain process-local and sensitive action
parameters are never written to durable storage.

For production multi-tenant deployments, set
`api.auth.require_tenant_workspace_root: true` and configure a distinct
`api.tenants.<tenant>.workspace_root` for every non-admin tenant. Task creation
is then rejected with `403` if the requested workspace is outside that root.
The compatibility default is `false`; a configured `workspace_root` is always
enforced even when strict mode is disabled.

Every structured log record includes `app_version`. Local `go run` and builds
without version injection use `dev`. Release builds should embed the version:

```bash
VERSION=v1.0.0
go build -ldflags="-X github.com/wuxujun/ai-agent/internal/buildinfo.Version=${VERSION}" -o server ./cmd/server
```

### Multi-Agent Orchestration Mode

```bash
export AI_AGENT_ORCHESTRATOR_MODE=multiagent
```

Configure teams and workflows in [`teams.yaml`](teams.yaml):

```yaml
active_team: "software_reviewed"       # or "software" / "data"

teams:
  software_reviewed:
    workflow: "planner_critic_executor_verifier"   # Reviewed (high-rigor)
  software:
    workflow: "adaptive"                           # Auto-route by risk/complexity
  data:
    workflow: "planner_researcher_writer"          # Research (fast)
```

Override at runtime:
```bash
AI_AGENT_MULTIAGENT_TEAM=software_reviewed
AI_AGENT_MULTIAGENT_WORKFLOW=adaptive      # planner_researcher_writer | planner_critic_executor_verifier | adaptive
AI_AGENT_MULTIAGENT_RUNTIME=dag            # legacy | dag
```

### Langfuse Prompt Bootstrap

Each Langfuse-managed role in `teams.yaml` declares `prompt_name`, a label or
fixed version, and keeps `system_prompt` as its local fallback and first-version
seed:

```yaml
planner:
  prompt_name: "teams/software/planner"
  prompt_label: "production"
  system_prompt: |
    You are a software architect agent...
```

Enable startup synchronization explicitly:

```bash
LANGFUSE_ENABLED=true
LANGFUSE_BASE_URL=https://cloud.langfuse.com
LANGFUSE_PUBLIC_KEY=pk-lf-...
LANGFUSE_SECRET_KEY=sk-lf-...
LANGFUSE_BOOTSTRAP_MISSING_PROMPTS=true
LANGFUSE_BOOTSTRAP_FAILURE_POLICY=fail
LANGFUSE_BOOTSTRAP_TIMEOUT_SECONDS=15
```

At startup the server fetches every explicitly named team prompt. It creates a
text prompt from `system_prompt` only after both the requested label and
`latest` confirm that the prompt name is absent. Authentication errors,
timeouts, server errors, missing labels on existing prompts, and missing fixed
versions never trigger creation.

Keep bootstrap disabled on ordinary replicas in a multi-instance deployment;
run it on one designated instance or a deployment job to avoid concurrent
first-version creation. Runtime fetching and local fallback remain available
when bootstrap is disabled.

---

## 🔗 API Reference

### Sessions

Sessions group multiple tasks and their shared memories under the current tenant.

| Method | Path | Description |
|:---|:---|:---|
| `POST` | `/api/sessions` | Create a session |
| `GET` | `/api/sessions` | List sessions (`?status=active\|archived&limit=50&offset=0`) |
| `GET` | `/api/sessions/:id` | Get session detail |
| `PATCH` | `/api/sessions/:id` | Update the title or status |
| `POST` | `/api/sessions/:id/archive` | Archive a session |
| `GET` | `/api/sessions/:id/tasks` | List session tasks (`?status=&limit=50&offset=0`) |
| `GET` | `/api/sessions/:id/memories` | List session memories (`?limit=50&offset=0`) |

**Create a session:**

```http
POST /api/sessions
Content-Type: application/json

{
  "id": "session-demo-001",
  "title": "Agent file research"
}
```

The `id` is optional and is generated automatically when omitted. The `title`
defaults to `New session` and may contain at most 200 characters. New sessions
start with status `active`.

**Update a session:**

```http
PATCH /api/sessions/session-demo-001
Content-Type: application/json

{
  "title": "Updated research session",
  "status": "active"
}
```

To attach a task to the session, set `session_id` when creating the task:

```json
{
  "id": "task-001",
  "session_id": "session-demo-001",
  "mode": "multiagent",
  "goal": "Find all TODO comments in the codebase",
  "workspace": "./workspace",
  "max_steps": 8,
  "tool_budget": 10
}
```

`mode` is optional. Supported per-task values are `eino`, `legacy`, `adk`,
`step`, and `multiagent`; when omitted, the server-wide orchestrator mode is
used. Prefer `multiagent` for research/RAG workflows and `eino` or `legacy` for
simple generation and tool tasks.

Archived sessions remain queryable, but do not accept new tasks. Use
`PATCH /api/sessions/:id` with `{"status":"active"}` to reactivate one.

### Tasks

| Method | Path | Description |
|:---|:---|:---|
| `POST` | `/api/tasks` | Create a task |
| `POST` | `/api/tasks/:id/run` | Execute one Plan-Execute cycle (`?stream=true` for SSE) |
| `POST` | `/api/tasks/:id/run-all` | Run to completion async (`?stream=true` for sync SSE) |
| `GET` | `/api/tasks/:id` | Get task detail |
| `GET` | `/api/tasks` | List tasks (`?status=&limit=50&offset=0`) |
| `GET` | `/api/tasks/:id/stream` | SSE event stream |
| `POST` | `/api/tasks/:id/approve` | Approve high-risk action |
| `POST` | `/api/tasks/:id/reject` | Reject high-risk action |
| `DELETE` | `/api/tasks/:id/cancel` | Cancel running task |
| `DELETE` | `/api/tasks/:id` | Delete task (must cancel first if running) |
| `DELETE` | `/api/tasks?confirm=true` | Admin: delete all tasks |

`orchestrator.run_all_timeout_seconds` limits active execution time. Time spent
awaiting a human approval is excluded; explicit cancellation and graceful
shutdown still interrupt a paused task immediately.

SSE `token` events contain only incremental `final_answer` text. Structured
planner thoughts, action names, and tool parameters are intentionally filtered
and remain available only through the normal audited execution trace where applicable.
Multi-Agent buffers answer chunks until the draft is accepted (and, in reviewed
workflows, independently verified); rejected or low-confidence drafts are never streamed.

**Create task payload:**

```json
{
  "id": "task-001",
  "session_id": "session-demo-001",
  "mode": "multiagent",
  "goal": "Find all TODO comments in the codebase",
  "workspace": "./workspace",
  "max_steps": 8,
  "tool_budget": 10
}
```

### Memory

| Method | Path | Description |
|:---|:---|:---|
| `GET` | `/api/memories` | List memories (`?limit=50&offset=0&tenant_id=`) |
| `DELETE` | `/api/memories/:id` | Delete one memory |
| `DELETE` | `/api/memories?confirm=true` | Admin: clear all memories |

### System

| Method | Path | Description |
|:---|:---|:---|
| `GET` | `/api/metrics` | Local performance metrics, including durable approval and DAG/Legacy rollout counters |
| `POST` | `/api/config/reload` | Hot-reload config (returns redacted diff and monotonic `config_revision`) |
| `POST` | `/api/prompt/init` | Admin: idempotently initialize missing `teams.yaml` prompts in Langfuse |
| `GET` | `/ping` | Health check → `{"message":"pong"}` |

---

## 🔒 Security Notes

- **Workspace boundary**: all file operations are confined to the declared workspace path.
- **High-risk approval**: `write_file`, `execute_code`, and other high-risk tools suspend the task and require explicit `POST /api/tasks/:id/approve`.
- **Prompt injection**: incoming text processed by `internal/promptguard` before entering LLM context.
- **Secret sanitization**: `internal/sanitize` scrubs secrets from observations before storage.
- **URL allowlist**: `http_fetch` is blocked for private and loopback addresses.
- **API key security**: never commit API keys; use environment variables or a secret manager.

---

## 🗺️ Roadmap

- [ ] DAG-based graph runtime (full rollout, currently behind `AI_AGENT_MULTIAGENT_RUNTIME=dag`)
- [ ] Web UI for task management and trace visualisation
- [ ] Additional built-in skills (test generation, code review, data analysis)
- [ ] Plugin system for custom tool registration

### DAG/Legacy release evaluation

Run one service with the Legacy runtime and another with DAG, then execute the
same budgeted dataset against both:

```bash
AI_AGENT_API_KEY='<key>' go run ./cmd/multiagent-eval \
  -legacy-url http://127.0.0.1:8088 \
  -dag-url http://127.0.0.1:8089 \
  -repetitions 3 \
  -input evals/multiagent_runtime.yaml
```

The command reports per-case status, latency, budgets, verifier support and
fallbacks, then applies the dataset success-rate and P95 latency gates. It
creates uniquely named evaluation tasks and does not delete them automatically.
Repeated runs also report how many cases passed every repetition, making LLM
variance visible instead of treating one successful run as stable.
Use `-case <exact-name>` to rerun one failed or newly added case.

For the deterministic external-RAG gate, start the loopback-only fixture and
configure both services with `AI_AGENT_RAG_SEARCH_URL=http://127.0.0.1:18080/search`,
`AI_AGENT_RAG_SEARCH_METHOD=POST`, and `AI_AGENT_RAG_CONTEXT_MODE=jit`:

```bash
go run ./cmd/rag-eval-stub
go run ./cmd/multiagent-eval \
  -legacy-url http://127.0.0.1:8088 -dag-url http://127.0.0.1:8089 \
  -input evals/multiagent_rag.yaml
```

### Software-team DAG canary

After the release matrices pass, enable a stable task-level canary without
switching the whole team to DAG:

```bash
AI_AGENT_MULTIAGENT_TEAM=software
AI_AGENT_MULTIAGENT_RUNTIME=legacy
AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT=5
```

These environment variables are bound into the `multiagent` section of
`config.yaml` by the central Viper loader. Multi-agent code reads the current
`config.Get()` snapshot and does not access these environment variables
directly. The equivalent file configuration is:

```yaml
multiagent:
  team: software
  runtime: legacy
  dag_canary_percent: 5
```

The percentage is an integer from 0 to 100. Tasks are deterministically
bucketed by team and task ID; the selected runtime is recorded in the task
Trace and reused on resume. Keep `runtime=legacy` during a percentage rollout:
an explicit `runtime=dag` remains a 100% DAG override. Invalid percentages fail
closed to 0. Environment changes require a process restart.

Use `GET /api/metrics` and the exported OpenTelemetry runtime histogram to
compare DAG/Legacy completion, failure, fallback, average latency, and P95.
Rollback by setting the percentage to 0 and restarting. Already-started tasks
retain their persisted runtime; new tasks use Legacy. A systemd environment
example and operating checklist are in `deploy/systemd/README.md`.
