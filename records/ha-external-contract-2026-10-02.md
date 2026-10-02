# 阶段 3.3 本地真实存储契约验证（2026-10-02）

## 结论与范围

提交 `e7e1319d04303a3a83551c63f3a273d440a93b0d` 的六项外部存储契约测试已在真实 PostgreSQL/Redis 上运行并通过，包含子用例共 27 项；退出码为 0，失败、跳过及缺失用例均为 0。启用了 Go 竞态检测，未出现竞态报告。

测试源为 `git archive HEAD` 导出的干净快照，与现有发布包对应。当前工作区另有审批 API/Store 等未提交改动，本记录不为这些改动提供发布验收结论。

这是单台 macOS 开发机上的独立 Docker 存储夹具验证，**不是 Linux 双节点 HA 验收**。阶段 3.3 的目标节点部署、一小时在线负载、进程故障演练和 DAG 灰度放量仍待执行。

## 环境与证据

- 主机：Darwin/arm64；Go 工具链报告 `go1.26.8 darwin/amd64`。
- Docker Engine：29.7.2，Linux/arm64；本轮检查 daemon 已可用。
- 专用 Compose 项目：`ai-agent-stage33-contract-t41b6vda`，使用提交内的 `deploy/ha/compose.yaml`，创建独立容器、网络和数据卷。
- PostgreSQL 镜像：`pgvector/pgvector:0.8.2-pg17-bookworm`；实际数据库版本 `17.10 (Debian 17.10-1.pgdg12+1)`；数据库为专用 `ai_agent_ha`。
- Redis：`redis:7.4.2-alpine`，测试使用专用实例的 DB 15。
- 夹具仅映射至本机 `127.0.0.1:15432` / `127.0.0.1:16379`。
- 凭据在运行时生成并通过进程环境注入，保存在仓库外权限为 0600 的文件中；本记录不包含凭据。
- 包含夹具启动和停止的执行区间：`2026-10-02T08:12:02.216681Z` 至 `2026-10-02T08:13:03.821995Z`。该区间不是持续负载窗口。
- 脱敏 JSONL：`/private/tmp/ai-agent-stage33-contract-t41b6vda/contracts.jsonl`；原始结构化汇总：同目录 `summary.json`。临时目录可能被清理；脱敏汇总及 JSONL SHA256 已另存为仓库内的[结构化证据](ha-external-contract-2026-10-02.json)。
- 完成后两个夹具容器均已停止；保留容器、网络与数据卷，没有删除数据。

## 执行命令与结果

执行前已设置 `AI_AGENT_RUN_EXTERNAL_INTEGRATION=true`、`TEST_POSTGRES_DSN` 和 `TEST_REDIS_URL`。仅将本次新建的专用夹具连接提供给测试；禁止复用业务数据库。

```bash
GOCACHE=/private/tmp/ai-agent-go-cache go test -race -json ./internal/store ./internal/api \
  -run '^(TestExternalStoresTaskCreation|TestExternalStoresTaskLeaseGuard|TestExternalStoresPersistPausedTaskAcrossClients|TestExternalPostgresDurableApprovalCASAcrossClients|TestExternalPostgresDurableApprovalRecoveryContract|TestPostgresPoolExternal)$' \
  -count=1 -timeout=5m
```

| 顶层用例 | 结果 | 用例耗时（秒） |
|---|---|---:|
| `TestExternalStoresTaskCreation` | PASS，PostgreSQL/Redis 均执行 | 0.31 |
| `TestExternalStoresTaskLeaseGuard` | PASS，PostgreSQL/Redis 均执行 | 0.15 |
| `TestExternalStoresPersistPausedTaskAcrossClients` | PASS，PostgreSQL/Redis 均执行 | 0.07 |
| `TestExternalPostgresDurableApprovalCASAcrossClients` | PASS | 0.07 |
| `TestExternalPostgresDurableApprovalRecoveryContract` | PASS，5 个子用例均执行 | 0.77 |
| `TestPostgresPoolExternal` | PASS | 0.27 |

JSONL 的 `pass`、`fail`、`skip` 事件与预期六项逐一核对，六项均出现 `pass`。审批恢复覆盖参数替换、原参数恢复、空参数替换、重启后的已消费 checkpoint，以及提交前失去租约。连接池验证包含实际连接上限、排队和归还。

## 同一发布快照的全仓验证

```bash
GOCACHE=/private/tmp/ai-agent-go-cache go test -p=4 ./... -count=1 -timeout=5m
```

退出码为 0，49 个含测试的包通过。全仓测试未设置外部集成环境，外部用例的实际执行证据以上述独立六项为准。首次全仓运行因沙箱禁止 `httptest` 绑定本地端口而失败，获授权在沙箱外重跑后通过，未修改代码。输出保存在 `/private/tmp/ai-agent-stage33-contract-t41b6vda/full-suite.log`，日志 SHA256 已记入结构化证据。

交接文档的本地链接与尾随空白检查、`git diff --check` 也通过。

## 发布包复核与连接配额

已有发布目录 `/private/tmp/ai-agent-ha-e7e1319-linux-amd64` 仍在。重新执行 `shasum -a 256 -c SHA256SUMS`，清单内全部 12 个文件通过。清单标记 `git_dirty=false`、`goos=linux`、`goarch=amd64`，Server SHA256 保持 `4b46f23a91130c02d0c5e27c7bebcda430f13c3d61999bd8733525a1338adabc`。目标节点架构尚未提供，不能据此认定发布包适配目标环境。

该次本地夹具 `SHOW max_connections` 返回 **100**。仓库配置每实例 `max_open_conns=50`，双实例合计即 100，未给监控、迁移和数据库保留连接留出余量。正式演练前应增加目标数据库可用连接配额，或降低两实例连接池上限；还需计入 PostgreSQL 保留连接和其他客户端。单个连接池用例通过不代表双实例容量已验收。

同日后续已将本地夹具调整到 150 条，并用双连接池契约同时持有 110 条真实连接；该次新验证详见[容量回归记录](ha-capacity-regression-2026-10-02.md)。本记录保留原始 100 条夹具的结果，目标环境配额仍待确认。

## 下一步

继续按[续接清单](../docs/阶段3.3未完成事项与下次续接.md)准备两个独立 Linux 测试节点、共享存储/workspace、认证/审批密钥、离线 stub 或模型预算、Prometheus 和故障注入范围。在目标共享数据库重新执行上述契约，再部署两个节点并运行 smoke。仓库保持 `runtime=legacy`、`dag_canary_percent=0`；审批 CAS 的客户端契约不能替代跨节点 API 与工具副作用演练。
