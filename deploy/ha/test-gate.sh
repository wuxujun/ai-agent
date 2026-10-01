#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 --bundle DIRECTORY --prometheus-url URL --output NEW_FILE [--manual-review-passed]" >&2
  exit 2
}

bundle= prometheus_url= output= manual=false
while (($#)); do
  case "$1" in
    --bundle) (($# >= 2)) || usage; bundle=$2; shift 2 ;;
    --prometheus-url) (($# >= 2)) || usage; prometheus_url=$2; shift 2 ;;
    --output) (($# >= 2)) || usage; output=$2; shift 2 ;;
    --manual-review-passed) manual=true; shift ;;
    *) usage ;;
  esac
done
[[ -n "$bundle" && -n "$prometheus_url" && -n "$output" ]] || usage
bundle=$(cd "$bundle" && pwd)
[[ -x "$bundle/canary-gate" ]] || { echo 'Missing executable canary-gate in bundle' >&2; exit 2; }
[[ ! -e "$output" && ! -L "$output" ]] || { echo 'Output file already exists' >&2; exit 2; }
umask 077
set -C
args=(--prometheus-url "$prometheus_url" --window 1h --json)
[[ "$manual" != true ]] || args+=(--manual-review-passed)
if ! exec 3> "$output"; then
  echo 'Cannot create gate report' >&2
  exit 2
fi
status=0
"$bundle/canary-gate" "${args[@]}" >&3 || status=$?
exec 3>&-
echo "Gate report: $output"
(( status <= 2 )) || exit 2
exit "$status"
