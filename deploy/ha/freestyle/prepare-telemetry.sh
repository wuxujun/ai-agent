#!/usr/bin/env bash
set -euo pipefail
usage() { echo 'usage: prepare-telemetry.sh --assets DIRECTORY [--apply]' >&2; exit 2; }
assets= apply=false
while (($#)); do
  case "$1" in
    --assets) (($# >= 2)) || usage; assets=$2; shift 2 ;;
    --apply) apply=true; shift ;;
    *) usage ;;
  esac
done
[[ -n "$assets" && -f "$assets/bootstrap.env" ]] || usage
source "$assets/bootstrap.env"
[[ "$HA_ROLE" == node-a || "$HA_ROLE" == node-b ]] || { echo 'Telemetry runs only on A/B' >&2; exit 2; }
echo 'Prepare node-local OTLP collector; Prometheus scrapes its private port 9464.'
[[ "$apply" == true ]] || { echo 'Plan only. Add --apply in the intended A/B VM.'; exit 0; }
[[ $(uname -s) == Linux && $(id -u) == 0 ]] || exit 2
[[ $(cat /etc/ai-agent-ha/identity) == "$HA_PREFIX:$HA_ROLE" ]] || exit 2
[[ ! -e /etc/ai-agent-ha/otel-collector.json && ! -e /etc/ai-agent-ha/collector-compose.json ]] || {
  echo 'Collector files already exist; inspect existing state instead of overwriting' >&2; exit 2;
}
install -m 0644 "$assets/otel-collector.json" /etc/ai-agent-ha/otel-collector.json
install -m 0600 "$assets/collector-compose.json" /etc/ai-agent-ha/collector-compose.json
compose=(docker compose -f /etc/ai-agent-ha/collector-compose.json)
"${compose[@]}" config --quiet
"${compose[@]}" pull
"${compose[@]}" run --rm -T --no-deps otel-collector validate --config=/etc/otelcol/config.json </dev/null
"${compose[@]}" up -d
echo 'Collector started; application export and both Prometheus targets still need verification.'
