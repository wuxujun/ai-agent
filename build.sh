#!/usr/bin/env bash
set -e

VERSION="1.0.0"
BUILD_DIR="dist"

echo "=== 正在清理构建目录... ==="
rm -rf ${BUILD_DIR}

PLATFORMS=(
  "darwin/amd64"
  "darwin/arm64"
  "linux/amd64"
  "linux/arm64"
  "windows/amd64"
)

for PLATFORM in "${PLATFORMS[@]}"; do
  GOOS=${PLATFORM%/*}
  GOARCH=${PLATFORM#*/}
  
  TARGET_DIR="${BUILD_DIR}/ai-agent-${VERSION}-${GOOS}-${GOARCH}"
  EXEC_NAME="server"
  
  if [ "$GOOS" = "windows" ]; then
    EXEC_NAME="server.exe"
  fi
  
  echo "--> 正在构建: GOOS=${GOOS} GOARCH=${GOARCH}..."
  mkdir -p "${TARGET_DIR}"
  
  CGO_ENABLED=0 GOOS=${GOOS} GOARCH=${GOARCH} go build \
    -ldflags="-s -w" \
    -o "${TARGET_DIR}/${EXEC_NAME}" \
    ./cmd/server
    
  # 复制配置文件和必要目录结构
  cp config.yaml "${TARGET_DIR}/"
  cp teams.yaml "${TARGET_DIR}/"
  cp -r skills "${TARGET_DIR}/"
  mkdir -p "${TARGET_DIR}/data" "${TARGET_DIR}/logs" "${TARGET_DIR}/workspace"
  
  # 压缩打包
  if [ "$GOOS" = "windows" ]; then
    (cd ${BUILD_DIR} && zip -r "ai-agent-${VERSION}-${GOOS}-${GOARCH}.zip" "ai-agent-${VERSION}-${GOOS}-${GOARCH}")
  else
    (cd ${BUILD_DIR} && tar -czvf "ai-agent-${VERSION}-${GOOS}-${GOARCH}.tar.gz" "ai-agent-${VERSION}-${GOOS}-${GOARCH}")
  fi
  
  echo "✅ 打包完成: ${TARGET_DIR}"
done

echo "=== 所有平台构建完成！输出文件位于 ${BUILD_DIR}/ 目录 ==="
