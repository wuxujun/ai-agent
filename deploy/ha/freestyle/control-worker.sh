#!/usr/bin/env bash
set -euo pipefail
[[ $# == 5 ]] || exit 2
mode=$1 bundle=$2 output=$3 source_dir=$4 manual=$5
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
finish() {
  status=$?
  trap - EXIT
  python3 - "$output/status.json" "$started" "$status" "$mode" <<'PY'
import datetime, json, sys
path, started, status, mode = sys.argv[1:]
with open(path, 'x', encoding='utf-8') as f:
    json.dump(dict(started_utc=started, finished_utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),
        exit_code=int(status), mode=mode, full_ha_acceptance=False), f, indent=2)
PY
  exit "$status"
}
trap finish EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
case "$mode" in
  contracts)
    export GOCACHE=/var/lib/ai-agent-ha/go-cache GOMODCACHE=/var/lib/ai-agent-ha/go-modcache
    cd "$source_dir"
    status=0
    go test -race -json -p=1 ./internal/store ./internal/api \
      -run '^Test(ExternalStoresTaskCreation|ExternalStoresTaskLeaseGuard|ExternalStoresPersistPausedTaskAcrossClients|ExternalPostgresDurableApprovalCASAcrossClients|ExternalPostgresDurableApprovalRecoveryContract|PostgresPoolExternal|PostgresPoolHAExternal)$' \
      -count=1 -timeout=5m > "$output/contracts.jsonl" 2> "$output/compiler.log" || status=$?
    python3 /opt/ai-agent-ha/bin/verify-contracts.py "$output/contracts.jsonl" "$status" > "$output/contracts-summary.json"
    ;;
  smoke|soak)
    bash "$bundle/test-load.sh" --bundle "$bundle" --mode "$mode" \
      --node-a "http://$HA_NODE_A_IP:8088" --node-b "http://$HA_NODE_B_IP:8088" --allow-private-network \
      --workspace /opt/ai-agent/workspace/ha-fixture --team software \
      --goal 'Read README.md and summarize it; do not modify files or execute commands.' \
      --output "$output/load" > "$output/runner.log" 2>&1
    ;;
  gate)
    args=(--bundle "$bundle" --prometheus-url http://127.0.0.1:9090 --output "$output/gate.json")
    [[ "$manual" != true ]] || args+=(--manual-review-passed)
    bash "$bundle/test-gate.sh" "${args[@]}" > "$output/runner.log" 2>&1
    ;;
  *) exit 2 ;;
esac
