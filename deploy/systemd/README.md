# systemd deployment

`ai-agent.service` reads `/etc/ai-agent/ai-agent.env`. Keep credentials in that
root-managed file; do not copy project `.env` files into source control.
The three Multi-Agent rollout variables are centrally bound to the
`multiagent` config section and consumed through `config.Get()`.

## Software DAG canary

Prerequisites:

1. The DAG/Legacy main and RAG release matrices pass in the target environment.
2. The active instance serves the `software` team.
3. All instances that may resume a task run the same binary and can access the
   same durable store.

Copy the non-secret values from `ai-agent-canary.env.example` into
`/etc/ai-agent/ai-agent.env`, add the deployment's existing secrets and store
settings, then restart the service:

```bash
sudo systemctl restart ai-agent
curl --fail http://127.0.0.1:8088/ping
curl --fail http://127.0.0.1:8088/ready
```

Start at 5%. Compare DAG and Legacy runtime metrics over a representative task
window before increasing the percentage. Check at least:

- completion and failure rates;
- DAG fallback count and reasons;
- approval and replan behavior;
- average latency from `/api/metrics` and P95 from the OpenTelemetry histogram.

`GET /api/metrics` is an admin endpoint and requires the configured API key.
Runtime counters are cumulative since process start, so record a baseline after
restart or compare deltas between observations.

Newer binaries export `agent_multiagent_runtime_events_total` with bounded
`runtime` and `event` labels for approval-required and successful Replan task
incidence. The metric starts at the application restart that deploys the new
binary; earlier requests are intentionally not reconstructed. Automated rate
comparison complements, but does not replace, manual Trace review.
Every instrumented runtime call also records `event=observed`; promotion must
hold while the event-coverage count is below the runtime-call count in the
selected window.

To stop assigning new DAG tasks, set
`AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT=0` and restart. Existing tasks keep the
runtime stored in their `multiagent_runtime_selection` Trace. Do not set
`AI_AGENT_MULTIAGENT_RUNTIME=dag` during a percentage rollout because it is an
explicit 100% DAG override.
