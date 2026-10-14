# 单机 Task/Trace 并发读写压力基线

更新日期：2026-10-12。F10 第四批增加可重复的 Memory/SQLite 并发 Store 基准。全部使用临时合成数据，未连接运行服务、真实任务、身份供应商或付费模型，未修改连接池/配置，未提交部署。本批是短时突发测试，不是持续 soak、HTTP/SSE 负载或正式 HA 验收。

## 夹具与指标

[task_contention_benchmark_test.go](../internal/store/task_contention_benchmark_test.go) 覆盖两个后端、1000/10000 条 Trace、1 个写入者和 0/4/16 个读取者，共 12 个场景，每个两轮。

每轮写入者执行 20 次完整保存，每个读取者执行 20 次轻量 snapshot 读取，共同 barrier 起跑，无 15 秒 SSE 周期或 think time。每次写入改变任务 StepCount 与最后一条 Trace 的 total_tokens；每次读均检查 `trace_count`、status 和 usage 必须属于同一保存版本，不能只计延迟而忽略一致性。上下文最多 30 秒，任意失败会取消其他 worker，等待全部结束；未完成操作使基准失败。

运行环境：Apple M4 Max、darwin/arm64、Go 1.26.8、GOMAXPROCS=16；约 1 KiB observation/evidence 的 Trace 夹具。SQLite 沿用 Store 默认单连接；数据库初始化、填充、worker 创建、样本分配和结果排序不计入测量。读取/保存执行和排队均计入其延迟。

- `b.N` 是保存次数，不是全部操作数。默认 `ns/op` / `B/op` 是整轮工作量除以保存次数，包含其他 worker 的成本，不能称为单条 SQL 延迟或单次读取分配量。
- `total-ops/s` = 已完成读写总数 / 整轮 wall time；每轮读写混合比例不同，不能只比较此值推导写吞吐增加。
- `read-p95-ns` 对该轮全部观察者样本取 nearest-rank，`write-p95-ns` 对该轮保存样本计算；每轮仅 20 个保存样本，P95 无统计置信区间。
- `pool-waits` / `pool-wait-ns/op` 为 SQLite `DB.Stats` 的 WaitCount/WaitDuration 增量；后者把各操作的连接等待累计后除以所有读写操作数。等待累计可超过整轮 wall time，不是一个额外请求的时间。

原始 [输出](benchmarks/task-contention-2026-10-12/contention.log) 与 [环境/源码哈希/机器可读结果](benchmarks/task-contention-2026-10-12/results.json) 已保留。采样工作区含此前未提交变更，不代表正式发布提交。

## 万条 Trace 结果

表中为两轮范围，0 读取者没有读取 P95。

| 后端 | 读取者 | 读取 P95 | 保存 P95 | 混合总操作/s |
|---|---:|---:|---:|---:|
| Memory | 0 | — | 0.751–0.912 ms | 2155–2375 |
| Memory | 4 | 0.884–0.908 ms | 0.874–0.897 ms | 10530–10558 |
| Memory | 16 | 1.529–1.794 ms | 0.952–0.965 ms | 29924–30389 |
| SQLite | 0 | — | 49.41–49.76 ms | 20.54–20.73 |
| SQLite | 4 | 122.58–163.61 ms | 90.70–105.94 ms | 74.99–75.87 |
| SQLite | 16 | 291.35–309.94 ms | 196.51–216.85 ms | 144.6–147.4 |

SQLite 的 4/16 读取者场景分别有每轮 96 次、332–336 次连接等待；平均每个读写操作累计连接等待约 40.8–44.9 ms、96.2–97.8 ms。单连接将完整保存和数值聚合排队，争用是本次延迟增长的重要来源。维持现有默认连接池，扩池/独立读连接应在专门的 SQLITE_BUSY、事务和实际负载验证后评估。

此前固定快照约 37 ms 的串行数据不能直接与此表作精确前后加速比较：本批真实修改 token/任务字段，执行突发混合操作，采样条件不同。实际 SSE 每个连接约 15 秒对账，本夹具连续读取更密集，不能按“16 观察者”推导“最多支持 16 SSE 连接”。

没有覆盖多写入者、HTTP 认证/分页/序列化、多个 Task、长时间 WAL/GC/RSS 高水位、磁盘耗尽、外部 Redis/PostgreSQL、生产机器或负载均衡器。Memory 成功不代表持久化容量，SQLite 短时成功不代表部署 SLO。

## 复跑与门禁

```bash
go test ./internal/store -run '^$' \
  -bench '^BenchmarkTaskConcurrentCapacity$' -benchmem -benchtime=20x -count=2
# 竞态仅检验 worker/快照一致性，不用其时延作性能结论
go test -race ./internal/store -run '^$' \
  -bench '^BenchmarkTaskConcurrentCapacity/(memory|sqlite)/traces_(1000|10000)/readers_4$' \
  -benchtime=3x -count=1

go test ./...
go test -race ./internal/store ./internal/api ./internal/orchestrator
go vet ./internal/store
```

上述 4 个有读取 worker 的缩小场景竞态通过；每个 3 次保存/每观察者 3 次读取，竞态检测明显改变时延，不计入原始性能表。正常 CI 不运行基准；普通全仓/竞态仍自动检查已有保存、快照、租约与恢复行为。

全仓 51 个含测试包、store/api/orchestrator 三包竞态、定向 vet、差异和文档链接检查通过。日志在本机 `/private/tmp/ai-agent-task-contention-{full,suite-race}-20261012.jsonl` 及 `ai-agent-task-contention-race-20261012.log`，不提交。没有重复浏览器、容器、真实身份或 HA 验收。

下一步按 [保留归档方案](TaskTrace保留归档方案_2026-10-12.md) 实现只读候选评估入口，再按目标机器/请求速率测量队列和长期存储增长；自动清理与生产容量上限尚未交付。
