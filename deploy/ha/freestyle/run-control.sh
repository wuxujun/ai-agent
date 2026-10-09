#!/usr/bin/env bash
set -euo pipefail
usage() { echo 'usage: run-control.sh --mode contracts|smoke|soak|gate --bundle DIR --run ID [--source CLEAN_SOURCE_DIR] [--manual-review-passed] [--apply]' >&2; exit 2; }
mode= bundle= run= source_dir= manual=false apply=false
while (($#)); do
  case "$1" in
    --mode) (($# >= 2)) || usage; mode=$2; shift 2 ;;
    --bundle) (($# >= 2)) || usage; bundle=$2; shift 2 ;;
    --run) (($# >= 2)) || usage; run=$2; shift 2 ;;
    --source) (($# >= 2)) || usage; source_dir=$2; shift 2 ;;
    --manual-review-passed) manual=true; shift ;;
    --apply) apply=true; shift ;;
    *) usage ;;
  esac
done
case "$mode" in contracts|smoke|soak|gate) ;; *) usage ;; esac
[[ "$run" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ && ${#run} -le 48 && -n "$bundle" ]] || usage
[[ "$mode" != contracts || -n "$source_dir" ]] || usage
[[ "$manual" != true || "$mode" == gate ]] || usage
echo "Control action: $mode; run: $run; background systemd unit: ai-agent-ha-$run"
[[ "$apply" == true ]] || { echo 'Plan only. Add --apply on Control after prerequisites pass.'; exit 0; }
[[ $(uname -s) == Linux && $(id -u) == 0 ]] || exit 2
[[ $(cat /etc/ai-agent-ha/status) == bootstrap-complete-not-accepted ]] || exit 2
source /etc/ai-agent-ha/bootstrap.env
[[ "$HA_ROLE" == control ]] || { echo 'Run only on Control' >&2; exit 2; }
bundle=$(cd "$bundle" && pwd)
[[ -f "$bundle/manifest.txt" && -x "$bundle/ha-soak" && -x "$bundle/canary-gate" ]] || exit 2
(cd "$bundle" && sha256sum --check --status SHA256SUMS)
[[ $(sed -n 's/^git_dirty=//p' "$bundle/manifest.txt") == false ]] || exit 2
case "$(uname -m):$(sed -n 's/^goarch=//p' "$bundle/manifest.txt")" in x86_64:amd64|aarch64:arm64) ;; *) exit 2 ;; esac
if [[ "$mode" == contracts ]]; then
  source_dir=$(cd "$source_dir" && pwd)
  [[ -z $(runuser -u ai-agent -- git -C "$source_dir" status --porcelain --untracked-files=all) ]] || { echo 'Contract source must be clean' >&2; exit 2; }
  [[ $(runuser -u ai-agent -- git -C "$source_dir" rev-parse HEAD) == "$(sed -n 's/^git_commit=//p' "$bundle/manifest.txt")" ]] || exit 2
  /usr/local/go/bin/go version >/dev/null
fi
runuser -u ai-agent -- test -x "$bundle/ha-soak"
runuser -u ai-agent -- test -r "$bundle/test-load.sh"
[[ $(stat -c %u /etc/ai-agent-ha/control.env) == 0 && $(stat -c %a /etc/ai-agent-ha/control.env) == 600 ]] || exit 2
output=/var/lib/ai-agent-ha/results/$run
[[ ! -e "$output" && ! -L "$output" ]] || { echo 'Run output exists; choose a new ID' >&2; exit 2; }
if systemctl cat "ai-agent-ha-$run.service" >/dev/null 2>&1; then
  echo 'Unit name already exists; choose a new run ID' >&2; exit 2
fi
umask 077
install -d -o ai-agent -g ai-agent -m 0700 /var/lib/ai-agent-ha /var/lib/ai-agent-ha/results "$output"
# systemd decouples the one-hour process from Freestyle exec's five-minute limit.
systemd-run --unit="ai-agent-ha-$run" --property=Type=exec \
  --property=User=ai-agent --property=Group=ai-agent --property=UMask=0077 \
  --property=EnvironmentFile=/etc/ai-agent-ha/control.env \
  --property=StandardOutput=null --property=StandardError=null \
  --property=RuntimeMaxSec=3h \
  --setenv="HA_NODE_A_IP=$HA_NODE_A_IP" --setenv="HA_NODE_B_IP=$HA_NODE_B_IP" \
  --setenv=PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin --setenv=GOTOOLCHAIN=local \
  /bin/bash /opt/ai-agent-ha/bin/control-worker.sh "$mode" "$bundle" "$output" "$source_dir" "$manual"
echo "Started. Poll systemctl show ai-agent-ha-$run -p ActiveState -p Result -p ExecMainStatus and $output/status.json."
