# 稳定 Task/Trace 单任务导出

后续更新：显式 [联合范围导出](Task联合快照导出_2026-10-12.md) 已补审批密文/版本、任务所属记忆/embedding 和 session/高水位；[持久化隔离恢复演练](Task持久化隔离恢复验收_2026-10-12.md) 已补。本文 v1 范围保留，不可用于完整恢复；在线恢复及源记录移除仍未实现。
日期：2026-10-12。当前工作区已实现 `task-trace-export-v1`；未提交、未部署，未操作在线数据或写入归档文件。

## 接口与范围

```http
GET /api/retention/tasks/<task-id>/export?tenant_id=<configured-tenant>&byte_budget=1048576
X-API-Key: <administrator-credential>
```

使用既有 API/Console 鉴权和 AdminMiddleware。tenant_id 必填且须为已配置租户，根管理员可明确选择 default。Store 在快照内再次核对任务与租户；错租户/缺失任务返回 404。X-Admin 不能取得管理员身份。没有安装 TaskExportReader 的自定义 Store 返回 501，不能退回普通 GetTask 拼接数据。

byte_budget 默认 1 MiB，范围 4096–8388608；完整响应不能超过预算，最大 10000 条 Trace，超过任何上限返回 413，不截断、不返回部分文件。未知参数、重复单值参数和非法 URL 编码返回 400。快照/编码错误返回脱敏 503，取消/超时返回 504；请求上下文有 15 秒期限。

成功响应是 JSON 附件，固定文件名 task-export.json，Cache-Control: no-store，X-Content-Type-Options: nosniff。接口不在服务端创建文件、不接受路径、不上传、不修复索引、不获取或续期租约、不修改 Task，不调用模型，也不移除源记录。浏览器或客户端可以自行保存响应。

包包含 Task 持久化字段、其中嵌入的 memories/answer_audit，以及完整 Trace events。Task.trace 为空，事件只出现在 payload.traces 中；每个事件保留连续的物理 sequence、可重复的逻辑 Step、evidence/usage/agent、occurred_at/duration_ms 和原 recorded_at。历史没有 recorded_at 时保持缺失，不生成导出时间作为替代。缺失/不连续的物理序号、Redis 时间索引数量不匹配或非法时间导致拒绝导出。

**本格式不是完整恢复或迁移包。** Manifest 明确列出 excluded：独立 durable approvals（含密文/审批版本）、独立 memories/embeddings、session 记录与 sequence 高水位、租约、外部审计、workspace 文件和加密密钥。restore_ready 始终 false，source_preserved 始终 true。它是单次 Task/Trace 观察，即使活动任务也可导出；不能据此判断 retention eligible 或进行删除。各后端沿用原持久化表示，未保证不同后端的 nil/empty 或 embedded memory 表示产生相同字节哈希。

## 清单及哈希

顶层字段：schema、source_backend、exported_at、source_preserved、restore_ready、included、excluded、payload_sha256、payload_bytes、trace_count、payload。payload 包含 task 和 traces；版本、原时间和所有正文均纳入哈希。包会含私有目标、答案、工作区引用和个人创建者字段，使用现有管理员保护和脱敏访问日志；没有开放静态或公共下载链接。

payload_sha256 是 **payload 在响应中的原始紧凑 JSON UTF-8 字节**的 SHA-256，不包含顶层清单/导出时刻；payload_bytes 是这些字节的长度。校验时保留原始 payload 字节，不能先重排 JSON key、重新格式化或把大整数解析成浮点数再计算。相同快照可比较内容哈希；导出时刻不同仍可能有相同 payload 哈希。哈希是完整性校验，不是签名、联合依赖版本或源记录删除 CAS。

预算既包含 payload，也包含清单。读取前做原始字符串/数量或对象复制成本检查，读取后检查对象成本，最后按编码后的响应精确计费；转义膨胀也会拒绝。预算是响应上限，不承诺峰值内存等于该值。

## 快照实现

- Memory 在同一个 RLock 中做预检查、深复制 Task 和 Trace 时间，锁外编码；两个可变时间指针及 evidence 与存储隔离。
- SQLite 在单一只读查询事务中完成租户/大小预检查、Task 元数据与全部 Trace 的读取；复用同一事务的查询器。现有 WAL 模式使读事务保持原观察，见 [SQLite 官方隔离说明](https://www.sqlite.org/isolation.html)。没有修改连接池、WAL 或 checkpoint 配置。
- PostgreSQL 使用只读 Repeatable Read 事务，相同快照覆盖预检查及各读取；其隔离契约见 [PostgreSQL 官方说明](https://www.postgresql.org/docs/current/transaction-iso.html)。实现及显式在线契约测试入口已增加，当前在线验收 **NOT_RUN**。
- Redis 使用 EVALSHA_RO/EVAL_RO，一次观察读取主 JSON 和物理时间列表。先按 STRLEN/数量/单时间长度/原始总字节检查，不修复或迁移索引；缺少历史时间列表时保留未知。Go 解析主 JSON，避免 Lua cjson 重编码造成数组形状和大整数精度变化。

## 验证

原生 Go 1.26.8、临时 SQLite、独立 Redis 7.2.6；不使用付费模型或在线生产数据。

```bash
go test ./...
go test -race ./internal/store ./internal/api ./internal/orchestrator
go vet ./...
AI_AGENT_RUN_EXTERNAL_INTEGRATION=true TEST_REDIS_URL=unix:///private/tmp/ai-agent-export-20261012.sock go test -race ./internal/store -run TestExternalRedisTaskExport -count=1
git diff --check
```

以上通过。回归覆盖：管理员/普通租户/无凭证/伪造 X-Admin、明确租户/参数/预算、失败正文脱敏、异常自定义适配器、物理顺序/重复 Step/原时间/evidence/大整数、SHA-256 和字节数、取消、连续 30 次写入与读取一致性、导出前后源记录不变。SQLite 额外用第二个独立 Store 在读事务中途提交新 Task/Trace，证明导出仍全为原版本；Redis 比较所有应用 key 的 DUMP，证明导出无写入，并拒绝不匹配的时间列表。

联合快照、HTTP 临时隔离导入和离线持久化恢复演练已实现。下一步补 PostgreSQL 在线验收、大事务异常退出/热日志/损坏验证及完整恢复准入条件，见 [续作交接](Task恢复验证续作交接_2026-10-12.md)。恢复不得调用正常 SaveFullTask 伪装导入，也不得触发记忆索引、模型或审批重放；restore_ready=false，自动清理和源记录移除继续未实现。
