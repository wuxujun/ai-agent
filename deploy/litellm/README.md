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
