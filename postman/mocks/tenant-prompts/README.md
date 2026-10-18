# Local tenant Prompt simulation

This local-only Langfuse stand-in follows the GET/POST JSON contract already
covered by `internal/promptmanager/bootstrap_test.go`. No published owner mock
was used. It models the paths used by tenant bootstrap and prompt resolution;
unmodeled operations return 501. Records live only in the mock process.

Run the server in one terminal (Postman CLI 1.70 or later):

```sh
postman simulation run postman/simulations/tenant-prompts.sim.yaml --port 4918 --output ndjson
```

Run the real Go prompt client against it:

```sh
AI_AGENT_TEST_LANGFUSE_SIM_URL=http://127.0.0.1:4918/langfuse go test ./internal/promptmanager -run '^TestTenantPromptSimulation$' -count=1 -v
```

The first run should log a missing tenant prompt, a system prompt read, a
tenant-only POST, and a successful tenant prompt read. Later runs reuse that
copy without creating versions. Stop the simulation with Ctrl-C. These local
checks do not verify a live Langfuse deployment or credentials.
