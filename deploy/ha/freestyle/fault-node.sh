#!/usr/bin/env bash
set -euo pipefail
usage() { echo 'usage: fault-node.sh --action term|kill|start --task-id ID --output NEW_JSON [--apply]' >&2; exit 2; }
action= task_id= output= apply=false
while (($#)); do
  case "$1" in
    --action) (($# >= 2)) || usage; action=$2; shift 2 ;;
    --task-id) (($# >= 2)) || usage; task_id=$2; shift 2 ;;
    --output) (($# >= 2)) || usage; output=$2; shift 2 ;;
    --apply) apply=true; shift ;;
    *) usage ;;
  esac
done
case "$action" in term|kill|start) ;; *) usage ;; esac
[[ "$task_id" =~ ^[A-Za-z0-9_-]+$ && ${#task_id} -le 128 && -n "$output" ]] || usage
echo "Fault action $action on this dedicated Agent; task $task_id. Assertions remain manual."
[[ "$apply" == true ]] || { echo 'Plan only. Add --apply inside the intended A/B test VM.'; exit 0; }
[[ $(uname -s) == Linux && $(id -u) == 0 ]] || exit 2
source /etc/ai-agent-ha/bootstrap.env
[[ "$HA_ROLE" == node-a || "$HA_ROLE" == node-b ]] || exit 2
[[ $(cat /etc/ai-agent-ha/status) == bootstrap-complete-not-accepted ]] || exit 2
[[ ! -e "$output" && ! -L "$output" ]] || exit 2
umask 077
set -C
exec 3> "$output" # Reserve the evidence path before touching the service.
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
pid=$(systemctl show ai-agent.service --property=MainPID --value)
status=0
case "$action" in
  term) systemctl stop ai-agent.service || status=$? ;; # Explicit stop uses the unit's SIGTERM; no automatic restart.
  kill)
    [[ "$pid" =~ ^[0-9]+$ && "$pid" -gt 1 ]] || status=2
    if [[ "$status" == 0 ]]; then systemctl kill --signal=SIGKILL --kill-who=main ai-agent.service || status=$?; fi
    ;; # Restart=on-failure remains active; record the replacement PID separately.
  start) systemctl start ai-agent.service || status=$? ;;
esac
python3 - "$action" "$task_id" "$started" "$pid" "$status" "$HA_ROLE" >&3 <<'PY'
import datetime, json, sys
action, task, started, pid, status, node = sys.argv[1:]
json.dump(dict(action=action, task_id=task, node=node, started_utc=started,
    command_finished_utc=datetime.datetime.now(datetime.timezone.utc).isoformat(), old_main_pid=int(pid),
    command_exit_code=int(status), recovery_verified=False, full_ha_acceptance=False), sys.stdout, indent=2)
PY
exec 3>&-
exit "$status"
