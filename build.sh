#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:-1.0.0}"
BUILD_DIR="dist"
DEFAULT_PLATFORMS=(
  "darwin/amd64"
  "darwin/arm64"
  "linux/amd64"
  "linux/arm64"
  "windows/amd64"
)
PLATFORMS=()

usage() {
  cat <<'USAGE'
用法: ./build.sh [选项] [平台 ...]

平台格式: GOOS/GOARCH，多个平台可用空格或逗号分隔。
不指定平台时，构建 darwin/amd64、darwin/arm64、linux/amd64、linux/arm64、windows/amd64。

选项:
  -p, --platform <平台>  指定构建平台，可重复传入
  -v, --version <版本>   指定版本（默认读取 VERSION 环境变量，否则为 1.0.0）
  -o, --output <目录>    指定输出目录（默认 dist，目录必须尚不存在）
  -h, --help             显示帮助

示例:
  ./build.sh linux/amd64
  ./build.sh linux/amd64 linux/arm64
  ./build.sh --platform linux/amd64,windows/amd64 --version 1.2.0
  ./build.sh -p darwin/arm64 -o dist-macos
USAGE
}

fail() {
  echo "错误: $*" >&2
  exit 1
}

add_platforms() {
  local value="$1" platform existing duplicate
  local -a entries
  [[ -n "$value" && "$value" != ,* && "$value" != *, && "$value" != *,,* ]] || fail "平台列表不能为空或包含空项"
  IFS=',' read -r -a entries <<< "$value"
  for platform in "${entries[@]}"; do
    [[ "$platform" =~ ^[a-z0-9]+/[a-z0-9]+$ ]] || fail "无效平台: ${platform}，请使用 GOOS/GOARCH 格式"
    duplicate=false
    for existing in "${PLATFORMS[@]+${PLATFORMS[@]}}"; do
      if [[ "$existing" == "$platform" ]]; then
        duplicate=true
        break
      fi
    done
    if [[ "$duplicate" == false ]]; then
      PLATFORMS+=("$platform")
    fi
  done
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -p|--platform|-v|--version|-o|--output)
      [[ $# -ge 2 && -n "$2" && "$2" != -* ]] || fail "$1 需要参数值"
      case "$1" in
        -p|--platform) add_platforms "$2" ;;
        -v|--version) VERSION="$2" ;;
        -o|--output) BUILD_DIR="$2" ;;
      esac
      shift 2
      ;;
    --platform=*) add_platforms "${1#*=}"; shift ;;
    --version=*) VERSION="${1#*=}"; shift ;;
    --output=*) BUILD_DIR="${1#*=}"; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) fail "未知选项: $1（使用 --help 查看用法）" ;;
    *) add_platforms "$1"; shift ;;
  esac
done

[[ "$VERSION" =~ ^[a-zA-Z0-9][a-zA-Z0-9._+-]*$ ]] || fail "版本只能包含字母、数字、点、下划线、加号和连字符，且须以字母或数字开头"
[[ -n "$BUILD_DIR" && "$BUILD_DIR" != -* ]] || fail "输出目录不能为空或以连字符开头"
if [[ ${#PLATFORMS[@]} -eq 0 ]]; then
  PLATFORMS=("${DEFAULT_PLATFORMS[@]}")
fi

# 资源路径相对于脚本目录；输出路径相对于调用时的工作目录。
if [[ "$BUILD_DIR" != /* ]]; then
  BUILD_DIR="${PWD}/${BUILD_DIR}"
fi
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

[[ ! -e "$BUILD_DIR" && ! -L "$BUILD_DIR" ]] || fail "构建目录 ${BUILD_DIR} 已存在，请确认内容后手动移除或改名，或通过 --output 指定新目录"
command -v go >/dev/null 2>&1 || fail "未找到 go 命令"
SUPPORTED_PLATFORMS="$(go tool dist list)"
for PLATFORM in "${PLATFORMS[@]}"; do
  case $'\n'"$SUPPORTED_PLATFORMS"$'\n' in
    *$'\n'"$PLATFORM"$'\n'*) ;;
    *) fail "当前 Go 工具链不支持平台: ${PLATFORM}（使用 go tool dist list 查看支持的平台）" ;;
  esac
  if [[ "$PLATFORM" == windows/* ]]; then
    command -v zip >/dev/null 2>&1 || fail "Windows 打包需要 zip 命令"
  else
    command -v tar >/dev/null 2>&1 || fail "打包需要 tar 命令"
  fi
done
[[ -f config.yaml && -f teams.yaml && -d skills ]] || fail "缺少 config.yaml、teams.yaml 或 skills 目录"

# mkdir 不覆盖已有目录，也避免并发构建写入同一个输出目录。
mkdir -p -- "$(dirname -- "$BUILD_DIR")"
mkdir -- "$BUILD_DIR"
for PLATFORM in "${PLATFORMS[@]}"; do
  TARGET_OS=${PLATFORM%/*}
  TARGET_ARCH=${PLATFORM#*/}
  PACKAGE_NAME="ai-agent-${VERSION}-${TARGET_OS}-${TARGET_ARCH}"
  TARGET_DIR="${BUILD_DIR}/${PACKAGE_NAME}"
  EXEC_NAME="server"
  if [[ "$TARGET_OS" == windows ]]; then
    EXEC_NAME="server.exe"
  fi

  echo "--> 正在构建: GOOS=${TARGET_OS} GOARCH=${TARGET_ARCH}..."
  mkdir -- "$TARGET_DIR"
  CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" go build \
    -ldflags="-s -w -X github.com/wuxujun/ai-agent/internal/buildinfo.Version=${VERSION}" \
    -o "${TARGET_DIR}/${EXEC_NAME}" \
    ./cmd/server

  cp config.yaml teams.yaml "$TARGET_DIR/"
  cp -r skills "$TARGET_DIR/"
  mkdir -p "${TARGET_DIR}/data" "${TARGET_DIR}/logs" "${TARGET_DIR}/workspace"

  if [[ "$TARGET_OS" == windows ]]; then
    (cd "$BUILD_DIR" && zip -r "${PACKAGE_NAME}.zip" "$PACKAGE_NAME")
  else
    (cd "$BUILD_DIR" && tar -czvf "${PACKAGE_NAME}.tar.gz" "$PACKAGE_NAME")
  fi
  echo "✅ 打包完成: ${TARGET_DIR}"
done

echo "=== 所选 ${#PLATFORMS[@]} 个平台构建完成！输出文件位于 ${BUILD_DIR}/ 目录 ==="
