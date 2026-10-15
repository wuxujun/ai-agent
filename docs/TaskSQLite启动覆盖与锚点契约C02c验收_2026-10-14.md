# SQLite 启动覆盖与锚点契约 C02c 验收

提交状态（2026-10-14）：本文与 C02a/b/c、C03a/b1/b2/b3 及 C03b4 首批 `/run` 实现一并纳入本批本地提交，准确提交号以 Git 记录为准。下文保留各阶段实现和验证边界；当前整体停点与最终验证见[会话续接文档](会话续接与完成进度_2026-10-13.md)。

2026-10-14，基线 `86bd534`。C02a/C02b/C02c 已纳入本批本地提交。C02c 完成本地原型的显式只读启动核验及外部锚点接入契约，不实现真实写者权限、运行时 fencing 或独立权威服务；整个 C02 不能因此宣称真实部署验收完成。


后续 [C03a 单任务持久化 gate/attempt/lease 原型](Task持久化Gate与Attempt原型C03a验收_2026-10-14.md) 已实现并纳入本批本地提交，实际运行入口尚未接入，下一步 C03b。
## 显式只读启动核验

新增 `internal/store/sqlite_restore_startup.go`：

- `SQLiteStore.ReadSQLiteRestoreStartup(ctx, request)` 仅在显式创建的核心内容版本原型上可用；普通 Store 返回 unavailable。
- `SQLiteRestoreVersionReader.ReadSQLiteRestoreStartup(ctx, authorityIdentitySHA256, reader)` 支持关闭后的只读重开，始终使用重开时固定的历史 pin，不替换 pin，不变成可写 Store。
- 不调用 Store 初始化/迁移，不重置 PRAGMA，不补造或写入锚点，不增加 server/config/CLI/HTTP 入口。

startup manifest 包含固定 coverage、四表/kind/key、内容函数版本、写者策略声明、journal/synchronous/trusted_schema/recursive_triggers 预期以及事件/资源/内容预算。预期 profile 来自代码，不接受源数据自行宣称完整 coverage。

同一只读数据库事务中检查：

1. `journal_mode=delete`、`synchronous=FULL (2)`、`trusted_schema=1`、`recursive_triggers=0`。这是当前专用原型 profile，未修改普通运行设置；自定义 SQLite 函数当前使用 trusted_schema=1，不能将此检查等同生产 schema 管理权限隔离。
2. 固定输入向量与编译时 SHA-256 预期值一致，证明实际内容函数符合本版本实现，而不是只检查函数名存在。
3. `restore_row_*` 恰好有 12 个 trigger，每个预期表/操作对应的 SQL 等于代码生成的记录触发器。即使调用方给出新的 schema 自填 pin，也不能用缺失记录触发器满足 startup coverage。
4. 复用 C02b 的 schema/origin/namespace、完整历史重放、当前内容与资源版本核对；核对外部传入 pin 的 schema、最低 epoch 和已保留内容历史前缀。
5. 私有文件/目录身份与取消/预算检查延续 C02a/C02b；不通过 UPDATE-based lease guard 获取写锁。

本轮还为 digest/namespace 的 hex 解码补了固定 64 字符长度前置检查，避免超大 pin 输入在验证前触发大分配。

## 锚点与写者策略接入契约

`SQLiteRestoreStartupAnchorReader` 只能由可信调用代码显式注入，不从恢复包选择或自动加载任何环境/秘密。没有默认实现或真实控制面客户端。

请求 scope 固定 origin、namespace、schema、coverage、startup manifest 和调用方独立选择的 authority identity 摘要。响应须绑定同一 scope，以及当前数据库 epoch、内容事件历史摘要、当前数据摘要、锚点 generation、writer identity、`sqlite-private-single-writer-v1` 写者策略和 active 状态。

检查如下：

- 锚点必须确认当前 head，而非只满足大于旧基准。锚点滞后意味着当前提交未确认；锚点领先意味着该源可能回滚。两者均拒绝。
- generation/writer identity/head 摘要格式严格，状态必须 active；撤销、未知策略、错误 authority/manifest/schema 或 scope 拒绝。
- IssuedAt/ExpiresAt 为 UTC，生效且未到期，期限最多五分钟。
- 锚点读取两次，中间核验数据库，随后再次读取本地 startup snapshot；前后锚点字段不同或数据库 epoch/历史/内容改变即拒绝，再复核有效期。
- 提供者失败只返回脱敏错误；遵循调用方取消和 30 秒上限，提供者必须遵守传入 context。

无 reader 时返回本地 unverified 诊断且 `anchor_contract_matched=false`，缺口中有 startup_anchor_unavailable。合法 fake anchor 返回 `anchor_contract_matched=true`，含义仅是本次接入协议匹配；`status` 仍固定 unverified，保留 authenticated_anchor_transport_unavailable、runtime_writer_fencing_unimplemented、other_store_domains_untracked、execution_isolation_unverified 四项缺口。

**报告不是写者准入凭证。** `WriterPolicy`/身份摘要是尚待外部实现验证的契约字段，不是本机文件权限推导出的权威事实。当前 startup read 不阻止其结束后继续发生写入，末次读取后仍存在变化窗口；没有以读取报告授权未来业务提交、签署 stopped 证据或升级 restore_ready。独立服务仍须实际认证权限、持久化不可回退 floor、证明完整实例/写者集合和生命周期；本地 pin 或 fake 响应不能替代它。

## 验收

专用 temp fixture/fake provider，没有在线数据库、生产密钥或外部模型调用。

- query_only 下核验成功，total_changes、业务/历史观察与数据库字节不变；只读重开核验一致。
- 普通构造器不提供 startup 原型；无外部锚点不冒充匹配。
- 错误 origin/namespace/schema/coverage/manifest/authority、floor 滞后/领先、错误 head、generation/身份/策略、撤销、未来/过期/超长/空时间窗均拒绝。
- 双读间 generation、writer、撤销、有效期或本地业务变化均拒绝。
- 不安全 PRAGMA、缺失 trigger 拒绝；手动把 schema self-pin 改为缺少 trigger 的当前 schema，仍不能绕过代码预期的 coverage。
- 错误历史前缀、旧 source epoch、超大 pin、提供者错误和取消均拒绝，错误不包含上游私密正文。
- C02a/C02b 全部既有回归继续执行，包括内容分叉、ABA、资源上限、崩溃提交前后和原子会话导出。

```bash
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test ./internal/store -run '^TestSQLiteRestore(Versions|Content|Baseline|Startup)' -count=1 -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -p 1 ./... -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -race -p 1 ./internal/store ./internal/authorityledger ./cmd/task-restore -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go vet ./internal/store ./internal/authorityledger ./cmd/task-restore
git diff --check
```

最终定向、串行全仓、Store/authorityledger/CLI 竞态、专项 vet、相对链接与差异格式检查均 PASS。日志 `/private/tmp/ai-agent-c02c-target-20261014.log`、`/private/tmp/ai-agent-c02c-full-20261014.log`、`/private/tmp/ai-agent-c02c-race-20261014.log`。全仓 httptest 按权限流程在沙箱外运行。

## 后续

C02 的本地原型已覆盖物理 DML+内容历史、同事务会话/导出基准及只读 startup 协议；真实保护写者、可写重启准入/fencing、独立 floor/outbox/权限客户端仍未接入。下一步可独立推进 **C03 专用持久化 dispatch gate/attempt 协议原型**，先做同事务禁派发/租约竞争与 attempt 生命周期回归，再审查 HTTP、Engine/Coordinator、启动/审批恢复、最后 flush 与工具清理各入口；不能先发布完整 stopped 结论。

真实权威 NOT_RUN、恢复 blocked/restore_ready=false、HA/DAG 延后，默认运行配置不变，不操作在线租约/执行恢复/清理源记录。
