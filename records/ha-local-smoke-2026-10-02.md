# 阶段 3.3 本机 Linux 双容器 Smoke（2026-10-02）

## 结果与范围

两个 Linux/arm64 Agent 容器使用同一提交构建的 Server，共享专用 PostgreSQL、Redis、只读 workspace、租户和审批密钥。离线 stub 提供模型生成和 embedding，内部隔离网络未发布服务端口。两分钟真实 API smoke **12 个任务全部通过，A/B 各 6 个，P95 为 529 ms**。跨实例持久化终态、回答与完整 Trace 对比，以及对端终态 SSE 检查均通过；12 个任务的租户均为 `ha_smoke`。

这是单台 Docker Desktop 主机上的两个 Agent 进程，**不是两个独立 Linux 节点的 HA 验收**。未运行一小时负载、未注入任务执行故障、未验证审批 API CAS/重复副作用或 Prometheus 灰度门禁。报告保持 `window_complete=false`、`load_checks_passed=false`、`full_ha_acceptance=false`。Smoke 包装脚本退出码为 0，底层短时负载工具返回 1 符合预期。

## 环境与发布身份

- Server 和 `ha-soak` 源码：提交 `e7e1319d04303a3a83551c63f3a273d440a93b0d` 的干净 `git archive` 快照，未包含工作区其他代码改动。
- 构建：`CGO_ENABLED=0 GOOS=linux GOARCH=arm64`。两个 Agent 内 `uname -sm` 均返回 `Linux aarch64`。
- 两实例实际 Server SHA256 一致：`0fe1e5d656cbce04c844cdc431d719b8672b84a621208829f5d3f1dfa0503546`。这是本次 arm64 测试二进制，与此前 amd64 正式发布包区别记录。
- Compose 项目：`ai-agent-stage33-smoke-o80g71f4`；Agent 容器 ID 为 A `f804a6209481`、B `9b18370312a4`。
- Agent/stub/control 基础镜像：`busybox:1.37.0`，本机实际镜像 ID `sha256:6df9636795d37473994366014c25264edeb6c00d7a57188ff62d5a94276b4297`。
- 共享 PostgreSQL 使用 `pgvector/pgvector:0.8.2-pg17-bookworm`，专用库 `ai_agent_ha_smoke`，连接上限 150；Redis 使用 `redis:7.4.2-alpine` 的专用实例 DB 15。
- 测试 Team 的 planner/researcher 仅允许 `find_files`、`read_file`；workspace 以只读卷挂载，README 内容验证未变。
- 租户、模型 stub 和审批凭据随机生成，仅留在仓库外权限为 0600 的文件中。stub 统计为 24 次生成、1 次 embedding；没有调用真实模型。
- readiness 模式为 `gateway`：A/B 的 `/ping`、`/ready` 均为 200，全部 LLM scene 的 gateway 检查通过；`llm_verified=false` 表示未执行 inference readiness 探测。后续实际任务完成提供了生成链路证据。
- 两实例 `/api/metrics` 未认证返回 401，管理员凭据访问返回 200。保存了指标快照，没有建立 Prometheus 连续采集窗口。

## 执行窗口与命令

真实负载窗口：`2026-10-02T13:34:16.207263378Z` 至 `2026-10-02T13:36:16.210027378Z`，约 120 秒。包含预检、任务后复核与停止夹具的控制区间为 `13:34:02.606167Z` 至 `13:36:22.622373Z`。

控制端在同一内部 Docker 网络运行 Linux `ha-soak`，主机包装程序只传递参数并读取输出。调用已有脚本：

```bash
bash deploy/ha/test-load.sh \
  --bundle /private/tmp/ai-agent-stage33-smoke-o80g71f4/control-bundle \
  --mode smoke --node-a http://agent-a:8088 --node-b http://agent-b:8088 \
  --allow-private-network --workspace /app/workspace --team ha_smoke \
  --goal 'Read README.md and summarize it; do not modify files or execute commands.' \
  --output /private/tmp/ai-agent-stage33-smoke-o80g71f4/control/smoke-001
```

`control-bundle/ha-soak` 是转发到隔离控制容器的脚本；它不是正式 Linux 发布包。调用凭据通过环境注入，不出现在命令中。初始准备中发现宿主机代理影响读取、内部网络无法从宿主机访问映射端口，以及检查器错误要求 gateway 模式的 inference 标志；最终控制端移入内部网络并按实际 readiness 语义验证，未修改生产代码。

## 运行时样本

报告的 `runtime_samples` 为 **`unknown: 12`**。逐任务持久化 Trace 含 plan、find_files、read_file、write（writer 合成）事件，没有 `multiagent_runtime_selection`。基础提交的 `resolveTaskRuntime` 仅在 Legacy 加非零百分比灰度时追加该选择 Trace，本次保持 0%，所以不从配置推断 Legacy 或伪造运行时样本。本次不能提供 DAG/Legacy 的 Trace 对照；目标灰度窗口仍需核验选择 Trace。

## 证据与后续

- [结构化汇总](ha-local-smoke-2026-10-02.json)：节点身份、二进制摘要、readiness/鉴权、负载报告及范围声明。
- [12 个任务的脱敏逐任务结果](ha-local-smoke-2026-10-02-results.jsonl)：任务 ID、提交实例、终态、延迟、Trace/终态摘要，不包含回答正文或凭据。
- 原始夹具、stub/probe 源码、运行程序、指标快照及构建产物位于 `/private/tmp/ai-agent-stage33-smoke-o80g71f4`，可能被临时目录清理。不要将其中的凭据/config 直接提交到仓库。

结束后 Agent A/B、控制端、stub、PostgreSQL、Redis 六个容器均已停止，数据卷和夹具文件保留。下一步仍是提供两个独立 Linux 测试节点、共享存储/workspace、Prometheus 和故障注入范围，在目标环境完成 smoke、一小时在线负载、故障及审批验收，随后按门禁推进 20% → 50% → 100%。
