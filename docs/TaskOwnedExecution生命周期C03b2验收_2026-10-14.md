# Owned Execution 生命周期 C03b2 验收

提交状态（2026-10-14）：本文与 C02a/b/c、C03a/b1/b2/b3 及 C03b4 首批 `/run` 实现一并纳入本批本地提交，准确提交号以 Git 记录为准。下文保留各阶段实现和验证边界；当前整体停点与最终验证见[会话续接文档](会话续接与完成进度_2026-10-13.md)。

2026-10-14，基线 `86bd534`。C02a/b/c、C03a、C03b1 和 C03b2 已纳入本批本地提交。本轮完成显式专用 SQLite owned execution 包装器与 fake 生命周期回归，不启用 server/API/config，不签 stopped 或执行恢复。


后续 [C03b3 审批 recovery adapter、Coordinator scope 修复及 HTTP 矩阵](Task审批恢复与外层接入C03b3验收_2026-10-14.md) 已实现并纳入本批本地提交，HTTP/启动 adapter 尚未开启，下一步 C03b4。
## 显式 owned 入口

新增 `internal/orchestrator/sqlite_owned_execution.go`：`Engine.RunOwnedSQLiteExecution(ctx, store, task, options)`。

入口要求 Engine.Store 与 ExecutionAdmission 都是同一个 C03 专用 Store，明确提供唯一 attempt ID、instance 摘要、预期 gate epoch、TTL（300 ms–1 h）、Next/RunAll 选择，以及必需的 Cleanup callback。缺 callback/配置不匹配/已取消时，不登记 attempt；没有隐式 no-op cleanup。

按以下顺序执行：

1. 原子 Start：复用 C03a gate/attempt/lease 事务，并绑定 attempt 与业务 lease scope。
2. 启动现有 ExecutionLease，失效时取消执行；初始有效期从 Start 调用前保守计时，不把锁等待算成额外已确认所有权。
3. 登记后重新读取 Task，核对 scope，替换调用方的旧 checkpoint，再调用实际 Engine.Next 或 RunAll。保留 C03b1 准入检查和既有 lease loss 检查。
4. Engine 同步返回后记录局部 exit 摘要：固定域、immutable identity、返回/拒绝/取消/错误/未进入结果分类，不包含原始错误正文或 Goal。该记录仅表示本 goroutine 观察到调用返回，不证明宿主机/工具均退出。
5. 仍持续续租，使用保留 attempt/lease values 的 WithoutCancel + 10 秒上下文做最后 SaveFullTask，并原子读回 C02 baseline；flush 摘要绑定 scope 与读回 baseline 摘要。Task ID/tenant 被改动、lease lost、保存/读回失败，不确认 flush。
6. 即使 exit/flush 失败也尝试 Cleanup；callback 接收不可变 scope 副本，须等待实际所属工具清理完成、遵守 context，并返回 64 位小写十六进制摘要。超时/错误/缺失或非法摘要统一为脱敏的 cleanup unconfirmed。
7. 等 Cleanup 返回后停止并等待 renewal goroutine，检查最后 ownership 状态；仅所有阶段成功才 finalize。
8. finalize 在同一 SQLite 事务中重新锁定/核对当前 lease owner 和有效期，写完整回执并释放 lease。过期 ownership 拒绝，保留 exited/expired-unresolved。

执行错误仍返回给调用方；真实取消可能在成功 flush/cleanup 后得到 complete attempt，这是局部执行生命周期完成，不表示任务执行成功。gate open 时不会得到 ReceiptsComplete=true；即使关闭后该字段为 true，snapshot 仍 unverified，不升级恢复。

## 失败保留与 scope 修复

cleanup 失败、缺摘要、flush 失败、ownership 丢失或 finalize 失败，都不发布完整 flush/cleanup 回执或删除 lease，保留 exited/unresolved；续租在包装器结束时停止，TTL 后仍不能自动移除 attempt。回调必须合作遵守 context，包装器不通过遗弃后台 cleanup goroutine 来伪造返回。

Engine panic 时，defer 停止 renewal，保留 running 且无 exit/cleanup 回执；cleanup panic 时保留已有 exited 且不 finalize。panic 继续传递给调用方，不把普通 panic 当作完整宿主机收尾。没有提供失败后的自动重试执行或跨进程恢复准入。

复核补齐 attempt 的 source origin/namespace：Start 与只读 snapshot 返回实际源身份，C03b1 checker 每次核对上下文与当前源的 origin/namespace，owned exit/flush/cleanup identity 同样绑定这些字段。两个专用库即使 task/tenant/attempt ID、instance、gate epoch、Task incarnation 相同，也不能混用 ticket。它仍是本地上下文契约，不是经过身份认证的客户端凭证。

## 新增回归与验证

所有测试使用专用 SQLite 文件与 fake Planner/Cleanup，没有真实模型、工具/容器/宿主机操作。

- stale 调用方 Task 被 reload；cleanup 观察到 exited、已持久化完成状态和仍有效 lease；最终 complete/lease absent，不自动打开 gate。
- cleanup error/缺摘要/panic 保留 exited、不发布完整回执；私密 callback 错误不出现在返回错误中。
- 取消后使用独立收尾 context flush，cleanup 不继承取消；返回仍含 context.Canceled，局部 attempt 可以完成。
- cleanup 运行时间超过原 TTL，renewal 仍维持 active-unresolved，随后正常完成。
- Task scope 被改变导致 flush 失败仍执行 cleanup，但不 finalize。
- 缺 cleanup 在登记前拒绝；Engine panic 保留 running、无退出/清理声明。
- finalize 时真实 lease 已过期，事务拒绝且保留 exited/expired-unresolved，不清理证据。
- 两个同 ID/tenant/instance/epoch/incarnation 专用库，foreign origin ticket 在任何 Planner 动作前拒绝。

```bash
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test ./internal/orchestrator ./internal/store -run '^Test(SQLiteOwned|SQLiteExecutionGate|ExecutionAdmission)' -count=1 -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -p 1 ./... -timeout 120s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -race -p 1 ./internal/types ./internal/store ./internal/orchestrator ./internal/multiagent ./internal/api ./cmd/server ./cmd/task-restore -timeout 180s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go vet ./internal/types ./internal/store ./internal/orchestrator ./internal/multiagent ./internal/api ./cmd/server ./cmd/task-restore
git diff --check
```

最终定向、串行全仓、types/Store/Engine/Coordinator/API/server/CLI 竞态、专项 vet、相对链接与差异格式检查均 PASS。首轮竞态尚未结束时追加 origin 字段，检查读到新旧接口混合导致构建/vet 失败；不计为 PASS，固定代码后已重新启动全仓/竞态/vet。日志 `/private/tmp/ai-agent-c03b2-target-20261014.log`、`/private/tmp/ai-agent-c03b2-full-20261014.log`、`/private/tmp/ai-agent-c03b2-race-20261014.log` 为最终重跑路径。全仓/API 竞态 httptest 按权限流程在沙箱外运行。

## 下一步 C03b3

API/Handler 的普通 owner、审批 recovery lease 生命周期、Coordinator detached persistence 尚未改用 owned scope；先审查它们的最后 flush/取消/关闭/lease cleanup 归属，定义显式 opt-in adapter 和覆盖矩阵，再用 dedicated fixtures 接入。C03 强制进程退出、完整会话闭包/实例目录、独立 control floor 与真实 cleanup/authentication 仍未验收，不能用 fake callback、Engine 返回或 TTL 宣称整个集群停止。

C02 真实写者保护/fencing/独立服务与 C04 signer/trust 尚未接入；真实权威 NOT_RUN，恢复 blocked/restore_ready=false，HA/DAG 延后，默认配置不变，不调用在线恢复或删除源记录。
