#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: sudo $0 --bundle DIRECTORY --apply [--health-url http://127.0.0.1:8088] [--allow-dirty]" >&2
  exit 2
}

bundle=
health_url=http://127.0.0.1:8088
apply=false
allow_dirty=false
while (($#)); do
  case "$1" in
    --bundle) (($# >= 2)) || usage; bundle=$2; shift 2 ;;
    --health-url) (($# >= 2)) || usage; health_url=$2; shift 2 ;;
    --apply) apply=true; shift ;;
    --allow-dirty) allow_dirty=true; shift ;;
    *) usage ;;
  esac
done
[[ -n "$bundle" ]] || usage
[[ $(uname -s) == Linux ]] || { echo 'deploy-node.sh runs only on Linux' >&2; exit 2; }
bundle=$(cd "$bundle" && pwd)
[[ -f "$bundle/SHA256SUMS" && -f "$bundle/manifest.txt" ]] || { echo 'Incomplete bundle' >&2; exit 2; }
for tool in sha256sum systemctl curl getent install readlink runuser stat; do
  command -v "$tool" >/dev/null || { echo "Missing tool: $tool" >&2; exit 2; }
done
(cd "$bundle" && sha256sum --check --status SHA256SUMS) || { echo 'Bundle checksum mismatch' >&2; exit 2; }
manifest_value() { sed -n "s/^$1=//p" "$bundle/manifest.txt"; }
[[ $(manifest_value goos) == linux ]] || { echo 'Bundle is not for Linux' >&2; exit 2; }
commit=$(manifest_value git_commit)
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo 'Invalid bundle commit' >&2; exit 2; }
case "$(uname -m):$(manifest_value goarch)" in
  x86_64:amd64|aarch64:arm64) ;;
  *) echo 'Bundle architecture does not match this node' >&2; exit 2 ;;
esac
if [[ $(manifest_value git_dirty) != false && "$allow_dirty" != true ]]; then
  echo 'Dirty bundle requires --allow-dirty; it is for isolated tests only' >&2
  exit 2
fi
[[ "$health_url" =~ ^http://(127\.0\.0\.1|localhost):[0-9]+/?$ ]] || {
  echo 'Health URL must be a local HTTP origin with an explicit port' >&2; exit 2;
}
health_url=${health_url%/}
getent passwd ai-agent >/dev/null || { echo 'Create the ai-agent service user first' >&2; exit 2; }
getent group ai-agent >/dev/null || { echo 'Create the ai-agent service group first' >&2; exit 2; }
[[ -s /etc/ai-agent/ai-agent.env ]] || { echo 'Missing /etc/ai-agent/ai-agent.env' >&2; exit 2; }
env_owner=$(stat -Lc %u /etc/ai-agent/ai-agent.env)
env_mode=$(stat -Lc %a /etc/ai-agent/ai-agent.env)
if [[ "$env_owner" != 0 ]] || (( (8#$env_mode & 8#077) != 0 )); then
  echo 'The environment file must be root-owned and private (mode 0600)' >&2
  exit 2
fi

root=/opt/ai-agent
unit=/etc/systemd/system/ai-agent.service
[[ ! -L "$root" && ! -L "$root/releases" ]] || {
  echo 'Refusing symlinked deployment roots' >&2; exit 2;
}
[[ ! -L "$unit" ]] || { echo 'Refusing a symlinked systemd unit' >&2; exit 2; }
[[ -d "$root/workspace" && ! -L "$root/workspace" ]] || {
  echo "Prepare a shared workspace at $root/workspace before deployment" >&2; exit 2;
}
runuser -u ai-agent -- test -w "$root/workspace" || {
  echo 'The ai-agent user cannot write to the workspace' >&2; exit 2;
}
for name in server config.yaml teams.yaml skills; do
  if [[ -e "$root/$name" || -L "$root/$name" ]]; then
    [[ -L "$root/$name" && $(readlink "$root/$name") == "current/$name" ]] || {
      echo "Refusing to replace an existing non-managed path: $root/$name" >&2; exit 2;
    }
  fi
done
if [[ -e "$unit" ]] && ! cmp -s -- "$bundle/ai-agent.service" "$unit"; then
  echo "Existing $unit differs from the bundle; review it manually" >&2
  exit 2
fi
if [[ -e "$root/current" || -L "$root/current" ]] && [[ ! -L "$root/current" ]]; then
  echo 'Refusing to replace a non-symlink current path' >&2
  exit 2
fi
if [[ -L "$root/current" ]]; then
  current=$(readlink "$root/current")
  [[ "$current" =~ ^releases/[0-9a-f]{40}-(amd64|arm64)-[0-9a-f]{12}$ && -d "$root/$current" ]] || {
    echo 'Current release link is invalid' >&2; exit 2;
  }
fi

sha=$(sha256sum "$bundle/server" | cut -d ' ' -f 1)
bundle_sha=$(sha256sum "$bundle/SHA256SUMS" | cut -d ' ' -f 1)
release_id="$commit-$(manifest_value goarch)-${bundle_sha:0:12}"
release="$root/releases/$release_id"
if [[ -e "$release" ]]; then
  cmp -s -- "$bundle/SHA256SUMS" "$release/SHA256SUMS" &&
    (cd "$release" && sha256sum --check --status SHA256SUMS) || {
      echo "Existing release differs or is damaged: $release" >&2; exit 2;
    }
fi
echo "Node release: $release_id"
echo "Server SHA256: $sha"
echo "Current release: $(readlink "$root/current" 2>/dev/null || echo none)"
if [[ "$apply" != true ]]; then
  echo 'Preflight passed. Add --apply to install and restart on this test node.'
  exit 0
fi
[[ $(id -u) -eq 0 ]] || { echo 'Run --apply as root' >&2; exit 2; }

install -d -m 0755 "$root" "$root/releases"
install -d -o ai-agent -g ai-agent -m 0750 "$root/data" "$root/logs"
if [[ ! -e "$release" ]]; then
  install -d -m 0755 "$release"
  cp -a -- "$bundle/." "$release/"
  chmod 0755 "$release"
fi
(cd "$release" && sha256sum --check --status SHA256SUMS) || { echo 'Installed release checksum mismatch' >&2; exit 2; }
for name in server config.yaml teams.yaml skills; do
  if [[ ! -L "$root/$name" ]]; then
    ln -s "current/$name" "$root/$name"
  fi
done
if [[ ! -e "$unit" ]]; then
  install -m 0644 "$bundle/ai-agent.service" "$unit"
fi
systemctl daemon-reload
systemctl enable ai-agent.service >/dev/null

old=$(readlink "$root/current" 2>/dev/null || true)
next="$root/.current.$(date -u +%Y%m%dT%H%M%SZ).$$"
ln -s "releases/$release_id" "$next"
mv -Tf -- "$next" "$root/current"

wait_healthy() {
  for ((attempt=0; attempt<30; attempt++)); do
    if systemctl is-active --quiet ai-agent.service && \
       [[ $(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 2 "$health_url/ping" 2>/dev/null) == 200 ]] && \
       [[ $(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 2 "$health_url/ready" 2>/dev/null) == 200 ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

restore_previous() {
  if [[ -n "$old" ]]; then
    rollback="$root/.current.rollback.$$"
    ln -s "$old" "$rollback"
    mv -Tf -- "$rollback" "$root/current"
    if systemctl restart ai-agent.service && wait_healthy; then
      echo "Previous release is healthy: $old" >&2
    else
      echo "Previous release was selected but is not healthy; manual recovery required: $old" >&2
    fi
  fi
}
if ! systemctl restart ai-agent.service; then
  echo 'Service restart failed; inspect journalctl -u ai-agent.service' >&2
  restore_previous
  exit 1
fi

if ! wait_healthy; then
  echo 'Health check failed; inspect journalctl -u ai-agent.service' >&2
  restore_previous
  exit 1
fi
echo "Healthy release: $release_id"
