# Local LiteLLM Gateway

The Compose stack runs LiteLLM with PostgreSQL persistence and enables its
Admin UI. The gateway binds to `127.0.0.1:4000` by default; PostgreSQL is
available only inside the Compose network.

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
  -f deploy/litellm/compose.yaml --profile bundled-db up -d
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml --profile bundled-db ps
curl http://127.0.0.1:4000/health/liveliness
```

Open the Admin UI at `http://127.0.0.1:4000/ui` and sign in with
`LITELLM_UI_USERNAME` and `LITELLM_UI_PASSWORD`. Swagger remains available at
`http://127.0.0.1:4000/docs`.

Models declared in `config.yaml` remain available. Models, virtual keys and
provider credentials created in the UI are stored in the
`litellm_postgres_data` volume. `STORE_MODEL_IN_DB=True` allows UI-managed
models to survive restarts.

To stop the services without deleting the database:

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml --profile bundled-db down
```

Do not use `down --volumes` unless the persisted LiteLLM configuration, keys,
and spend history are intentionally being discarded.

## Use an existing PostgreSQL server

LiteLLM can share a PostgreSQL server with other applications. Use a dedicated
database and database user for LiteLLM rather than sharing another
application's database/schema. The user must be able to create and migrate the
LiteLLM tables.

Set `LITELLM_DATABASE_URL` in `deploy/litellm/.env`. For example, if PostgreSQL
is running on the Docker host and listens on host port `55432`:

```dotenv
LITELLM_DATABASE_URL=postgresql://litellm:password@host.docker.internal:55432/litellm
```

Percent-encode special characters in the username or password used in the URL.
`host.docker.internal` works with Docker Desktop; the Compose configuration
also maps it through `host-gateway` for Docker Engine on Linux.

Start only LiteLLM—omit the `bundled-db` profile:

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d litellm
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
