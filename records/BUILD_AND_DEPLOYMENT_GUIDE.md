# AI Agent 多平台编译打包与安装发布指南

本文档涵盖 **macOS (macOS ARM64 / Intel)**、**Linux (Ubuntu / Debian x86_64 & ARM64)** 和 **Windows (x86_64)** 平台的编译打包命令、可执行文件打包发布流程以及运行部署方法。

---

## 目录
- [一、准备工作](#一准备工作)
- [二、交叉编译命令 (Cross-Compilation)](#二交叉编译命令-cross-compilation)
  - [1. macOS (Darwin)](#1-macos-darwin)
  - [2. Linux (Ubuntu / Debian)](#2-linux-ubuntu--debian)
  - [3. Windows](#3-windows)
  - [4. 一键交叉编译构建脚本](#4-一键交叉编译构建脚本)
- [三、发布包制作 (Packaging)](#三发布包制作-packaging)
  - [1. macOS (.tar.gz / Zip)](#1-macos-targz--zip)
  - [2. Linux (.tar.gz / .deb)](#2-linux-targz--deb)
  - [3. Windows (.zip)](#3-windows-zip)
- [四、各平台安装与运行部署](#四各平台安装与运行部署)
  - [1. macOS 安装部署](#1-macos-安装部署)
  - [2. Linux (Ubuntu) 服务化部署 (systemd)](#2-linux-ubuntu-服务化部署-systemd)
  - [3. Docker / Container 镜像发布](#3-docker--container-镜像发布)
  - [4. Windows 服务化部署 (NSSM)](#4-windows-服务化部署-nssm)
- [五、GitHub Actions 自动化 CI/CD 打包与 Release 发布](#五github-actions-自动化-cicd-打包与-release-发布)

---

## 一、准备工作

### 1. 环境需求
- **Go 语言版本**：Go 1.25 或以上。
- **CGO 配置**：项目默认尽量保持 `CGO_ENABLED=0` 纯 Go 编译，以最大化交叉编译兼容性（底层使用现代 C 转换包如 `modernc.org/sqlite`）。

### 2. 编译打包需包含的静态文件与配置
一个完整的发布安装包包含以下文件：
- 编译生成的二进制文件（例如 `server` 或 `server.exe`）
- 主配置文件：`config.yaml`（及可选的 `config_zh.yml`）
- 团队多智能体配置文件：`teams.yaml`（及可选的 `teams_zh.yml`）
- 技能与 Prompt 目录：`skills/`
- 数据与日志存放目录创建：`data/`、`logs/`、`workspace/`

---

## 二、交叉编译命令 (Cross-Compilation)

通过环境变量设置目标操作系统 (`GOOS`) 与目标架构 (`GOARCH`)。

### 1. macOS (Darwin)

```bash
# macOS Apple Silicon (M1/M2/M3/M4, arm64)
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o dist/darwin-arm64/server ./cmd/server

# macOS Intel (x86_64, amd64)
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o dist/darwin-amd64/server ./cmd/server
```

> **参数说明**：`-ldflags="-s -w"` 用于去除符号表和调试信息，压缩二进制文件体积约 20%~30%。

---

### 2. Linux (Ubuntu / Debian)

```bash
# Linux 64位 Intel/AMD (x86_64, amd64)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/linux-amd64/server ./cmd/server

# Linux ARM64 (如 树莓派 / 云厂商 ARM 架构服务器)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o dist/linux-arm64/server ./cmd/server
```

---

### 3. Windows

```bash
# Windows 64位 (x86_64, amd64)
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dist/windows-amd64/server.exe ./cmd/server
```

---

### 4. 一键交叉编译构建脚本

创建一个 Shell 脚本 `build.sh` 批量构建所有主流平台：

```bash
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
```

运行编译命令：
```bash
chmod +x build.sh
./build.sh
```

---

## 三、发布包制作 (Packaging)

### 1. macOS (.tar.gz / Zip)
打包包含程序与配置的归档文件：
```bash
tar -czvf ai-agent-v1.0.0-darwin-arm64.tar.gz -C dist/darwin-arm64 .
```

### 2. Linux (.tar.gz / .deb)
#### (1) 标准 Tarball 归档：
```bash
tar -czvf ai-agent-v1.0.0-linux-amd64.tar.gz -C dist/linux-amd64 .
```

#### (2) Ubuntu / Debian `.deb` 安装包制作：
准备构建结构：
```bash
mkdir -p debian-pkg/DEBIAN
mkdir -p debian-pkg/usr/local/bin
mkdir -p debian-pkg/etc/ai-agent

# 复制可执行文件与配置
cp dist/linux-amd64/server debian-pkg/usr/local/bin/ai-agent
cp config.yaml debian-pkg/etc/ai-agent/config.yaml
cp teams.yaml debian-pkg/etc/ai-agent/teams.yaml

# 创建 control 文件
cat <<EOF > debian-pkg/DEBIAN/control
Package: ai-agent
Version: 1.0.0
Architecture: amd64
Maintainer: wuxujun <wuxujun@example.com>
Description: Production-grade Go AI Agent Execution Engine
EOF

# 打包 deb
dpkg-deb --build debian-pkg ai-agent_1.0.0_amd64.deb
```

### 3. Windows (.zip)
使用 PowerShell 或 zip 命令打包：
```powershell
Compress-Archive -Path dist/windows-amd64/* -DestinationPath ai-agent-v1.0.0-windows-amd64.zip
```

---

## 四、各平台安装与运行部署

### 1. macOS 安装部署

#### (1) 解压与权限设置
```bash
tar -xzvf ai-agent-v1.0.0-darwin-arm64.tar.gz -C /usr/local/ai-agent/
cd /usr/local/ai-agent/
chmod +x server
```

#### (2) 配置环境变量并启动
```bash
export OPENAI_API_KEY="your-api-key"
export AI_AGENT_API_ADDR="127.0.0.1:8088"

./server
```

#### (3) 配置 macOS launchd 后台开机自启服务
创建 `~/Library/LaunchAgents/com.aiagent.server.plist`：
```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.aiagent.server</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/ai-agent/server</string>
    </array>
    <key>WorkingDirectory</key>
    <string>/usr/local/ai-agent</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>OPENAI_API_KEY</key>
        <string>your-api-key</string>
        <key>AI_AGENT_API_ADDR</key>
        <string>127.0.0.1:8088</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
```
加载并启动服务：
```bash
launchctl load ~/Library/LaunchAgents/com.aiagent.server.plist
```

---

### 2. Linux (Ubuntu) 服务化部署 (systemd)

#### (1) 安装与解压
```bash
sudo mkdir -p /opt/ai-agent
sudo tar -xzvf ai-agent-v1.0.0-linux-amd64.tar.gz -C /opt/ai-agent --strip-components=1
sudo chmod +x /opt/ai-agent/server
sudo useradd --system --home-dir /opt/ai-agent --shell /usr/sbin/nologin ai-agent
sudo chown -R ai-agent:ai-agent /opt/ai-agent/data /opt/ai-agent/logs /opt/ai-agent/workspace
```

#### (2) 创建 systemd 服务文件
Linux 发布包已包含 `ai-agent.service`。安装服务文件并创建仅 root 可读的环境变量文件：
```bash
sudo install -m 0644 /opt/ai-agent/ai-agent.service /etc/systemd/system/ai-agent.service
sudo install -d -m 0750 /etc/ai-agent
sudo touch /etc/ai-agent/ai-agent.env
sudo chmod 0600 /etc/ai-agent/ai-agent.env
sudo editor /etc/ai-agent/ai-agent.env
```

`/etc/ai-agent/ai-agent.env` 示例：
```dotenv
AI_AGENT_API_ADDR=0.0.0.0:8088
AI_AGENT_API_KEY=replace-with-a-strong-admin-key
AI_AGENT_STORE_TYPE=sqlite
AI_AGENT_STORE_DSN=/opt/ai-agent/data/agent.db
AI_AGENT_LLM_PROVIDER=litellm
AI_AGENT_LLM_BASE_URL=http://127.0.0.1:4000/v1/chat/completions
AI_AGENT_LLM_API_KEY=replace-with-a-litellm-virtual-key
LANGFUSE_ENABLED=true
LANGFUSE_BASE_URL=https://cloud.langfuse.com
LANGFUSE_PUBLIC_KEY=replace-with-langfuse-public-key
LANGFUSE_SECRET_KEY=replace-with-langfuse-secret-key
```

#### (3) 启动与开启自启
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ai-agent
sudo systemctl status ai-agent
sudo journalctl -u ai-agent -f
```

---

### 3. Docker / Container 镜像发布

#### (1) Dockerfile
在项目根目录创建 `Dockerfile`：
```dockerfile
# 编译阶段
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o server ./cmd/server

# 运行阶段
FROM alpine:latest

# 安装 ripgrep 及基础设施工具
RUN apk add --no-cache ca-certificates ripgrep findutils bash tzdata

WORKDIR /app
COPY --from=builder /app/server .
COPY --from=builder /app/config.yaml .
COPY --from=builder /app/teams.yaml .
COPY --from=builder /app/skills ./skills

RUN mkdir -p data logs workspace

EXPOSE 8088

ENTRYPOINT ["./server"]
```

#### (2) 构建与运行容器
```bash
# 构建镜像
docker build -t ai-agent:1.0.0 .

# 运行容器
docker run -d \
  --name ai-agent \
  -p 8088:8088 \
  -e OPENAI_API_KEY="your-api-key" \
  -v $(pwd)/data:/app/data \
  -v $(pwd)/workspace:/app/workspace \
  ai-agent:1.0.0
```

---

### 4. Windows 服务化部署 (NSSM)

#### (1) 解压文件
解压 `ai-agent-v1.0.0-windows-amd64.zip` 至 `C:\ai-agent`。

#### (2) 方式 A：命令行直接运行 (PowerShell / CMD)
```powershell
$env:OPENAI_API_KEY="your-api-key"
$env:AI_AGENT_API_ADDR="127.0.0.1:8088"

.\server.exe
```

#### (3) 方式 B：使用 NSSM 注册为 Windows 后台服务
1. 下载 [NSSM (Non-Sucking Service Manager)](https://nssm.cc/) 并放入 PATH。
2. 使用管理员身份打开 CMD/PowerShell 运行：
```powershell
nssm install AIAgentService "C:\ai-agent\server.exe"
nssm set AIAgentService AppDirectory "C:\ai-agent"
nssm set AIAgentService AppEnvironmentExtra "OPENAI_API_KEY=your-api-key" "AI_AGENT_API_ADDR=127.0.0.1:8088"
nssm start AIAgentService
```

---

## 五、GitHub Actions 自动化 CI/CD 打包与 Release 发布

已经在项目 `.github/workflows/release.yml` 中集成了全平台的自动化打包与 GitHub Release 自动发布工作流。

### 1. 自动化打包构建的工作流配置 (`release.yml`)

```yaml
name: Release Multi-Platform Packages

on:
  push:
    tags:
      - 'v*'
  workflow_dispatch:
    inputs:
      version:
        description: 'Release Version (e.g. v1.0.0)'
        required: true
        default: 'v1.0.0'

permissions:
  contents: write

jobs:
  build-release:
    name: Build & Upload Release Artifacts
    runs-on: ubuntu-latest
    timeout-minutes: 30

    strategy:
      matrix:
        include:
          - os: darwin
            arch: amd64
            artifact_name: ai-agent-darwin-amd64
            asset_name: ai-agent-darwin-amd64.tar.gz
          - os: darwin
            arch: arm64
            artifact_name: ai-agent-darwin-arm64
            asset_name: ai-agent-darwin-arm64.tar.gz
          - os: linux
            arch: amd64
            artifact_name: ai-agent-linux-amd64
            asset_name: ai-agent-linux-amd64.tar.gz
          - os: linux
            arch: arm64
            artifact_name: ai-agent-linux-arm64
            asset_name: ai-agent-linux-arm64.tar.gz
          - os: windows
            arch: amd64
            artifact_name: ai-agent-windows-amd64
            asset_name: ai-agent-windows-amd64.zip

    steps:
      - name: Checkout Code
        uses: actions/checkout@v4

      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.25.x'
          cache: true

      - name: Package Release Directory Structure
        run: |
          mkdir -p build_stage/${{ matrix.artifact_name }}
          EXEC_NAME="server"
          if [ "${{ matrix.os }}" = "windows" ]; then
            EXEC_NAME="server.exe"
          fi

          CGO_ENABLED=0 GOOS=${{ matrix.os }} GOARCH=${{ matrix.arch }} go build \
            -ldflags="-s -w" \
            -o build_stage/${{ matrix.artifact_name }}/${EXEC_NAME} \
            ./cmd/server

          cp config.yaml build_stage/${{ matrix.artifact_name }}/
          cp teams.yaml build_stage/${{ matrix.artifact_name }}/
          cp -r skills build_stage/${{ matrix.artifact_name }}/
          mkdir -p build_stage/${{ matrix.artifact_name }}/data \
                   build_stage/${{ matrix.artifact_name }}/logs \
                   build_stage/${{ matrix.artifact_name }}/workspace

      - name: Create Archive (.tar.gz / .zip)
        run: |
          cd build_stage
          if [ "${{ matrix.os }}" = "windows" ]; then
            zip -r ../dist/${{ matrix.asset_name }} ${{ matrix.artifact_name }}
          else
            tar -czvf ../dist/${{ matrix.asset_name }} ${{ matrix.artifact_name }}
          fi
          cd ..

      - name: Upload Artifacts
        uses: actions/upload-artifact@v4
        with:
          name: ${{ matrix.asset_name }}
          path: dist/${{ matrix.asset_name }}

  publish-release:
    name: Create GitHub Release
    needs: build-release
    runs-on: ubuntu-latest
    steps:
      - name: Checkout Code
        uses: actions/checkout@v4

      - name: Download All Artifacts
        uses: actions/download-artifact@v4
        with:
          path: ./release-assets

      - name: Move Assets to Flat Directory
        run: |
          mkdir -p ./final-assets
          find ./release-assets -type f \( -name "*.tar.gz" -o -name "*.zip" \) -exec cp {} ./final-assets/ \;

      - name: Create GitHub Release
        uses: softprops/action-gh-release@v2
        with:
          files: ./final-assets/*
          draft: false
          prerelease: false
          generate_release_notes: true
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

### 2. 如何使用该工作流

1. **Tag 触发**（自动发布版本）：
   ```bash
   git tag v1.0.0
   git push origin v1.0.0
   ```
2. **手动触发**：
   在项目 GitHub 仓库页面的 **Actions -> Release Multi-Platform Packages -> Run workflow** 页面直接运行，即可自动构建并生成 GitHub Release 发布包。

---

## 总结

| 平台 | 构建命令 | 二进制程序 | 发布推荐格式 |
|:---|:---|:---|:---|
| **macOS (ARM64)** | `GOOS=darwin GOARCH=arm64 go build ...` | `server` | `.tar.gz` / `launchd` |
| **macOS (Intel)** | `GOOS=darwin GOARCH=amd64 go build ...` | `server` | `.tar.gz` |
| **Linux (x86_64)** | `GOOS=linux GOARCH=amd64 go build ...` | `server` | `.tar.gz` / `.deb` / `Docker` / `systemd` |
| **Linux (ARM64)** | `GOOS=linux GOARCH=arm64 go build ...` | `server` | `.tar.gz` / `Docker` |
| **Windows (x64)** | `GOOS=windows GOARCH=amd64 go build ...` | `server.exe` | `.zip` / `NSSM Service` |
