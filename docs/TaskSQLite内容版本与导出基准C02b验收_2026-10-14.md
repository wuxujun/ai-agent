# SQLite 内容版本与导出基准 C02b 验收

提交状态（2026-10-14）：本文与 C02a/b/c、C03a/b1/b2/b3 及 C03b4 首批 `/run` 实现一并纳入本批本地提交，准确提交号以 Git 记录为准。下文保留各阶段实现和验证边界；当前整体停点与最终验证见[会话续接文档](会话续接与完成进度_2026-10-13.md)。

2026-10-14，基线 `86bd534`；C02a/C02b 改动已纳入本批本地提交。本轮完成本地原型的历史内容摘要绑定和原子导出基准。C02 整体还缺启动 coverage、受保护写者/独立历史锚点接入，不进入真实 verified，不宣称在线恢复完成。


后续 [C02c 启动核验与锚点接入契约](TaskSQLite启动覆盖与锚点契约C02c验收_2026-10-14.md) 已实现并纳入本批本地提交。真实写者权限/fencing/独立服务仍未接入，下一步可推进 C03 专用 gate/attempt 原型。
## 内容版本 v2

在 [C02a 原型](TaskSQLite业务行版本C02a验收_2026-10-14.md) 上，把 coverage 升级为 `sqlite-core-content-versions-prototype-v2`。仅创建全新专用私有文件，旧 v1 原型没有迁移/接管入口，普通 Store/server/config/CLI 不启用。

四表触发器在业务事务内记录每次变化前后的 `before_sha256`、`after_sha256`；资源版本保存当前 `content_sha256`，删除后的内容摘要为空。创建、更新、删除、重建和事件重放均检查内容连续性。

摘要输入含表名、列名、SQLite 存储类型和内容：text/blob 使用原始字节的 hex；整数保留存储值；浮点采用 SQLite `%!.26g`，避免 CAST-to-text 舍入后相邻 double 混淆；时间按实际存储字节而非 Go 驱动解析时间绑定。固定 SHA-256 域为 `ai-agent/sqlite-core-row/v2`。

`internal/store/sqlite_restore_content.go` 在 package init 注册一个带版本名称的纯确定性 SQLite 函数。modernc 的函数注册对随后建立的连接生效，因此不在运行中懒注册。普通连接可见该函数，但普通 Store 不调用、不安装版本表/触发器；没有网络、文件或运行配置动作。函数输入有预算，触发器计算前先检查行字节数，单行超过 8 MiB 的写入整条拒绝。

更新/删除要求 OLD 摘要等于上一个资源内容摘要；只读诊断还要求实际当前行摘要等于重放后的版本。移除触发器、修改内容、恢复完全相同 trigger SQL 后，schema fingerprint 虽然恢复，内容核对仍拒绝；后续正常写入也不能自动承认脏 predecessor。

事件摘要现在包含内容摘要，因此相同行身份/操作序列、相同当前内容但不同历史中间内容，会有不同历史前缀；调用方保留的 pin 可以拒绝这类历史分叉。旧 v1 的仅身份/操作事件不能提供此结论。

仍不能以本地内容哈希认证持有数据库管理权限的写者：完整历史/版本/数据一起重写、未记录变化再回到原值、运行 PRAGMA 等管理行为，需要受保护写者与独立 floor/signer 契约。没有保留每次历史 payload 原文，不能从摘要重建任意历史业务数据。

## 同事务导出基准

新增 `ReadSQLiteRestoreBaseline(ctx, TaskRestoreLocalObservationRequest)`，只在显式原型 Store 上可用。租户匹配先检查，随后在同一只读事务中：

1. 校验完整 schema、内容版本历史和当前四表行。
2. 复用 S02 原子观察，读取完整当前会话成员、高水位、Task/物理 Trace 摘要和租约诊断；无 session 时只观察根任务。
3. 读取根任务联合导出（含原有审批、task-owned memory、会话信息），按相同 JSON 编码检查根 Task/Trace/Session 摘要与会话观察一致。
4. 汇总当前会话所属 Task、Trace、lease 和 session 的行版本，返回全局 origin/namespace/schema/epoch/内容历史前缀 pin 和整体 baseline 摘要。

返回 `SQLiteRestoreBaseline` 固定 unverified，只有本会话所属版本，没有其他租户的任务 ID；全局内容摘要/epoch 是整个专用库的诊断锚点，其他会话的核心写入也会使它变化，不是租户专属权威版本。读取遵循请求字节/任务数预算、30 秒时限及取消；总输出超预算拒绝，不截断成员。没有持久化 baseline 写入或修改 export v2 包格式，也没有新增 CLI/HTTP 准入提供者。

`Snapshot` 是真实导出数据，调用方应按原有导出边界保护；不在日志或测试输出中打印内容。`Session` 保留 S02 standalone unverified/blockers 语义；新增 baseline/本地版本字段在外层绑定，不将 S02 升级成权威观察。审批/effects、memory 等虽沿用联合导出内容，但不在核心版本 coverage 内，不能据此宣称它们具有历史版本证明。

## 新增回归与验证

- 相邻 double 内容摘要不同；业务内容变化进入事件历史摘要。
- 临时移除后恢复相同 trigger SQL 的未记录写入，被当前只读核对及后续写入拒绝。
- 只改历史中间内容摘要，保持连续性、相同事件身份/操作和相同当前内容：本地事件摘要不同，原 pin 只读重开拒绝。
- 三成员完整会话、高于 JavaScript 安全整数的序号高水位、根联合导出摘要一致、其他租户的行身份不出现在 scoped versions。
- query_only 下基准读取一致、取消生效、错误租户拒绝、成员数量预算不足拒绝。
- 并发写事务同时改变任务与会话信息，基准读取不混合 Task/成员/会话快照。
- C02a 的事务回滚、取消、资源/epoch 上限、直接 SQL ABA、删除重建、租约守卫、只读字节不变和提交前/后强制进程退出回归继续执行；超大行改为在触发器写入阶段拒绝且前后数据/版本不变。

```bash
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test ./internal/store -run '^TestSQLiteRestore(Versions|Content|Baseline)' -count=1 -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -p 1 ./... -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -race -p 1 ./internal/store ./internal/authorityledger ./cmd/task-restore -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go vet ./internal/store ./internal/authorityledger ./cmd/task-restore
git diff --check
```

最终定向、串行全仓、Store/authorityledger/CLI 竞态、专项 vet、相对链接与差异格式检查均 PASS。新增历史分叉夹具最初假设 SaveFullTask 只产生一个事件；核对其生命周期 UPDATE 后改为按实际末尾事件定位，最终回归及全仓均通过。日志：`/private/tmp/ai-agent-c02b-target-20261014.log`、`/private/tmp/ai-agent-c02b-full-20261014.log`、`/private/tmp/ai-agent-c02b-race-20261014.log`。全仓 httptest 按权限流程在沙箱外运行，未接入生产库或外部模型。

## 下一步

C02 仍需受保护写者的启动 coverage 拒绝契约、重启写者与未知入口控制、导出 baseline 的独立持久保存/信任锚点及 outbox/floor 衔接。完成这些本地契约后推进 C03 durable execution gate/attempt/退出 flush/工具清理回执；C04 才接真实 signer/trust 环境。自然租约过期未物化，也不能证明执行器停止。真实权威 NOT_RUN，恢复 blocked/restore_ready=false，HA/DAG 延后，不连接在线库或执行恢复。
