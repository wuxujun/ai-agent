# Harness reload HTTP simulation

This local member models only the `POST /api/config/reload` dependency used by
`harness-refine run`, from `internal/api/config_handler.go` and
`Sample/agent-api.http`. It supplies a 200 response, a 500 response, and a response
delayed by two seconds. Other calls return 501. Keys are synthetic; the handler
logs only method and path. No owner mock was searched because this is the same
repository's runtime API and the local failure fixture is intentionally narrow.

```sh
postman simulation run postman/simulations/harness-reload.sim.yaml --port 4917 --output ndjson
```

From another terminal, run the actual Go client:

```sh
AI_AGENT_TEST_HARNESS_RELOAD_URL=http://127.0.0.1:4917/runtime \
  go test -v ./cmd/harness-refine -run '^TestHarnessReloadHTTPSimulation$' -count=1
```

The timeout assertion uses a 100 ms client deadline. The simulation may finish
its delayed upstream request after the client has returned; its eventual 200
is not evidence that the client's timeout failed. Check both the Go assertions
and the request log. Stop this dedicated simulation when finished. These checks
use local stand-ins and do not establish live runtime/identity/model readiness.
