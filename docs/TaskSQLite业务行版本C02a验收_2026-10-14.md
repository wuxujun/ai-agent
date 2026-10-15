# SQLite 业务行版本 C02a 验收

提交状态（2026-10-14）：本文与 C02a/b/c、C03a/b1/b2/b3 及 C03b4 首批 `/run` 实现一并纳入本批本地提交，准确提交号以 Git 记录为准。下文保留各阶段实现和验证边界；当前整体停点与最终验证见[会话续接文档](会话续接与完成进度_2026-10-13.md)。

2026-10-14，基线 `86bd534`。本轮实现与文档纳入本批本地提交，准确提交号以 Git 记录为准。C02 分为 C02a 物理业务行变更记录与 C02b 完整会话/导出历史基准；本文只记录 C02a，不宣称 C02 或真实权威已完成。


后续已完成 [C02b 内容版本与原子导出基准](TaskSQLite内容版本与导出基准C02b验收_2026-10-14.md)，本批实现升级 coverage v2，事件含前后内容摘要、超大行在写入阶段拒绝。下文记录 C02a v1 阶段的实现和验证边界；最终 v2 检查见 C02b，不把旧结果当成新代码验收。
## 实现与入口

新增 `internal/store/sqlite_restore_versions.go`，普通 `NewSQLiteStore` 不新增表或触发器，server/config/CLI 不自动启用。

`NewSQLiteStoreWithRestoreVersions(path, namespaceSHA256)` 只能创建全新专用私有文件：绝对规范路径、无父路径符号链接、私有目录/0600 文件、已有主文件或 SQLite sidecar 拒绝。失败保留产物，不做自动清理；不得用业务库尝试此原型。专用文件使用 DELETE journal，默认 Store 的 WAL 设置不变。没有原型可写重开/线上迁移入口。

固定 coverage 为 `sqlite-core-row-versions-prototype-v1`。安装 tasks、sessions、traces、task_leases 四表的 INSERT/UPDATE/DELETE 触发器共 12 个；每次实际物理行变更在业务事务中同时更新全局 epoch、事件和资源版本。触发器在 SQLite 内执行，直接 SQL 的变更也被记录，无需逐个业务函数重复实现 hooks。

- 资源以 `(kind, physical row ID)` 标识。任务/会话用 id，Trace 用物理 id，租约用 task_id；不是租户作用域的权威恢复资源。
- 创建记录 incarnation=本次 epoch，删除保留 tombstone，重建给新的 incarnation；更新保留 incarnation。原型拒绝主键改写。
- UPDATE 比较四表全部列；无变化 UPDATE 不增加代次，所以租约守卫 `owner=owner` 不产生假租约变化。
- A→B→A 即使当前规范化内容相同，仍有两次事件。代次表示物理事件数，不表示 SaveTask 次数。
- epoch 最多 8192、资源（含 tombstone）最多 4096；达到预算后触发器失败，整条业务 DML 回滚。原型没有截断历史或自动清理接口。

## 原子只读诊断

`ReadSQLiteRestoreVersions(ctx)` 在同一 SQLite 只读事务里核对进程中固定的 origin、namespace、完整 schema fingerprint（含所有触发器），重放全部事件并比对资源版本，读取四表规范化内容摘要且检查存活版本与物理行一一对应。

事件、资源、schema、核心行内容均有预算：8192 事件、4096 资源；单表最多 4096 行，总原始值大小 8 MiB；schema 最多 256 对象/1 MiB，主文件最多 64 MiB。SQL 先检查内容/schema 长度，再读取值；输出只有版本、事件元数据与摘要，不输出目标、Trace 内容、会话标题或租约 owner。读取有 30 秒上限，连接竞争遵循调用方取消。

返回固定 `status=unverified` 和八项缺口：physical_rows_only、historical_business_contents_unavailable、session_dependencies_not_bound、execution_isolation_unverified、independent_trust_floor_unavailable、other_store_domains_untracked、lease_expiry_not_materialized、writer_permissions_unverified。

`OpenSQLiteRestoreVersionReader(path, pin)` 是独立只读重开入口：mode=ro/query_only，不运行 Store 初始化或迁移；要求调用方传入 origin/namespace/schema、最小 epoch 和截至该 epoch 的事件元数据摘要。拒绝较旧副本、不同事件元数据前缀、已有 journal/WAL/shm；不会恢复 hot journal，也不修改或清理 sidecar。崩溃恢复测试由普通 SQLite 可写连接对专用夹具执行恢复，再调用只读入口。

该 pin 是调用方保存的本地输入，不是独立 authenticated trust floor。事件只包含行身份与操作，尚未绑定历史业务内容；不同业务内容但相同事件身份/操作序列不能靠此事件摘要区分。持有 SQLite 管理权限的写者仍能重写完整数据/历史、移除后恢复相同触发器或修改运行 PRAGMA；原型不能认证这类操作。schema/重放检查是当前完整性诊断，不是抗恶意篡改证明。

## 验收覆盖

全部使用专用 `t.TempDir`，无在线库、生产密钥、外部模型调用或源记录清理。

- Create/UpdateSession、NextSessionTaskSequence、SaveTask、SaveFullTask、CreateTask、ReplaceTraces、状态迁移、获取/续约/释放租约、DeleteTask/DeleteAllTasks 的物理变更记录。
- 获取租约失败不变、租约守卫无变化 UPDATE 不变、删除/重建 ABA、直接 SQL 内容 ABA。
- 业务事务回滚和取消不留下业务行或版本，版本溢出失败回滚业务行；4096 资源上限拒绝第 4097 个资源且前后观察一致。
- 并发读者只看见完整的双 UPDATE 提交，没有业务内容与事件的半提交。
- schema/触发器变化、丢失事件、版本篡改、身份变化、主键改写、超大内容均拒绝；POSIX 文件/目录权限变化拒绝。
- query_only 下反复观察，SQLite total_changes 与主文件字节不变；关闭后只读重开一致、不能写入、不创建 sidecar、旧 epoch/错误前缀拒绝，残留 journal 保留。
- 专用子进程在同一业务事务提交前/后强制退出（exit 23），恢复后业务行与版本分别同时缺失/同时保留。
- 普通构造器不安装版本表，读取返回 unavailable；拒绝接管已有文件或 `:memory:` DSN。

验证命令及日志：

```bash
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test ./internal/store -run '^TestSQLiteRestoreVersions' -count=1 -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -p 1 ./... -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -race -p 1 ./internal/store ./internal/authorityledger ./cmd/task-restore -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go vet ./internal/store ./internal/authorityledger ./cmd/task-restore
git diff --check
```

最终定向、串行全仓、Store/authorityledger/CLI 竞态、专项 vet、相对链接与差异格式检查均 PASS。首轮私有目录夹具权限检查失败，修正专用夹具权限后通过；复核补充读取预算后已按最终代码重跑全仓/竞态/vet，不把早期结果代替最终检查。日志 `/private/tmp/ai-agent-c02a-target-20261014.log`、`/private/tmp/ai-agent-c02a-full-20261014.log`、`/private/tmp/ai-agent-c02a-race-20261014.log`。全仓 httptest 本机监听需要按权限流程在沙箱外运行。

## 后续 C02b

仍需完整租户作用域 Task+Trace/会话成员与高水位汇总、同一导出事务的历史 baseline、业务内容与版本的历史绑定、unknown writer/完整 coverage 的启动拒绝契约，以及与独立 C01/C04 floor/outbox 的衔接。其他 Store 域（approvals/effects、memory、tenant usage 等）没有被此 coverage 包含。自然租约到期没有写事务，不能作为已物化代次或执行器停止。

C03 的 durable execution gate/attempt/退出 flush/工具清理回执、C04 的真实 signer/trust、其他强制恢复证据仍待实现。默认配置不切换，HA/DAG 延后，恢复继续 `decision=blocked`、`restore_ready=false`；不连接在线库或执行恢复。
