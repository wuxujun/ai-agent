#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 --output NEW_DIRECTORY [--arch amd64|arm64] [--allow-dirty]" >&2
  exit 2
}

output=
arch=amd64
allow_dirty=false
while (($#)); do
  case "$1" in
    --output) (($# >= 2)) || usage; output=$2; shift 2 ;;
    --arch) (($# >= 2)) || usage; arch=$2; shift 2 ;;
    --allow-dirty) allow_dirty=true; shift ;;
    *) usage ;;
  esac
done
[[ -n "$output" && ( "$arch" == amd64 || "$arch" == arm64 ) ]] || usage

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)
cd "$root"
output_parent=$(cd "$(dirname "$output")" && pwd -P)
case "$output_parent/" in
  "$root/"*) echo 'Output directory must be outside the repository' >&2; exit 2 ;;
esac
command -v go >/dev/null || { echo 'Go is required' >&2; exit 2; }
command -v git >/dev/null || { echo 'Git is required' >&2; exit 2; }
commit=$(git rev-parse --verify HEAD)
dirty=false
if [[ -n $(git status --porcelain --untracked-files=all) ]]; then
  dirty=true
  if [[ "$allow_dirty" != true ]]; then
    echo 'Worktree is dirty; commit the release or pass --allow-dirty for a test-only bundle' >&2
    exit 2
  fi
fi

[[ ! -e "$output" ]] || { echo 'Output path already exists' >&2; exit 2; }
mkdir -m 0700 -- "$output"
output=$(cd "$output" && pwd)
version=${commit:0:12}
[[ "$dirty" == false ]] || version="${version}-dirty"
for target in server ha-soak canary-gate; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build \
    -ldflags="-s -w -X github.com/wuxujun/ai-agent/internal/buildinfo.Version=$version" \
    -o "$output/$target" "./cmd/$target"
done
cp -- config.yaml teams.yaml "$output/"
cp -R -- skills "$output/"
cp -- deploy/systemd/ai-agent.service "$output/"
cp -- deploy/ha/deploy-node.sh deploy/ha/test-load.sh deploy/ha/test-gate.sh "$output/"
cat > "$output/manifest.txt" <<EOF
git_commit=$commit
git_dirty=$dirty
goos=linux
goarch=$arch
go_version=$(go version)
created_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF

if command -v sha256sum >/dev/null; then
  hash_file() { sha256sum -- "$1"; }
elif command -v shasum >/dev/null; then
  hash_file() { shasum -a 256 -- "$1"; }
else
  echo 'sha256sum or shasum is required' >&2
  exit 2
fi
(
  cd "$output"
  find . -type f ! -path ./SHA256SUMS | LC_ALL=C sort | while IFS= read -r file; do
    hash_file "$file"
  done > SHA256SUMS
)
echo "Bundle: $output"
echo "Commit: $commit (dirty=$dirty), linux/$arch"
echo "Server SHA256: $(hash_file "$output/server" | cut -d ' ' -f 1)"
