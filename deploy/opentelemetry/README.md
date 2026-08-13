# OpenTelemetry 本地可观测性环境

本目录提供适用于 `ai-agent` 本地开发的可观测性环境，包括：

- OpenTelemetry Collector：接收应用发送的 OTLP traces 和 metrics。
- Prometheus：保存并查询应用指标与 Tempo 生成的 span metrics。
- Tempo：保存 traces，并为 Grafana 提供 TraceQL 查询。
- Jaeger：提供独立的 trace 查询界面，便于调试和对比。
- Grafana：预置 Prometheus、Tempo 数据源和 `AI Agent Overview` 仪表盘。
- Langfuse：接收 Collector 转发的应用 Trace，与 LiteLLM Generation 关联。

所有宿主机端口都只绑定到 `127.0.0.1`，不对局域网或公网开放。

## 前置条件

- Docker Desktop 或兼容的 Docker Engine
- Docker Compose v2（使用 `docker compose` 命令）
- 确保所需端口未被其他本地进程占用

## 快速启动

从项目根目录执行：

先根据 Langfuse 项目公钥和私钥生成 Basic 凭证，并写入
`deploy/opentelemetry/.env`。不要提交生成后的 `.env`：

```bash
printf '%s' 'pk-lf-public-key:sk-lf-secret-key' | base64
```

配置示例：

```dotenv
OTEL_COLLECTOR_CONFIG_PATH=/etc/otelcol/config-langfuse.yaml
LANGFUSE_OTEL_ENDPOINT=https://langfuse.example.com/api/public/otel
LANGFUSE_OTEL_AUTHORIZATION="Basic BASE64_RESULT"
```

`OTEL_COLLECTOR_CONFIG_PATH` explicitly selects the Langfuse-enabled Collector
configuration. If it is omitted, Collector uses the local Jaeger/Tempo-only
configuration, and lifecycle commands such as `docker compose down` remain
usable without loading Langfuse credentials.

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
Trace 会同时发送到 Jaeger、Tempo 和 Langfuse；metrics 仍进入 Prometheus。
产生一些 API 请求后，等待约 15 秒，再打开 Grafana、Prometheus 或 Langfuse 查看数据。

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

## Multi-Agent DAG Canary 观测与晋级门槛

`AI Agent Overview` 仪表盘包含 DAG 流量占比、窗口调用量、非成功率、fallback、
按 runtime 划分的完成率、P95 和平均延迟。Prometheus 同时加载
`prometheus-rules/multiagent-canary.yml`，提供以下保护性告警：

- DAG 与 Legacy 在 30 分钟内各至少 10 次调用后，DAG 非成功率持续 10 分钟高于
  Legacy 5 个百分点；
- 两侧在 30 分钟内各至少 10 个延迟样本后，DAG P95 持续 10 分钟高于
  Legacy 的 1.20 倍；
- 15 分钟内出现任何 DAG fallback。
- 两侧在 30 分钟内各至少 10 次调用后，DAG 的 approval-required 比例持续
  高于 Legacy 5 个百分点；
- 同样的样本条件下，DAG 的 Replan 比例持续高于 Legacy 5 个百分点。

本地 Compose 栈未包含 Alertmanager；规则状态可在 Prometheus 的 `/alerts` 页面查看。
需要主动通知时，应在实际部署环境中把这些规则接入已有 Alertmanager 或告警平台。

修改规则或首次加入规则目录后，需要重新创建 Prometheus 容器以加载新挂载；这不会
删除命名卷中的历史数据：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  up -d --force-recreate prometheus
```

提升 Canary 比例前，不只依赖告警的最低触发样本。建议在代表性真实流量窗口中至少
累计 20 次 DAG 完成任务，并确认 DAG/Legacy 两侧均有可比较样本、没有 fallback、
DAG 非成功率不高于 Legacy 5 个百分点、DAG P95 不超过 Legacy 的 1.20 倍，且审批与
Replan 行为没有回归。任一条件不满足时保持当前比例；出现 fallback 或明显正确性回归时
将比例回退为 0，重启应用后仅影响新任务。

应用同时导出 `agent_multiagent_runtime_events_total`，以 bounded labels 记录每次 runtime
调用是否进入 `approval_required` 或成功产生 `replanned` Trace。它用于自动比较两侧事件
比例，但不能判断审批请求和重规划内容是否语义正确，因此人工 Trace 复核仍是强制门禁。
部署包含该指标的新版本后，需要重启应用，重启前的调用不会补记为 runtime event。
每次新版本 runtime 调用还会记录 `event=observed`；晋级工具要求 observed 数不少于窗口内
runtime calls。Collector 保留的旧调用若缺少新事件覆盖，将明确导致 HOLD，直到旧样本离开
观察窗口，避免混用指标上线前后口径。

使用只读晋级门禁统一计算上述指标。`--manual-review-passed` 只能在人工检查审批和
Replan Trace 后设置；工具不会自行修改配置。默认通过退出码为 0，HOLD 为 1，查询或
参数错误为 2：

```bash
go run ./cmd/canary-gate \
  --prometheus-url http://127.0.0.1:19090 \
  --window 24h
```

计数使用窗口终点与起点的原始 Counter 差值，不使用会对短生命周期序列做外推的
`increase()`。如果窗口内发生 Counter 重置，样本数会被保守计为零并保持 HOLD；应等待
当前进程积累足够的新样本，而不是把重置前后的计数拼接为晋级依据。

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

如果 Langfuse 没有 Trace，先检查 Collector exporter 日志中的 HTTP 状态：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  logs otel-collector
```

`401`/`403` 通常表示 Basic 凭证不正确；`404` 通常表示 endpoint 缺少
`/api/public/otel` 或反向代理没有转发该路径。Collector 不会在 Span 中记录
Authorization 值。

## 停止与重新启动

停止并移除容器和 Compose 网络：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  down
```

`down` may also be run without `--env-file`; the Compose file no longer uses
required-variable interpolation for Langfuse settings:

```bash
docker compose -f deploy/opentelemetry/docker-compose.yml down
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

### TraceQL `rate()` 报告 `localblocks processor not found`

TraceQL metrics 查询需要 Tempo metrics-generator 的 `local-blocks` processor。当前
配置已经为所有租户启用该 processor，并将生成的 RF1 blocks 写入持久化存储。修改
`tempo.yaml` 后需要重新创建 Tempo 容器：

```bash
docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  up -d --force-recreate tempo
```

然后确认 Tempo 就绪并查看日志：

```bash
curl --fail http://127.0.0.1:3200/ready

docker compose \
  --env-file deploy/opentelemetry/.env \
  -f deploy/opentelemetry/docker-compose.yml \
  logs tempo
```

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
