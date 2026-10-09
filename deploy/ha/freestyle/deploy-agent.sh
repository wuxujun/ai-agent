#!/usr/bin/env bash
set -euo pipefail
usage() { echo 'usage: deploy-agent.sh --bundle DIRECTORY [--apply]' >&2; exit 2; }
bundle= apply=false
while (($#)); do
  case "$1" in
    --bundle) (($# >= 2)) || usage; bundle=$2; shift 2 ;;
    --apply) apply=true; shift ;;
    *) usage ;;
  esac
done
[[ -n "$bundle" ]] || usage
echo 'Deploy the clean bundle with the prepared test configuration; initial canary must remain 0%.'
[[ "$apply" == true ]] || { echo 'Plan only. Add --apply inside the intended A/B test VM.'; exit 0; }
[[ $(uname -s) == Linux && $(id -u) == 0 ]] || exit 2
source /etc/ai-agent-ha/bootstrap.env
[[ "$HA_ROLE" == node-a || "$HA_ROLE" == node-b ]] || exit 2
[[ $(cat /etc/ai-agent-ha/status) == bootstrap-complete-not-accepted ]] || exit 2
grep -Fxq 'AI_AGENT_MULTIAGENT_RUNTIME="legacy"' /etc/ai-agent/ai-agent.env
grep -Fxq 'AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT="0"' /etc/ai-agent/ai-agent.env
findmnt -n -t nfs,nfs4 --target /opt/ai-agent/workspace >/dev/null
runuser -u ai-agent -- test -r /opt/ai-agent/workspace/ha-fixture/README.md
runuser -u ai-agent -- test -r /etc/ai-agent/ha-config.json
runuser -u ai-agent -- test -r /opt/ai-agent-ha/runtime/teams.yaml
bundle=$(cd "$bundle" && pwd)
bash "$bundle/deploy-node.sh" --bundle "$bundle"
bash "$bundle/deploy-node.sh" --bundle "$bundle" --apply
echo 'Local health passed. Cross-node smoke, monitoring and fault assertions remain pending.'
