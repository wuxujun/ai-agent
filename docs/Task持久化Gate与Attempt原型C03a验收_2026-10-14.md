# 持久化 Gate 与 Attempt 原型 C03a 验收

提交状态（2026-10-14）：本文与 C02a/b/c、C03a/b1/b2/b3 及 C03b4 首批 `/run` 实现一并纳入本批本地提交，准确提交号以 Git 记录为准。下文保留各阶段实现和验证边界；当前整体停点与最终验证见[会话续接文档](会话续接与完成进度_2026-10-13.md)。

2026-10-14，基线 `86bd534`。C02a/C02b/C02c/C03a 已纳入本批本地提交。C03a 只在新建专用 SQLite fixture 内实现单任务 gate、attempt 和租约协议；默认 server、API、Engine/Coordinator 和启动/审批恢复未接入。真实隔离/权威验收 NOT_RUN，不能签 stopped 或执行恢复。


后续 [C03b1 实际执行入口的可选准入检查](Task执行器准入接入C03b1验收_2026-10-14.md) 已实现并纳入本批本地提交。完整 owned scope/续租/退出/最后 flush/cleanup 仍待 C03b2，默认配置未启用。
## 专用入口与持久化记录

新增 `internal/store/sqlite_execution_gate.go`：`NewSQLiteStoreWithExecutionGate(path, namespaceSHA256)` 先新建 C02 内容版本专用库，再安装控制表/租约守卫并固定最终 schema fingerprint。原有 Core DML 内容版本继续生效；普通 SQLite 构造器不增加这些表或切换行为，没有配置/HTTP/CLI 开关，也没有原型可写重开。

控制记录包括：

- `execution_control`：有界全局控制 epoch，最大 8192。
- `execution_gates`：最多 256 个单任务 gate，绑定 tenant 与 Task 创建 incarnation；phase 只允许 open/blocked/draining，独立 gate epoch。
- `execution_attempts`：最多 1024 个保留记录，唯一 attempt ID、instance 摘要、Task incarnation、准入 gate epoch、running/exited/complete 状态和三类回执摘要；每任务最多一个未完成 attempt。
- `execution_events`：enroll/start/blocked/draining/exit/finalize 事件，与控制状态、业务租约在同一事务。

控制操作先获得 SQLite 控制写锁，然后核对核心内容版本与控制 head/事件连续性，检查 materialized gate/attempt 状态与控制事件对应，再变更并提交；失败整笔回滚。控制历史尚未建立独立 content-bound floor，不把本地事件表称为可信全实例目录。

`EnrollSQLiteExecutionGate` 只接入当前 Task incarnation，已有 gate/lease/attempt 拒绝，不能给旧 ID 自动重新登记或打开 gate。没有任何 gate/attempt 删除接口；保留历史触发器拒绝正常 SQL 删除。控制数据管理权限隔离仍未证明，不能防御有数据库管理权限者完整改写记录。

## 禁派发与租约原子性

`StartSQLiteExecutionAttempt` 在同一事务中：

1. 检查已登记且 open 的 gate、tenant、Task incarnation 和预期 gate epoch。
2. 拒绝旧 attempt ID、未完成的旧 attempt（即使其租约已过期）、任何遗留 lease 或超出预算。
3. 插入新的 running attempt，创建唯一对应 owner 的 lease，追加控制事件，一起提交。

`BlockSQLiteExecutionGate` 使用 gate epoch CAS，允许 open→blocked/draining、blocked→draining；没有 open 恢复接口，触发器禁止关闭后回到 open。draining 是协议状态，函数不发送取消、不调用执行器。

专用库对 task_leases 安装 INSERT/UPDATE/DELETE 守卫：没有已登记且 scope/incarnation 匹配的未完成 attempt，直接获取普通租约也拒绝；新 lease 需要 open gate。原 owner 可在 blocked/draining 期间正常续租，以便最后 flush；owner 不能改写，过期 ownership 不允许通过 SQL 延长复活。时间由注册的 `ai_agent_execution_now_ns_v1` 函数在检查时取得，普通 Store 不调用该函数。

自然过期保持 running/exited attempt，诊断为 expired-unresolved；TTL 不会清理 attempt 或产生 stopped 回执。已有未完成 attempt 时不能释放 lease/删除 Task，以免丢失退出和清理证据。默认 `RenewTaskLease` 的锁后时间检查继续保留，C03 SQL 守卫是专用库的额外约束。

## 退出与完成回执

`ExitSQLiteExecutionAttempt` 先记录 exit 摘要，将 running→exited；不会自动释放 lease。

`FinalizeSQLiteExecutionAttempt` 只有 exited 且已有 exit 摘要才允许，必须同时提供 flush 与 cleanup 摘要；同事务将状态改为 complete、删除本 attempt 对应 lease、记录 finalize。缺任意回执或错误 instance/scope 拒绝。精确回执重试不重复发布事件，冲突回执拒绝；Start ID 始终不能重用，读取旧 attempt 不是重新执行许可。

所有回执均为当前调用方提供的协议夹具字段，没有真实实例身份认证、Engine 退出观测、宿主机隔离、工具清理或外部副作用对账。不得用这些 fake 摘要替代真实回执。

## 只读诊断与重开

`ReadSQLiteExecutionGateSnapshot` 在一笔只读事务内核对核心历史、控制 head/事件、当前 gate incarnation、attempt 状态/回执及 lease。输出无 lease owner/Goal/Trace 内容，固定 status=unverified。

`ReceiptsComplete` 仅在 gate 已关闭、该单任务所有 attempt 均 complete 且无 lease 时为 true；这是局部协议一致性字段，未映射为 sealed/stopped 或恢复准入。固定缺口包含 single_task_fixture_only、runtime_entrypoints_not_integrated、instance_receipts_not_authenticated、host_cleanup_not_reconciled、independent_control_floor_unavailable。

`OpenSQLiteExecutionGateReader(path, corePin)` 通过原有 mode=ro/query_only 核心 pin 打开，核对控制表存在，再提供 scope-specific 诊断；不能写入、启动或解除 gate。corePin 绑定核心内容历史，不独立绑定控制历史，控制库回滚/分叉的独立锚点仍待接入。

## 验证

全部 dedicated temp fixture，无线上业务库、生产密钥、真实执行器/容器或外部模型调用。

- 正常 enroll/start/block/exit/finalize 生命周期，退出与完整清理明确分开，缺回执、错误顺序、冲突重试拒绝，精确回执重试状态不变。
- blocked/draining 后禁止新 attempt，原 attempt 可以续租完成收尾；不能自动 reopen 或从 draining 回退。
- 直接普通租约获取、过期租约续约/raw SQL 延长、过期 unresolved attempt 被新 attempt 替换均拒绝；释放租约/删除任务不能隐藏未完成 attempt。
- 12 轮并发 block/start：要么完整注册 attempt+lease 后留下 blocked gate，要么 start 拒绝且没有任何新 attempt/lease。
- 注入控制事件写入失败：attempt、lease、核心版本和控制数据全部回滚；stale CAS、错租户、取消拒绝。
- control 事件缺失后读取和后续控制写入拒绝；任务删除重建后旧 incarnation 的 gate 不允许新 attempt 或自动重登记。
- query_only 诊断一致；关闭数据库后只读重开保留 draining gate 和 unresolved attempt，不能写入。
- C02a/b/c 全部回归继续执行；C03 专属提交前/后强制进程退出、完整运行入口与宿主机残留对账尚未验收，应在接入后补充，不用普通关闭重开代替崩溃验收。

```bash
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test ./internal/store -run '^TestSQLite(ExecutionGate|Restore(Versions|Content|Baseline|Startup))' -count=1 -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -p 1 ./... -timeout 120s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -race -p 1 ./internal/store ./internal/authorityledger ./cmd/task-restore -timeout 180s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go vet ./internal/store ./internal/authorityledger ./cmd/task-restore
git diff --check
```

最终定向、串行全仓、Store/authorityledger/CLI 竞态、专项 vet、相对链接与差异格式检查均 PASS。日志 `/private/tmp/ai-agent-c03a-target-20261014.log`、`/private/tmp/ai-agent-c03a-full-20261014.log`、`/private/tmp/ai-agent-c03a-race-20261014.log`。全仓 httptest 按权限流程在沙箱外运行。

## 下一步

C03b 先核对真实 Engine/Coordinator 入口与租约生命周期，定义可选 gate/attempt admission、退出/最后 flush/renewal 停止/工具 cleanup 的集成顺序，再以明确 opt-in 专用执行夹具覆盖直接执行、HTTP、启动/审批恢复和取消。还需完整会话闭包/实例目录、unknown 崩溃处理、控制历史独立 floor 与真实 cleanup/authentication，不能把单任务原型宣称为完整集群隔离。

C02 的真实写者保护/fencing/独立服务仍未接入，C04 signer/trust 与其他强制恢复证据未完成；HA/DAG 延后，默认运行配置不变，恢复 decision=blocked/restore_ready=false，不清理源记录。
