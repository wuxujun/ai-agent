# Task/Trace 容量与分页性能基线

后续更新：完整 Trace 保存的重复准备开销已优化，本机性能与故障回滚记录见 [保存优化](完整Trace保存优化与事务回归_2026-10-12.md)。下文保留本批当时数据与边界。

第二批更新：SSE 轻量快照已实现并完成 Memory/SQLite 与专用 Redis 验证，最新实测见 [轻量快照记录](SSE轻量快照读取与一致性验收_2026-10-12.md)。下文为第一批基线，完整读取的数据保留供比较。

更新日期：2026-10-12。F10 第一批建立离线合成基线，并修复三个已测热点。未使用真实任务数据、在线模型、Redis/PostgreSQL 实例或运行服务，未提交/部署。长期保留、备份、磁盘配额和目标环境容量仍需后续工作。

## 测量范围与复跑

本机 Apple M4 Max、darwin/arm64、Go 1.26.8、GOMAXPROCS=16。读操作串行执行，不是 16 个并发客户端。Trace 夹具为 100/1000/10000 条，每条约 1 KiB 独立 observation、一个 evidence 和固定 token usage；任务摘要为 100/10000 个任务，页大小 20。Trace 首/尾页各 100 条；“offset_equivalent” 只是游标位置名称，实际查询没有 OFFSET。

每个读取/事件总线场景固定 100 次、重复两轮；完整快照保存每场景 20 次、重复两轮。初始化数据库、填充数据和样本数组不计入测量。`ns/op` 包含调用、返回值检查与单次采样开销；P95 为每轮的 nearest-rank，样本较少，仅作本机诊断，不是生产 SLO 或统计置信区间。缓存/GC/负载会影响结果，尤其是亚微秒数据。完整快照保存使用固定长度且未改 Trace 的快照，不能等同于逐步增长/持续并发写入。

```bash
go test ./internal/store -run '^$' \
  -bench 'BenchmarkTask(Read|Summary)Capacity' -benchmem -benchtime=100x -count=2
go test ./internal/store -run '^$' \
  -bench '^BenchmarkTaskSnapshotWriteCapacity$' -benchmem -benchtime=20x -count=2
go test ./internal/api -run '^$' \
  -bench '^BenchmarkEventBusFanoutCapacity$' -benchmem -benchtime=100x -count=2
```

源码入口：[读取/写入基准](../internal/store/task_capacity_benchmark_test.go)、[本地事件总线基准](../internal/api/sse_capacity_benchmark_test.go)。夹具仅存于 `b.TempDir()`，普通 `go test ./...` 不运行基准、不接触外部服务。

原始数据已保留在仓库：[优化前读取](benchmarks/task-capacity-2026-10-12/before.log)、[优化后读取](benchmarks/task-capacity-2026-10-12/after.log)、[写入](benchmarks/task-capacity-2026-10-12/write.log)、[本地 fanout](benchmarks/task-capacity-2026-10-12/fanout.log)、[机器可读结果/环境/源码哈希](benchmarks/task-capacity-2026-10-12/results.json) 和 [本批读取补丁](benchmarks/task-capacity-2026-10-12/task_read.diff.patch)。采样时工作区已有其他未提交变更；前后其他运行时代码保持一致，数据不代表一个已发布提交的性能。

## 已测热点与修复

下表平均时间/分配量取两轮结果的算术平均；P95 为两轮范围。

| 场景 | 优化前平均 | 优化后平均 | 每次分配：前 → 后 | P95：前 → 后 |
|---|---:|---:|---:|---|
| Memory，万条 Trace，无 Trace 详情 | 371.6 μs | 0.132 μs | 2,488,156 B → 576 B | 761–801 μs → 167–208 ns |
| Memory，万任务摘要首页，20 条 | 1.645 ms | 0.321 ms | 1,925,839 B → 8,273 B | 2.10–2.20 ms → 0.342–0.348 ms |
| SQLite，万任务游标深页，20 条 | 2.399 ms | 52.5 μs | 16,978 B → 16,872 B | 2.47–2.54 ms → 55.9–63.5 μs |

Memory 原 `GetTaskWithoutTrace` 先深复制完整 Task 再丢弃 Trace；现改为锁内复制元数据，清空 Trace 后再深复制其余可变字段，保留快照隔离。空 metadata 夹具仅一次分配；带其他可变元数据仍按这些字段的大小分配，不能解释成任何 Task 都固定 576 B。

Memory 摘要原分配全任务大小的切片并排序所有命中项；现用有上限的小顶堆保留最新页再排序。临时空间为 O(page limit)，扫描仍为 O(task count)，选取最坏为 O(N log limit)，不等于持久化索引。日期相同时按 ID 降序，租户/session/status 和排他游标语义不变。

SQLite 深页原 OR 谓词让索引只按租户前缀读取再过滤游标；改为 `(created_at, id) < (?, ?)`，直接使用现有复合索引的范围，不新增索引/迁移。实际 EXPLAIN 显示：

```text
SEARCH tasks USING INDEX idx_tasks_tenant_created (tenant_id=? AND (created_at,id)<(?,?))
SEARCH tasks USING INDEX idx_tasks_tenant_status_created (tenant_id=? AND status=? AND (created_at,id)<(?,?))
```

SQL 构建提取为一个函数，使回归检查分析实际读取的查询。PostgreSQL 原有 OR 谓词及占位符保持不变，本次没有宣称 PostgreSQL 的实测优化。SQLite row-value 比较及游标窗口语义参考 [官方文档](https://www.sqlite.org/rowvalue.html#scrolling_window_queries)。HTTP 请求、响应及游标编码不变。

## 仍待优化的容量边界

SQLite 的万条 Trace 夹具 JSON 约 12.26 MB，测量时 DB+WAL+SHM 合计 28,291,840 B；这不是备份大小或保留期容量估算，WAL checkpoint 与页复用会改变占用。当前测量未覆盖磁盘耗尽、文件增长上限、归档/恢复或数据删除策略。

万条 Trace 的 SQLite 完整读取仍平均约 **19.47 ms、38.33 MB 分配/次**，P95 20.18–20.43 ms；同任务尾页 100 条平均约 **0.222 ms、341 KB 分配/次**，P95 0.235–0.250 ms。页读取成本受页长控制，完整读取受 Trace 总长控制。这里的 B/op 是分配量，不是峰值 RSS。

当前 SSE 每个订阅每 15 秒仍完整读取 Task/Trace 并累计 usage。仅据上述分配量推算，100 个同时观察该长任务的订阅会产生约 `38.33 MB × 100 / 15 ≈ 256 MB/s` 的平均分配压力；这是计算推断，没有实际运行 100 个 HTTP 订阅，不能当作最大安全并发数。下一步应优先提供不读取 Trace 明细的权威快照/usage 汇总，保留终态确认、Trace 截断/替换一致性及重连授权。

SQLite 未改 Trace 的完整保存仍遍历全部 Trace。平均时间约：100 条 3.17 ms、1000 条 29.49 ms、10000 条 **306.35 ms**；万条 P95 307.8–314.4 ms，约 13.68 MB 分配/次。后续可测预编译语句复用与增量写入，但必须保留事务回滚、recorded_at、逻辑 Step 与物理 sequence、重排/截断及租约语义，不能直接只追加尾部。

EventBus 的 1000 订阅本地 publish+逐个排空平均约 29.9 μs/次；全部缓冲已满时丢事件约 3.38 μs/次，均为 0 B/op。饱和更快是因为丢事件，不能说明传送能力提高；也不包含 HTTP 序列化、网络、存储轮询或并发订阅争用。订阅通道保持 32 条上限，最终状态仍依赖 [SSE 持久化对账](SSE断线恢复与持久化对账_2026-10-12.md)。

Redis/PostgreSQL、目标 Linux 机器、真实业务数据和持续并发/写读竞争未测。Session 过滤的索引选择、answer audit/memory 增长和长页响应量也不属于本批结果。禁止将本机 SQLite 结果推导为双节点或生产容量结论。

## 回归门禁与结果

新增回归限制万元数据夹具的分配数（不允许复制 Trace），验证可变字段不共享、取消传播，以及 300 个同时间戳任务下的租户/session/status、页上限、游标和全页覆盖。SQLite 深页查询计划需有游标日期范围且不需要临时排序；已有 Memory/SQLite Trace/摘要契约回归继续通过。

CI 采用确定性行为/分配/查询计划门禁，不加入本机 μs 阈值；数据库/编译器升级需核对等价执行计划，时间基线在相同环境和夹具下重跑。正常 CI 全仓/竞态工作流自动包含新增测试，基准按需显式执行。

本批全仓 51 个含测试包通过；store/api 两包竞态、定向 vet、差异与文档链接检查通过。新增基准共 32 个读取/分页场景、6 个保存场景、6 个 fanout 场景，每个两轮。原始全仓/竞态/定向测试日志为本机 `/private/tmp/ai-agent-capacity-{full,race}-20261012.jsonl` 和 `ai-agent-capacity-target-20261012.log`，不提交。未重复浏览器、容器、真实身份或 HA 验收，未部署。

F10 第一批基线与局部优化完成。下一步是 SSE 权威快照轻量读取，其后优化完整 Trace 保存，再根据目标环境数据制定保留、归档、备份和磁盘预算。F05 在线身份验收继续待输入，HA/DAG 继续按用户要求延后。
