# Task/Trace 保留与归档方案

后续更新：显式 [联合范围导出](Task联合快照导出_2026-10-12.md) 已补审批密文/版本、任务所属记忆/embedding 和 session/高水位；[持久化隔离恢复演练](Task持久化隔离恢复验收_2026-10-12.md) 已补。本文原设计基线保留，在线恢复及源记录移除仍未实现。
后续更新：第一版 [只读候选评估接口](只读归档候选评估接口_2026-10-12.md) 已实现并通过本机回归及专用 Redis 只读验收。可信终态时间/维护版本已补，见 [生命周期契约](可信终态时间与维护版本_2026-10-12.md)。归档执行、自动清理和联合内容版本仍未实现；下文保留原设计基线。

单任务导出更新：[稳定 Task/Trace 导出](稳定TaskTrace单任务导出_2026-10-12.md) 已实现，源记录保留。v1 包不包含独立审批/记忆/session；联合 v2 包及隔离演练已补，restore_ready=false，完整在线恢复与归档执行仍待补。

日期：2026-10-12。状态：**可审核设计，扫描器/归档执行/自动清理尚未实现**。本批未改配置或数据，也未运行备份、VACUUM、归档上传、任务删除或文件清理。方案承接 [容量基线](TaskTrace容量与分页性能基线_2026-10-12.md)、[保存优化](完整Trace保存优化与事务回归_2026-10-12.md) 和 [并发基线](单机TaskTrace并发读写压力基线_2026-10-12.md)。

## 策略输入与阶段

先交付只读候选评估，输出 eligible/blocked/unknown 及原因；其后完成单任务导出和隔离恢复演练，再考虑在线迁移/源记录移除。后续执行必须使用明确 Task ID、租户范围、版本/内容 hash 和可审核产物，不能以年龄条件直接调用 DeleteAllTasks。

保留期限和磁盘预算尚无业务输入，以下字段只是设计输入，不是当前可用的 config.yaml 配置：

| 输入 | 作用 | 未提供时的行为 |
|---|---|---|
| tenant_allowlist | 评估/归档授权租户范围 | 拒绝执行 |
| hot_days | 终态可靠年龄超过多久进入归档候选 | 标记 unknown，不能作迁移决策 |
| archive_days | 归档材料保留期限 | 保留现状，禁止到期移除 |
| purge_grace_days | 归档验收后源记录再保留多久 | 禁止移除源记录 |
| max_task_bytes / scan_page_limit / byte_budget | 限制单任务和每轮扫描/导出成本 | 使用有界只读评估，超限 blocked |
| export_root / backup_policy | 受控产物位置与恢复保障 | 可以评估候选，不能归档/移除 |
| hold_ids / hold_sessions / hold_reason | 业务保留、复核或未完成工作 | 任何 hold 优先阻止移除 |

现有 `approval.retention_days` 和 `log.retention_days` 不代表 Task/Trace 的期限，不能复用来清理任务。缺失参数不是授权；设计中不设置自动清理的默认开启值。

## 候选与一致性规则

默认只把 completed/failed 且可靠年龄超过 hot_days 的任务列为可归档候选。created/running/awaiting_approval/paused 均 blocked；partial 先 blocked，因为 multiagent 的待 verifier/team-config checkpoint 可能恢复运行。当前 [CanResumeTask](../internal/orchestrator/multiagent.go) 已表明 terminal status 不等于永远不可恢复。

以下情况也须 blocked/unknown：活动或无法验证的执行租约、pending 审批或决策/消费尚未闭合、重新审计/继续执行中的状态、hold、所属 session 仍要求连续读取、读取失败、内容超限、租户授权缺失或源数据在评估过程中变化。

原设计基线没有统一的 `terminal_since` / `last_activity_at` 持久化字段；部分 status-only 迁移不更新 Task.updated_at。created_at/updated_at/Trace recorded_at 均不能单独当作可靠终态起点，recorded_at 是物理位置首次写入时间，也不是最后修改时间。第一版只读评估应把年龄来源和置信程度列出；没有可靠证据的历史任务标记 `unknown_age`，不自动回填时间。后续已增加 terminal_since / last_maintenance_at / maintenance_version 并覆盖内置存储保存、状态及 SQL Trace 替换；联合快照/哈希仍待补。

候选清单记录最小元数据：tenant/task/session ID、status、termination_kind、可靠年龄及来源、Trace count、usage、审批闭合状态、lease/hold/checkpoint 原因、估计字节量、观察时间、策略版本和内容 hash。默认不写 goal、答案、Trace 正文或审批 payload 到报告/日志。清单是观察结果，不是删除命令。

归档导出须基于一个稳定观察：SQL 使用一致性只读事务/经过校验的数据库快照；Memory 在锁内取得快照后在锁外编码；Redis 要在同一观察中取得主 JSON、首次 Trace 时间和关联审批的版本，不能拼接独立 GET 的不同保存版本。长任务导出有大小/时间限制。单次 Task 快照一致不代表 Task、approvals、memories、session 已形成同一联合快照，因此执行前需新增联合导出/版本检查契约。

在源记录移除前，必须重新核对授权、status、租约、审批、hold、session 依赖与清单版本/hash；变化、缺失或无法验证均停止该任务处理。当前 Store 没有完整的 archive/purge CAS 接口，不能仅复用一次 DeleteTask 判断。

## 归档集合与恢复

每个任务独立包，先写临时产物，计算 hash/字节量和 schema 版本，完成校验后发布 manifest。文件名使用工具生成的受控标识，不接受 Task ID 拼接路径；目录和恢复目标保持租户隔离，拒绝覆盖已有产物。初期导出不删除源记录，不自动上传外部服务。

| 数据 | 必须保留/核对的内容 |
|---|---|
| Task | 原 ID、租户、个人创建者、session/sequence、team/Brain 固定摘要、配额/用量、终态/答案/错误与原时间 |
| Trace | 物理 sequence、可重复逻辑 Step、全文/evidence、usage、occurred_at/duration_ms、原 recorded_at（允许历史 null） |
| 审批 | 记录 ID、tenant/task、版本、状态、owner/lease、时间和已存加密 action/resolution payload；不导出加密密钥 |
| Task-owned memories | 来源与关联 ID、embedding/内容及索引恢复要求；独立来源记忆的保留策略另列 |
| 审计与 session 依赖 | answer_audit、决策闭合证据、必要 session 元数据/sequence 高水位及关联关系；任务包不自动归档整个 session |
| Manifest | 格式版本、策略版本、源 backend/应用版本、导出时刻、最小 ID 关联、数量/usage、每个文件 hash/长度、验证结果和保留到期策略 |

原始包可能含私有代码、答案、个人身份和审批细节，采用受控存储、访问审计和独立保管的加密配置；报告只显示最小 metadata/hash，不传播正文或密钥。归档读取仍走租户授权，不能以本地文件路径或公开下载链接绕过 API 权限。

恢复先在独立测试目录/数据库核对 hash、数量、usage、逻辑/物理顺序、原时间、租户与审批状态。正常 CreateTask/SaveFullTask 会规范化时间、重建 recorded_at，并可能为 completed 任务触发异步 memory indexing；因此恢复工具必须使用专门导入事务/模式，保留原时间且不运行任务、不调用模型、不重新执行审批或生成新记忆。重建读模型索引后，再用 API summary/Trace/approvals 和轻量 snapshot 对账。

源归档清单不等于恢复成功。清理候选至少需要“导出校验通过 + 隔离恢复通过 + grace 满足 + 重新核对未变化”的独立证据；恢复失败、损坏、配额超限或密钥不可用必须保留源数据。

## 现有删除边界与备份

当前 DeleteTask 会处理 Trace、Task-owned memory 和 lease；SQL schema 还声明 approvals 的 ON DELETE CASCADE，而 SQLite 实际 foreign-key 设置取决于连接。Redis 的删除脚本处理一套任务/索引/memory key，未提供跨后端一致的归档审批清单。这些方法是管理删除，不能视为完整归档 API，也不能假定所有后端都有相同的关联删除效果。新执行器须明确独立审批保留、session 依赖、异步索引和缓存对账行为。

删除记录不会自动保证 SQLite 文件立即缩小；空闲页复用和压缩是不同阶段。备份应采用受支持的一致性方法：SQLite [Online Backup / VACUUM INTO](https://www.sqlite.org/backup.html)，空间维护要求参考 [VACUUM](https://www.sqlite.org/lang_vacuum.html)；PostgreSQL 的一致性导出参考 [pg_dump](https://www.postgresql.org/docs/current/app-pgdump.html)。本批未执行这些操作，也未给运行中数据库做文件复制或改变 WAL/checkpoint 策略。目标环境的恢复时间和空间预留应通过专门演练测量。

按本批合成数据，仅逻辑序列化量可用 `任务/日 × 平均任务字节 × 热保留日数` 粗估。例：1000 任务/日、每任务 1000 条 Trace（约 1.225 MB JSON）、30 天约 36.75 GB 逻辑数据；这些是假设，不是实际业务输入。数据库页/WAL、索引、审批/记忆、备份/归档双份和恢复临时空间另计，不能将一个夹具的 DB/JSON 比例当作生产放大系数。

文件操作遵守仓库 AGENTS.md：不批量删除文件或目录；若将来需要清理多个产物，由用户手动处理，工具不能执行递归删除。数据库源记录移除也应是明确单任务、带版本前置条件的独立受控动作，不能在只读评估或导出路径中隐式发生。

## 实施与验收顺序

1. 实现只读候选评估与 reason codes，按租户分页，未知年龄/租约/审批/恢复状态 fail closed；以合成数据覆盖边界、取消、权限和扫描预算。
2. 明确终态时间/内容版本及联合快照契约，补跨后端迁移与事务/CAS 测试；先实现单任务导出，不移除源记录。
3. 实现隔离恢复和清单校验，证明原时间/usage/审批/session 关系保留且不产生付费调用或重新执行。
4. 在期限、预算和目标位置明确后审阅执行方案，后续才增加单任务移除、过期产物处理与维护操作；真实审批/归档前不得把本设计当作授权。

上述步骤为原实施顺序。当前只读评估、生命周期契约、联合导出和临时/持久化隔离演练已有实现；实际验证与剩余范围见 [续作交接](Task恢复验证续作交接_2026-10-12.md)。下一次先补专用 PostgreSQL，再补大事务异常退出/热日志/损坏验证和完整恢复准入条件。restore_ready=false、源记录保留，不能标记“自动保留/归档已实现”。F05 在线身份待输入，HA/DAG 继续延后。
