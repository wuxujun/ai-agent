# OpenTelemetry 本地可观测性环境

本目录提供适用于 `ai-agent` 本地开发的可观测性环境，包括：

- OpenTelemetry Collector：接收应用发送的 OTLP traces 和 metrics。
- Prometheus：保存并查询应用指标与 Tempo 生成的 span metrics。
- Tempo：保存 traces，并为 Grafana 提供 TraceQL 查询。
- Jaeger：提供独立的 trace 查询界面，便于调试和对比。
- Grafana：预置 Prometheus、Tempo 数据源和 `AI Agent Overview` 仪表盘。

所有宿主机端口都只绑定到 `127.0.0.1`，不对局域网或公网开放。

## 前置条件

- Docker Desktop 或兼容的 Docker Engine
- Docker Compose v2（使用 `docker compose` 命令）
- 确保所需端口未被其他本地进程占用

## 快速启动

从项目根目录执行：

```bash
cp deploy/opentelemetry/.env.example deploy/opentelemetry/.env
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  up -d
```

查看容器状态：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  ps
```

首次启动需要拉取镜像，耗时取决于网络环境。

## 服务地址

| 服务 | 本地地址 | 用途 |
| --- | --- | --- |
| Grafana | <http://127.0.0.1:3000> | 仪表盘和 Tempo trace 查询 |
| Prometheus | <http://127.0.0.1:9090>（默认） | PromQL 和指标检查 |
| Jaeger | <http://127.0.0.1:16686> | Trace 查询界面 |
| Tempo | <http://127.0.0.1:3200> | Tempo HTTP API |
| OTLP/HTTP | `http://127.0.0.1:4318` | 应用默认上报端点 |
| OTLP/gRPC | `127.0.0.1:4317` | gRPC 上报端点 |
| Collector metrics | <http://127.0.0.1:9464/metrics> | Prometheus exporter 输出 |

Grafana 的本地默认账号和密码均为 `admin`。该配置只适合本机开发环境。

## Prometheus 端口配置

编辑 `deploy/opentelemetry/.env`：

```dotenv
PROMETHEUS_PORT=19090
```

重新创建 Prometheus 容器后，使用 <http://127.0.0.1:19090> 访问：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  up -d
```

这里只修改宿主机端口。Grafana 和 Tempo 在 Docker 网络内部仍通过
`http://prometheus:9090` 访问 Prometheus，因此不需要同步修改其他配置。

如果没有创建 `.env` 或没有设置 `PROMETHEUS_PORT`，Compose 默认使用宿主机端口 `9090`。

## 启动 ai-agent

项目默认配置已经适配本环境：

```yaml
telemetry:
  enabled: true
  endpoint: "127.0.0.1:4318"
  environment: "dev"
  exporter: "otlp"
```

启动服务：

```bash
go run ./cmd/server
```

应用运行在宿主机时，会通过 OTLP/HTTP 将 traces 和 metrics 发送到 Collector。
产生一些 API 请求后，等待约 15 秒，再打开 Grafana 或 Prometheus 查看数据。

也可以使用环境变量覆盖项目配置：

```bash
AI_AGENT_TELEMETRY_ENABLED=true \
AI_AGENT_TELEMETRY_ENDPOINT=http://127.0.0.1:4318 \
AI_AGENT_TELEMETRY_ENVIRONMENT=dev \
AI_AGENT_TELEMETRY_EXPORTER=otlp \
go run ./cmd/server
```

如果以后将 `ai-agent` 放入容器，不能在应用容器中继续使用
`127.0.0.1:4318`。应用需要加入同一 Docker 网络，并将 endpoint 改为
`http://otel-collector:4318`。

## 验证服务

检查后端是否就绪：

```bash
curl --fail http://127.0.0.1:9090/-/ready
curl --fail http://127.0.0.1:3200/ready
curl --fail http://127.0.0.1:3000/api/health
```

如果修改过 `PROMETHEUS_PORT`，第一条命令中的 `9090` 也要替换为对应端口。

检查 Collector 是否已经导出应用指标：

```bash
curl --fail http://127.0.0.1:9464/metrics | rg '^agent_'
```

查看所有组件日志：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  logs -f
```

只查看 Collector 和 Tempo：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  logs -f otel-collector tempo
```

## 停止与重新启动

停止并移除容器和 Compose 网络：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  down
```

此命令不会删除命名卷。Prometheus、Tempo、Jaeger 和 Grafana 数据会保留，下一次执行
`up -d` 时继续使用。不要为 `down` 增加 `--volumes` 或 `-v` 参数，否则 Compose
会删除命名卷及其中的数据。

重新启动已有容器：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  restart
```

## 数据持久化

Compose 使用以下命名卷：

| 卷 | 内容 |
| --- | --- |
| `opentelemetry_prometheus-data` | Prometheus 时序数据 |
| `opentelemetry_tempo-data` | Tempo traces、WAL 和 metrics-generator WAL |
| `opentelemetry_grafana-data` | Grafana 本地状态 |
| `opentelemetry_jaeger-data` | Jaeger Badger trace 数据和索引 |

这些卷使用固定名称，不受 Compose 项目名变化影响。执行普通 `docker compose down`
后再次执行 `up -d`，各服务会重新挂载同一组卷。Jaeger 使用单节点 Badger 存储，适合
本地开发；生产环境应按容量和可靠性要求选择专用存储后端。

Jaeger 镜像以 UID `10001` 的非 root 用户运行。`jaeger-storage-init` 是一次性初始化
容器，只负责创建 Badger 目录并设置所有者；初始化成功后退出，Jaeger 随后以非 root
用户启动。`docker compose ps -a` 中该容器显示 `Exited (0)` 属于正常状态。

## 常见问题

### 应用日志持续出现 telemetry connection/export error

确认 Collector 正在运行，并检查 `AI_AGENT_TELEMETRY_ENDPOINT` 是否指向
`http://127.0.0.1:4318`：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  ps otel-collector
```

### Grafana 仪表盘没有数据

确认应用已经产生请求，并等待一次应用导出周期和 Prometheus 抓取周期。还可以在
Prometheus 中查询 `agent_planner_calls_total`，判断指标是否已经进入 Prometheus。

### Prometheus 端口被占用

在 `.env` 中将 `PROMETHEUS_PORT` 改为其他未占用端口，然后再次执行 `up -d`。

### Tempo 中没有 traces

先查看 Collector 与 Tempo 日志，并确认 Collector 没有连接 `tempo:4317` 失败的错误。
Tempo 的 OTLP receiver 已显式监听容器网络接口 `0.0.0.0:4317` 和
`0.0.0.0:4318`。

### Jaeger 报告 `/badger` permission denied

确认一次性初始化容器执行成功：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  ps -a jaeger-storage-init
```

然后查看初始化日志：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  logs jaeger-storage-init jaeger
```
