# Local LiteLLM Gateway

The Compose stack runs LiteLLM with PostgreSQL persistence and enables its
Admin UI. The gateway binds to `127.0.0.1:4000` by default. PostgreSQL publishes
port `5432` on host loopback by default and can be explicitly exposed on a
trusted external interface.

## Start the gateway

Create the local environment file and replace all example secrets:

```bash
cp deploy/litellm/.env.example deploy/litellm/.env
openssl rand -hex 32
```

Use independently generated values for `LITELLM_MASTER_KEY`,
`LITELLM_SALT_KEY`, `LITELLM_UI_PASSWORD`, and `POSTGRES_PASSWORD`. Prefix the
master key with `sk-`. Do not rotate `LITELLM_SALT_KEY` after storing provider
credentials: existing encrypted credentials cannot be decrypted with a new
salt.

Start and inspect the stack:

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml ps
curl http://127.0.0.1:4000/health/liveliness
```

`COMPOSE_PROFILES=bundled-db` in `.env` keeps PostgreSQL in the active Compose
service set for both `up` and `down`. Supplying the profile only to `up` can
leave PostgreSQL running when a later plain `down` does not select that service.

The local stack defaults to one LiteLLM worker and disables telemetry. Compose
also applies resource ceilings: 1 CPU/2 GiB for LiteLLM, 0.25 CPU/128 MiB
for the model reconciler, and 0.5 CPU/256 MiB for PostgreSQL. Override
`LITELLM_CPU_LIMIT`, `LITELLM_MEMORY_LIMIT`, and the related values documented
in `.env.example` when production concurrency requires more capacity. Swap
cannot exceed each service's memory ceiling. A limit that is too low can cause
Docker to restart an out-of-memory container.

### OpenAI provider

Configure OpenAI directly or route OpenAI models through a compatible private
endpoint:

```dotenv
OPENAI_API_KEY=sk-replace-with-your-key
OPENAI_BASE_URL=https://api.openai.com/v1
```

For a compatible proxy:

```dotenv
OPENAI_BASE_URL=https://openai-proxy.example.com/v1
```

The pinned LiteLLM image also recognizes the legacy `OPENAI_API_BASE` variable,
but `OPENAI_BASE_URL` takes precedence and is the deployment convention used by
this Compose stack. Models should use the `openai/` provider prefix.

### Groq provider

The Compose service passes LiteLLM's native Groq environment variables into
the gateway:

```dotenv
GROQ_API_KEY=gsk-replace-with-your-key
GROQ_API_BASE=https://api.groq.com/openai/v1
```

`GROQ_API_BASE` may point to a compatible private endpoint or network proxy.
Include the Groq OpenAI-compatible `/openai/v1` path expected by the target
endpoint and omit a trailing slash. Models registered in LiteLLM should use the
`groq/` provider prefix, for example `groq/llama-3.3-70b-versatile`.

### Gemini provider

The Compose service also passes LiteLLM's native Gemini endpoint variables:

```dotenv
GEMINI_API_KEY=replace-with-your-key
GEMINI_API_BASE=https://generativelanguage.googleapis.com
```

For a compatible private endpoint or network proxy, replace
`GEMINI_API_BASE` with its base origin. Do not include `/v1beta` in the standard
configuration because LiteLLM appends the Gemini API path:

```dotenv
GEMINI_API_BASE=https://gemini-proxy.example.com
```

Models registered in LiteLLM should use the `gemini/` provider prefix, for
example `gemini/gemini-2.5-flash`.

### xAI provider

Configure xAI directly or replace its OpenAI-compatible endpoint with a private
proxy:

```dotenv
XAI_API_KEY=xai-replace-with-your-key
XAI_API_BASE=https://api.x.ai/v1
```

Models registered in LiteLLM should use the `xai/` provider prefix, for example
`xai/grok-4`.

### NVIDIA NIM provider

Configure NVIDIA NIM directly or point LiteLLM at a compatible self-hosted NIM
endpoint:

```dotenv
NVIDIA_NIM_API_KEY=nvapi-replace-with-your-key
NVIDIA_NIM_API_BASE=https://integrate.api.nvidia.com/v1
```

Models registered in LiteLLM should use the `nvidia_nim/` provider prefix. A
self-hosted endpoint may use a value such as
`http://nvidia-nim.internal:8000/v1`.

Open the Admin UI at `http://127.0.0.1:4000/ui` and sign in with
`LITELLM_UI_USERNAME` and `LITELLM_UI_PASSWORD`. Swagger remains available at
`http://127.0.0.1:4000/docs`.

The local direct-access configuration must not set a public URL prefix:

```dotenv
SERVER_ROOT_PATH=
PROXY_BASE_URL=http://127.0.0.1:4000
```

If `SERVER_ROOT_PATH=/litellm` is set instead, the direct container UI is
`http://127.0.0.1:4000/litellm/ui/`; `http://127.0.0.1:4000/ui/` will return
404 because the UI assets are mounted below the configured root path.

### Serve LiteLLM under `/litellm`

When the public gateway exposes LiteLLM below a URL prefix, configure both the
ASGI root path and the externally reachable URL:

```dotenv
SERVER_ROOT_PATH=/litellm
PROXY_BASE_URL=https://gateway.example.com/litellm
```

Keep `DOCS_URL=/docs` and `ROOT_REDIRECT_URL=/ui` in Compose. LiteLLM applies
`SERVER_ROOT_PATH` when generating public Admin UI routes, so including the
prefix in those two values would produce duplicate paths.

Use `nginx-litellm-subpath.conf` as the Nginx location template. Its
`proxy_pass` URL intentionally ends in `/`, which removes the external
`/litellm/` prefix before forwarding the request. The `^~` location modifier is
also required when the server has a generic dot-file rule such as
`location ~ /\.`; without it, `/.well-known/litellm-ui-config` can be rejected
by Nginx before it reaches LiteLLM.

After installing the location block in the applicable HTTPS `server`, validate
and reload Nginx, then recreate LiteLLM so the new environment variable is
loaded:

```bash
sudo nginx -t
sudo systemctl reload nginx
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d --force-recreate litellm
```

Verify both the direct upstream route and the public prefixed routes:

```bash
curl -i http://127.0.0.1:4000/health/liveliness
curl -i http://127.0.0.1:4000/.well-known/litellm-ui-config
curl -i https://gateway.example.com/litellm/health/liveliness
curl -i https://gateway.example.com/litellm/.well-known/litellm-ui-config
```

The public responses should come from LiteLLM rather than an Nginx HTML 404.
When upgrading the pinned LiteLLM image, verify subpath UI behavior before
deploying because older releases had `SERVER_ROOT_PATH` regressions.

### SMTP email notifications

The Compose service passes optional SMTP settings from `.env`. For Tencent
Exmail, configure the complete sender address and its client-specific password:

```dotenv
SMTP_HOST=smtp.exmail.qq.com
SMTP_PORT=465
SMTP_USERNAME=notifications@example.com
SMTP_PASSWORD=replace-with-client-specific-password
SMTP_SENDER_EMAIL=notifications@example.com
SMTP_TLS=False
SMTP_USE_SSL=True
LITELLM_EMAIL_INCLUDE_API_KEY=False
SERVER_ROOT_PATH=/litellm
PROXY_BASE_URL=https://proxy.your-company.com/litellm
```

`PROXY_BASE_URL` must be the externally reachable LiteLLM URL because
notification templates use it when generating links. Prefer HTTPS in
non-local environments and omit a trailing slash.

After all SMTP values are configured, enable the callback in `config.yaml`:

```yaml
litellm_settings:
  callbacks: ["smtp_email"]
```

Restart LiteLLM after changing either file. The SMTP callback sends invitation,
virtual-key, and budget notification emails to addresses attached to those
events; it is not used for request/response payload logging.

Models, virtual keys and provider credentials created in the UI are stored in
the `litellm_postgres_data` volume. `STORE_MODEL_IN_DB=True` allows UI-managed
models to survive restarts.

### Model bootstrap and Admin UI ownership

Baseline model aliases are defined in `bootstrap-models.json`, not in the
LiteLLM `model_list`. The `model-bootstrap` service waits for LiteLLM to become
healthy, checks `/model/info`, and creates each missing alias through
`POST /model/new`. Created models are stored in PostgreSQL and appear in the
Admin UI with `db_model=true`.

The reconciler is intentionally create-only:

- Existing aliases are never updated, so Admin UI edits remain authoritative.
- Models not present in the seed manifest are never deleted.
- A deleted baseline alias is recreated on the next reconciliation interval.
- Provider API keys remain environment references and are not copied into the
  seed manifest.

#### Use the Qwen manifest with a LiteLLM Credential

`qwen-models.json` is a Qwen-only alternative to `bootstrap-models.json`. It
references the reusable LiteLLM Credential named `dashscope-qwen` and assigns
the models to the Team whose alias is `ai-agent`:

```json
{
  "team_name": "ai-agent",
  "litellm_params": {
    "model": "dashscope/qwen3.7-plus",
    "litellm_credential_name": "dashscope-qwen"
  }
}
```

Create `dashscope-qwen` in the LiteLLM Admin UI before selecting this manifest.
The Credential should contain the DashScope API key and API base. Its name is a
literal LiteLLM database lookup key; `credential_name` and `credential_id` are
not valid replacements in a `/model/new` payload.

Create the Team before starting the reconciler, or change `team_name` in
`qwen-models.json` to an existing LiteLLM `team_alias`. `team_name` is a local
manifest convenience field: the reconciler calls `GET /team/list`, requires an
exact and unique alias match, removes `team_name`, and sends the resolved ID as
`model_info.team_id` to `POST /model/new`.

Omit `team_name`, or set it to `null`, `""`, or whitespace, to create a global
model without resolving or assigning a Team. The bootstrap-only `team_name`
field is always removed before the `/model/new` request.

The manifest may repeat a `model_name` to create a LiteLLM model group with
multiple deployments, for example one deployment per Credential. Repeated
entries must differ by upstream `model`, `litellm_credential_name`, or Team;
only an identical deployment tuple is rejected as a configuration error.

LiteLLM restricts models scoped with `model_info.team_id` to Premium/Enterprise
deployments. A Community deployment will reject Team-scoped model creation
with HTTP 403; remove `team_name` from each entry to create ordinary global
models instead.

Confirm the Credential exists:

```bash
curl -sS \
  -H "Authorization: Bearer <LITELLM_MASTER_KEY>" \
  http://127.0.0.1:4000/credentials/by_name/dashscope-qwen
```

Confirm the configured Team alias exists:

```bash
curl -sS \
  -H "Authorization: Bearer <LITELLM_MASTER_KEY>" \
  http://127.0.0.1:4000/team/list
```

Select the Qwen manifest in `deploy/litellm/.env`:

```dotenv
LITELLM_MODEL_BOOTSTRAP_MANIFEST=/bootstrap/qwen-models.json
```

Then recreate the reconciler:

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d --force-recreate model-bootstrap
```

The reconciler remains create-only. If an alias such as `agent-planner`
already exists, selecting the Qwen manifest will not replace its credential or
other settings; update/delete that deployment deliberately through the Admin
UI before expecting it to be recreated.

The manifest is reloaded on every reconciliation pass. A missing bind mount,
invalid JSON, unknown Team, or temporarily unavailable LiteLLM API is logged
with the manifest path and retried without terminating the container. If Docker
shows `Restarting`, confirm the deployed `bootstrap_models.py` contains this
retry behavior and recreate `model-bootstrap`; an older script loaded the
manifest before entering its retry loop.

The default aliases use distinct DashScope Qwen models that support structured
output:

| Alias | DashScope model | Purpose |
| --- | --- | --- |
| `agent-planner` | `qwen3.7-plus` | Complex planning and tool decisions |
| `agent-planner-fallback` | `qwen3.6-plus` | Planner fallback |
| `agent-writer` | `qwen3.5-plus` | Final synthesis and writing |
| `agent-fast` | `qwen3.6-flash` | Low-latency utility scenes |
| `agent-embedding` | `text-embedding-v4` | Memory and retrieval vectors |

Legacy aliases coexist for controlled fallback and rollback:

| Alias | Upstream model |
| --- | --- |
| `agent-planner-legacy` | `openai/gpt-4.1-mini` |
| `agent-planner-fallback-legacy` | `gemini/gemini-2.5-flash` |
| `agent-writer-legacy` | `openai/gpt-4.1-mini` |
| `agent-fast-legacy` | `gemini/gemini-2.5-flash` |

Configure the DashScope credential and regional endpoint before inference:

```dotenv
DASHSCOPE_API_KEY=sk-your-dashscope-key
DASHSCOPE_API_BASE=https://dashscope.aliyuncs.com/compatible-mode/v1
OPENAI_API_KEY=sk-your-openai-key
GEMINI_API_KEY=your-gemini-key
```

The root `config.yaml` binds core scenes to Qwen aliases and uses the matching
Legacy aliases as application-level fallback. To switch one scene
deterministically, exchange its primary and fallback model names. For example:

```yaml
llm:
  scenes:
    task_planner:
      model: "agent-planner-legacy"
      fallback_scene: "task_planner_qwen_fallback"
    task_planner_qwen_fallback:
      model: "agent-planner"
```

Reload ai-agent after changing scene bindings:

```bash
curl -X POST \
  -H "Authorization: Bearer ${AI_AGENT_API_KEY}" \
  http://127.0.0.1:8088/api/config/reload
```

Changing an upstream deployment in the LiteLLM Admin UI takes effect behind
that alias without an ai-agent reload. Keep provider-specific aliases separate
when deterministic switching and rollback are required; registering multiple
providers under one alias delegates selection to LiteLLM routing.

The default reconciliation interval is 60 seconds. Override it in
`deploy/litellm/.env`:

```dotenv
LITELLM_MODEL_BOOTSTRAP_INTERVAL_SECONDS=60
LITELLM_MODEL_BOOTSTRAP_TIMEOUT_SECONDS=10
```

Inspect bootstrap activity and confirm database ownership:

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml logs model-bootstrap

source deploy/litellm/.env
curl -sS -H "Authorization: Bearer ${LITELLM_MASTER_KEY}" \
  http://127.0.0.1:4000/model/info |
  jq '.data[] | {model_name, db_model: .model_info.db_model}'
```

To stop the services without deleting the database:

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml down
```

Do not use `down --volumes` unless the persisted LiteLLM configuration, keys,
and spend history are intentionally being discarded.

## Expose the bundled PostgreSQL service

The bundled PostgreSQL service publishes its container port through
`POSTGRES_BIND_ADDRESS` and `POSTGRES_PORT`. The defaults expose it only on
host loopback:

```dotenv
POSTGRES_BIND_ADDRESS=127.0.0.1
POSTGRES_PORT=5432
```

For access from a trusted external network, bind the server's private/VPN
address where possible. Binding all interfaces is supported but must be paired
with a restrictive firewall or cloud security group:

```dotenv
POSTGRES_BIND_ADDRESS=0.0.0.0
POSTGRES_PORT=55432
POSTGRES_PASSWORD=replace-with-a-long-random-password
```

Recreate only the PostgreSQL container to apply a changed port mapping. The
named data volume is retained:

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d --force-recreate postgres
```

Remote clients then connect to the Docker host, not the Compose service name:

```bash
psql "host=your-server.example.com port=55432 dbname=litellm user=litellm sslmode=prefer"
```

Do not expose PostgreSQL unrestricted to the public internet. Limit inbound TCP
to known source IPs, use a non-default host port, and prefer a VPN or SSH
tunnel. The bundled configuration does not provision a PostgreSQL server
certificate, so configure PostgreSQL TLS separately before sending credentials
over an untrusted network.

## Use an existing PostgreSQL server

LiteLLM can share a PostgreSQL server with other applications. Use a dedicated
database and database user for LiteLLM rather than sharing another
application's database/schema. The user must be able to create and migrate the
LiteLLM tables.

Set `LITELLM_DATABASE_URL` in `deploy/litellm/.env`. For example, if PostgreSQL
is running on the Docker host and listens on host port `55432`:

```dotenv
COMPOSE_PROFILES=
LITELLM_DATABASE_URL=postgresql://litellm:password@host.docker.internal:55432/litellm
```

Percent-encode special characters in the username or password used in the URL.
`host.docker.internal` works with Docker Desktop; the Compose configuration
also maps it through `host-gateway` for Docker Engine on Linux.

Start LiteLLM and its model reconciler while omitting the `bundled-db` profile:

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d litellm model-bootstrap
```

For PostgreSQL on another machine, replace `host.docker.internal:55432` with
that server's reachable hostname and port. Ensure PostgreSQL accepts the Docker
host/network in `listen_addresses`, `pg_hba.conf`, and any firewall rules.

## Connect ai-agent

Run the project client against the gateway:

```bash
set -a
source deploy/litellm/.env
set +a
AI_AGENT_LLM_PROVIDER=litellm \
AI_AGENT_LLM_MODEL=agent-planner \
AI_AGENT_LLM_BASE_URL=http://127.0.0.1:4000/v1/chat/completions \
AI_AGENT_LLM_API_KEY="$LITELLM_MASTER_KEY" \
go run ./cmd/llm-eval -input Sample/llm-eval.jsonl
```

Readiness defaults to `gateway`: the service checks LiteLLM liveness and model
visibility without sending a paid generation request. Configure strict
inference verification explicitly:

```bash
AI_AGENT_LLM_READINESS_MODE=inference \
AI_AGENT_LLM_READINESS_CACHE_TTL_SECONDS=300 \
go run ./cmd/server
```

Available modes are `config_only`, `gateway`, and `inference`. Only a successful
`inference` probe sets `llm_verified=true`; it may incur provider cost. The
selected mode is returned as `llm_readiness_mode` by `/ready`.

Each case has a 30-second timeout by default. For CI, use JSON Lines output;
the command exits with `0` when every case passes, `1` for evaluation failures,
and `2` for invalid arguments or input:

```bash
go run ./cmd/llm-eval \
  -input Sample/llm-eval.jsonl \
  -timeout 45s \
  -max-total-tokens 10000 \
  -input-cost-per-million-usd 2 \
  -output-cost-per-million-usd 8 \
  -max-total-cost-usd 1.50 \
  -format json
```

Use `-max-line-bytes` when an evaluation case must exceed the default 4 MiB
JSONL line limit.

Each JSONL case must set exactly one assertion: `expected_contains` for a
case-insensitive substring match, `expected_exact` for an exact match, or
`expected_regex` for a Go regular expression match. JSON answers can instead
set `expected_json_path` together with `expected_json_value`; paths support the
root `$`, dotted object fields, and non-negative array indexes such as
`$.items[0].status`. The expected value must be a valid JSON value. A positive
`-max-total-tokens` stops execution before starting another case once the
budget is reached; the default `0` disables this limit.

Use `-parallelism N` to evaluate cases with a bounded worker pool. All input is
validated before the first LLM call and results remain in JSONL input order.
Parallel execution cannot be combined with `-max-total-tokens`, because calls
already in flight cannot honor a strict aggregate Token budget.

Cost estimation uses the supplied input and output prices per million tokens;
the CLI does not embed a provider price table. Cases targeting differently
priced models can override the global rates with
`input_cost_per_million_usd` and `output_cost_per_million_usd`.
`-max-total-cost-usd` requires at least one non-zero rate and, like the strict
Token budget, cannot be combined with parallel execution.

For qualitative evaluation, set `judge_criteria` instead of a text or JSON
assertion. The judge uses the `answer_verifier` scene by default, requires a
score of at least `0.7`, and returns a score plus reason. Override these with
`judge_scene` and `judge_min_score`. When the judge model has different prices,
set `judge_input_cost_per_million_usd` and
`judge_output_cost_per_million_usd`; judge usage is included in the case Token
and cost budgets.

The repository root `.env` is loaded by the server even when it starts from
`deploy/e2e`. Explicit `AI_AGENT_LLM_PROVIDER=litellm` is therefore required
when the root `.env` selects another provider.

`deploy/e2e/config.yaml` intentionally calls a missing primary model and then
falls back to `agent-planner`. Build and run from that directory to exercise
application-level fallback:

```bash
go build -o /tmp/ai-agent-llm-eval ./cmd/llm-eval
cd deploy/e2e
AI_AGENT_LLM_API_KEY="$LITELLM_MASTER_KEY" \
  /tmp/ai-agent-llm-eval -input ../../Sample/llm-eval.jsonl
```

Google may reject Gemini API calls in unsupported locations. That is an
external provider restriction; the OpenAI-backed aliases remain usable for
local gateway and fallback testing.
