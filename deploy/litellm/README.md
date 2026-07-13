# Local LiteLLM E2E

The Compose service exposes LiteLLM at `http://127.0.0.1:4000` and reads
provider credentials only from environment variables.

```bash
export LITELLM_MASTER_KEY="$(openssl rand -hex 24)"
docker compose -f deploy/litellm/compose.yaml up -d
docker compose -f deploy/litellm/compose.yaml ps
curl http://127.0.0.1:4000/health/liveliness
```

Run the project client against the gateway:

```bash
AI_AGENT_LLM_PROVIDER=litellm \
AI_AGENT_LLM_MODEL=agent-planner \
AI_AGENT_LLM_BASE_URL=http://127.0.0.1:4000/v1/chat/completions \
AI_AGENT_LLM_API_KEY="$LITELLM_MASTER_KEY" \
go run ./cmd/llm-eval -input Sample/llm-eval.jsonl
```

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
