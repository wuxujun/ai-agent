#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 --bundle DIRECTORY --mode smoke|soak --node-a URL --node-b URL --workspace PATH --team NAME --goal TEXT --output NEW_DIRECTORY [--allow-private-network]" >&2
  exit 2
}

bundle= mode= node_a= node_b= workspace= team= goal= output=
allow_private=false
while (($#)); do
  case "$1" in
    --bundle) (($# >= 2)) || usage; bundle=$2; shift 2 ;;
    --mode) (($# >= 2)) || usage; mode=$2; shift 2 ;;
    --node-a) (($# >= 2)) || usage; node_a=$2; shift 2 ;;
    --node-b) (($# >= 2)) || usage; node_b=$2; shift 2 ;;
    --workspace) (($# >= 2)) || usage; workspace=$2; shift 2 ;;
    --team) (($# >= 2)) || usage; team=$2; shift 2 ;;
    --goal) (($# >= 2)) || usage; goal=$2; shift 2 ;;
    --output) (($# >= 2)) || usage; output=$2; shift 2 ;;
    --allow-private-network) allow_private=true; shift ;;
    *) usage ;;
  esac
done
[[ -n "$bundle" && -n "$node_a" && -n "$node_b" && -n "$workspace" && -n "$team" && -n "$goal" && -n "$output" ]] || usage
[[ "$mode" == smoke || "$mode" == soak ]] || usage
[[ -n ${AI_AGENT_HA_API_KEY:-} ]] || { echo 'AI_AGENT_HA_API_KEY is required' >&2; exit 2; }
command -v python3 >/dev/null || { echo 'Python 3 is required to validate the report' >&2; exit 2; }
bundle=$(cd "$bundle" && pwd)
[[ -x "$bundle/ha-soak" ]] || { echo 'Missing executable ha-soak in bundle' >&2; exit 2; }
[[ ! -e "$output" ]] || { echo 'Output directory already exists' >&2; exit 2; }
umask 077
mkdir -- "$output"
output=$(cd "$output" && pwd)

args=(--node-a "$node_a" --node-b "$node_b" --workspace "$workspace" --team "$team" --goal "$goal"
  --results "$output/results.jsonl" --report "$output/report.json")
if [[ "$allow_private" == true ]]; then
  args+=(--allow-private-network)
fi
if [[ "$mode" == smoke ]]; then
  args+=(--duration 2m --interval 10s --max-tasks 20 --concurrency 2)
else
  args+=(--duration 1h --interval 30s --max-tasks 150 --concurrency 2)
fi
status=0
"$bundle/ha-soak" "${args[@]}" || status=$?
python3 - "$output/report.json" "$status" "$mode" <<'PY'
import json
import sys

path, status, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3]
try:
    with open(path, encoding="utf-8") as stream:
        report = json.load(stream)
except (OSError, ValueError) as error:
    print(f"No valid HA report: {error}", file=sys.stderr)
    sys.exit(2)
if report.get("full_ha_acceptance") is not False:
    print("Unexpected HA acceptance claim", file=sys.stderr)
    sys.exit(2)
if status == 2:
    print("Load tool reported an execution or output error", file=sys.stderr)
    sys.exit(2)
both_nodes = (isinstance(report.get("by_node"), list) and
              len(report["by_node"]) == 2 and min(report["by_node"]) > 0)
if mode == "smoke":
    passed = (status == 1 and report.get("scheduled", 0) > 0 and
              report.get("scheduled") == report.get("passed") and both_nodes and
              report.get("failed") == 0 and report.get("window_complete") is False)
    print("Smoke checks passed; one-hour window and HA acceptance remain pending" if passed else
          "Smoke checks failed; inspect results.jsonl and report.json")
else:
    passed = (status == 0 and report.get("window_complete") is True and
              report.get("load_checks_passed") is True and report.get("failed") == 0 and
              both_nodes)
    print("One-hour load checks passed; fault injection and HA acceptance remain pending" if passed else
          "Load checks failed; inspect results.jsonl and report.json")
sys.exit(0 if passed else 1)
PY
