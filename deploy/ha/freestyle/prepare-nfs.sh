#!/usr/bin/env bash
set -euo pipefail
usage() { echo 'usage: prepare-nfs.sh --assets DIRECTORY [--apply]' >&2; exit 2; }
assets= apply=false
while (($#)); do
  case "$1" in
    --assets) (($# >= 2)) || usage; assets=$2; shift 2 ;;
    --apply) apply=true; shift ;;
    *) usage ;;
  esac
done
[[ -n "$assets" ]] || usage
assets=$(cd "$assets" && pwd)
source "$assets/bootstrap.env"
[[ "$HA_ROLE" == storage && "$HA_PREFIX" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]] || exit 2
echo 'Prepare private NFSv4 storage with root_squash; use Ganesha when kernel nfsd is unavailable.'
[[ "$apply" == true ]] || { echo 'Plan only. Add --apply on the prepared dedicated storage VM.'; exit 0; }
[[ $(uname -s) == Linux && $(id -u) == 0 ]] || exit 2
root=/etc/ai-agent-ha
[[ $(cat "$root/identity") == "$HA_PREFIX:storage" ]] || exit 2
[[ ! -e "$root/nfs-backend" && ! -e "$root/status" ]] || { echo 'Existing NFS/bootstrap completion marker; inspect instead of replacing' >&2; exit 2; }
[[ $(stat -c %u /srv/ai-agent-ha/workspace) == 21088 && $(stat -c %g /srv/ai-agent-ha/workspace) == 21088 ]] || exit 2
export DEBIAN_FRONTEND=noninteractive
if grep -Eq '^[[:space:]]*nodev[[:space:]]+nfsd$' /proc/filesystems ||
    { command -v modprobe >/dev/null && modprobe nfsd >/dev/null 2>&1; }; then
  apt-get install -y -qq nfs-kernel-server
  install -d -m 0755 /etc/exports.d
  install -m 0644 "$assets/exports" /etc/exports.d/ai-agent-ha.exports
  exportfs -ra
  systemctl enable --now nfs-server
  backend=kernel
else
  apt-get install -y -qq nfs-ganesha nfs-ganesha-vfs
  if systemctl cat nfs-server.service >/dev/null 2>&1; then
    systemctl disable --now nfs-server.service
  fi
  [[ ! -e "$root/ganesha-vendor-default.conf" ]] || exit 2
  if [[ -f /etc/ganesha/ganesha.conf ]]; then
    install -m 0600 /etc/ganesha/ganesha.conf "$root/ganesha-vendor-default.conf"
  fi
  install -d -m 0755 /etc/ganesha
  install -m 0644 "$assets/ganesha.conf" /etc/ganesha/ganesha.conf
  systemctl enable nfs-ganesha
  systemctl restart nfs-ganesha
  systemctl is-active --quiet nfs-ganesha
  kit=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
  ss -ltn | python3 "$kit/verify-listener.py" "$HA_STORAGE_IP"
  backend=ganesha
fi
printf '%s\n' "$backend" > "$root/nfs-backend"
echo "NFS service started using $backend; cross-node access and root_squash still require verification."
