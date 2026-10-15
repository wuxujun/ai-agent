# HTTP 单步 Owned Adapter C03b4 首批验收

提交状态（2026-10-14）：本文与 C02a/b/c、C03a/b1/b2/b3 及 C03b4 首批 `/run` 实现一并纳入本批本地提交，准确提交号以 Git 记录为准。下文保留各阶段实现和验证边界；当前整体停点与最终验证见[会话续接文档](会话续接与完成进度_2026-10-13.md)。

2026-10-14，基线 `86bd534`。当前 C02/C03 改动已纳入本批本地提交。本批将显式 owned adapter 接入现有 `POST /api/tasks/:id/run` JSON/SSE；C03b4 的 `/run-all`、启动扫描与后台审批调用点尚未切换，整个阶段不能据此宣称全部完成。

## 默认关闭、可信构造期选择

新增 `internal/api/sqlite_owned_step.go` 的 `Handler.SetOwnedSQLiteStepAdapter(provider)`，只能在服务开始接受请求前调用。它要求 SQLite Store、Engine.Store 与 ExecutionAdmission 为同一实例，provider 非 nil；实际执行仍通过 C03 专用 Store 的原子 Start 验证。普通 SQLite backend 若误设，也不会回退绕过 gate，执行失败。

provider 是可信构造代码，接收已加载 Task 的 ID/tenant，返回当前预期 gate epoch、唯一 attempt/instance 身份、TTL 和必需 cleanup。不能从 HTTP body/query/Task 内容选择提供者或 cleanup；adapter 强制 Next 而非 RunAll。没有新增配置项、URL、请求参数、秘密读取或 server builder 自动启用；默认 nil 保持原有执行路径。

当前新增 hook 是专用实验接入，不是生产部署开关，不代表已完成 startup coverage/独立 authority/writer fencing。新 provider 不会自动 enroll 或解除 gate。

## `/run` 生命周期与 HTTP 边界

保留现有路由链、中间件、认证/tenant 与入口 semaphore。新 branch 在 Task 加载后、普通 UUID task lease 获取前选择 owned wrapper，避免在已获普通 lease 后再套第二个 owner。

- 注册 activeRun 时以 token 指针保留唯一槽位；冲突返回 409。
- worker 使用 C03b2 的同事务 Start/lease、keeper、Engine.Next、最后 flush、cleanup、停止 keeper 与 finalize。成功前没有额外 API SaveFullTask 或普通 ReleaseTaskLease。
- 新 `OnAdmitted` hook 仅在 durable registration 与 monitor 启动后通知 Handler；不表示 Engine 已结束。HTTP 不在该通知前提交 SSE headers 或成功响应；Start 拒绝可正常返回 409。
- JSON 等待生命周期返回后序列化 Task。SSE 订阅保持原有 bus/非终态事件，缓存/上游 terminal 事件在本 stream 不提前转发；owned wrapper 成功后才为本响应写 canonical terminal event。快速完成时即使 result 先被选中，也保持 SSE Content-Type/事件格式而非错误返回 JSON。
- worker 持有 wg/semaphore/activeRun 到完整返回，按原 token compare-and-delete。Engine/cleanup panic 在 worker 边界转换为固定错误，durable unresolved 记录不被伪造完成，也不令请求永远等待 result。
- 请求 trace 在 owned reload 后重新绑定，防止旧 checkpoint 的 execution_trace_id 替代当前请求 trace；无 trace 时清除旧执行标识。

错误/不确认的 cleanup 不产生 adapter 成功 terminal。其他已有 bus 订阅者或 Engine 回调没有被此 branch 改造成完整 stopped 证明；失败后的流关闭/状态查询仍不表示可恢复/清理。没有新增 HTTP 请求/响应 schema，现有 Sample 无需增加一个不存在的开关参数。

## owned 取消与关闭

`activeRun` 标记 owned。CancelTaskByID 对 owned run 只发送 cancel，不提前从 activeTasks 删除；worker 最后 flush/cleanup 返回后才移除，防止进程内 map 缺失被误读为退出或允许重叠执行。legacy run 保持原行为。

Shutdown/Wait 继续取消和等待。owned run 不走 rollbackInterruptedTask 的普通 owner 重新获取/状态回滚段；其完成或 unresolved 保留由 owned 生命周期负责。超时仍不能签 stopped，不自动打开 gate，不删除记录。完整 shutdown timeout/跨实例清理验收仍需后续矩阵，不把此条件分支当作完成证明。

## 验证

通过实际 RegisterRoutes 和中间件，全部 dedicated SQLite + fake Planner/Cleanup；没有真实模型、容器/工具/宿主机或在线库调用。

- JSON 与 SSE `/run` 都执行一次 fake Planner，最终唯一 attempt complete、lease absent；SSE 始终保持事件格式，包括快速完成。
- 未鉴权返回 401；非管理员其他 tenant 返回 404，均不登记 attempt、不调用 Planner。
- closed gate 的 JSON/SSE 返回 409，Planner/attempt 均为零。
- cleanup 阻塞时 API cancel 不移除 activeRun；attempt exited 且 lease 保留；非流式等待请求通过现有错误中间件返回取消错误，不落为空 200；cleanup 返回后 worker 完成并释放槽位。
- 请求有效 trace ID 穿过 reload 并保存到 Task。
- C03b2 owned execution/approval recovery 与其余旧路径由全仓保持验证。

```bash
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test ./internal/api -run '^TestOwnedSQLiteStep' -count=1 -timeout 90s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -p 1 ./... -timeout 120s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go test -race -p 1 ./internal/types ./internal/store ./internal/orchestrator ./internal/multiagent ./internal/api ./cmd/server ./cmd/task-restore -timeout 180s
GOCACHE=/private/tmp/ai-agent-backlog-check-cache go vet ./internal/types ./internal/store ./internal/orchestrator ./internal/multiagent ./internal/api ./cmd/server ./cmd/task-restore
git diff --check
```

最终取消响应修复后的定向、串行全仓、types/Store/Engine/Coordinator/API/server/CLI 竞态、专项 vet、相对链接和差异格式检查均 PASS。首轮最终测试前补了取消分支，测试响应 channel 类型调整曾导致编译失败，已修正并通过定向，不将该失败计为 PASS。日志 `/private/tmp/ai-agent-c03b4-target-20261014.log`、`/private/tmp/ai-agent-c03b4-full-20261014.log`、`/private/tmp/ai-agent-c03b4-race-20261014.log`。全仓/API 竞态 httptest 按权限流程在沙箱外运行。

## C03b4 续作

`/run-all` 原来的 pre-202 reload/status CAS、semaphore 排队、pausable timeout、stream disconnect 与补偿 flush 仍使用普通 owner；不能简单改成异步调用 owned wrapper，否则 202 可能早于真实 admission/CAS，或多套一次 task lease。下一步应拆显式 prepared/owned scope，把注册与前置 CAS、排队、执行/abort、finally 清理作为同一 lifecycle，再接启动/后台审批的 owned adapter。

启动 builder 和 API approval dispatcher 仍调用原 Recover*，虽然 C03b3 显式 recovery adapter 可用，也没有自动改用它或为其自动发身份。C03 强制进程退出、完整会话/实例集合、独立 control floor、真实 writer/cleanup/authentication 仍未验收。真实权威 NOT_RUN、恢复 blocked/restore_ready=false、HA/DAG 延后，默认配置不变，不调用在线恢复或清理源记录。
