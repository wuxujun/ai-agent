# 审批 Dispatcher 与启动 Owned 接入 C03b4 验收

本批提交状态：基线 `8fd19bf` 后的 prepared/run-all、审批/启动 dispatcher、竞争与六边界崩溃回归、Web UI 发布检查及后期优化文档，一并纳入本次本地提交，准确提交号以 Git 记录为准。正文较早阶段的“工作区未提交”描述保留为历史记录。未推送、创建 tag、部署或启用 owned 配置；真实登录仍待确认，恢复控制面后续仍暂缓。

后续已新增 [竞争与崩溃失败矩阵](TaskOwned竞争与崩溃失败矩阵C03验收_2026-10-14.md) 专用回归，仍不代表真实 writer/宿主机隔离或完整 stopped 证明；下文保留本批历史范围。

2026-10-14，基线 `8fd19bf` 及此前未提交的 [prepared/run-all 续作](TaskHTTP全程OwnedAdapterC03b4续作验收_2026-10-14.md)。本轮同样保留在工作区，未提交或推送。

## 接入与默认行为

`Engine.SetOwnedSQLiteApprovalDispatcher(provider)` 是可信构造期 hook，必须在扫描/请求开始前调用，要求同一 SQLite Store 与 ExecutionAdmission；默认 nil，没有新增 server/config 开关。provider 只接收可信加载的 task ID/tenant，提供唯一 attempt/instance、当前 gate epoch、TTL 与必需 cleanup。provider 错误统一拒绝，不回退普通 lease。

`Engine.DispatchApprovalRecovery` 由 `startPausedTaskScan` 与 `Handler.startDurableApprovalRecovery` 共同调用。默认使用原 RecoverApproved/Rejected；显式 owned 时使用 C03b3 recovery wrapper，并复用 prepared scope 的登记、reload、退出、flush、cleanup 与 finalize。审批自己的消费 lease/CAS 保留，批准动作仍先消费后执行，拒绝不执行动作。传入 legacy owner 不会覆盖 owned attempt owner。provider 不能通过 RunAll 或 OnAdmitted 改写此执行入口。

启动扫描仍由原 BackgroundRunner 的 Stop/Wait 管理：Stop 取消扫描/执行，Wait 等到 detached 最后 flush 和 cleanup 返回。扫描与 Engine 的 Store 绑定不一致时拒绝 owned 扫描。扫描原有 500 条分页上限、30 秒上下文与恢复任务筛选维持现状；不宣称已经覆盖所有历史任务或全部实例。

API owned 后台恢复在启动前按 approval ID 登记 worker。同一 approval 的重复派发去重；如果 task 已有本地 active owner，worker 等待空槽且不覆盖原 token。等待中的 worker 单独登记，CancelTaskByID 与 Shutdown 均可取消并等待；获得槽位后保持到 owned cleanup/finalize 返回，compare-and-delete 只清理自己的槽位。Task lease busy 或未恢复且 attempt conflict 可在原 90 秒期限内等待重试；blocked gate/provider/cleanup 错误不回退普通 owner。失败或过期仍保留 durable 决定/未完成 attempt，不把超时当 stopped，不自动解除 gate。

API owned worker 捕获 panic 为固定诊断，保留未完成回执；启动扫描延续原进程 panic 行为，没有伪造 complete。批准/拒绝 HTTP 的持久化决策响应保持现有语义，202 表示决定已接受而非后台恢复/清理成功；当前没有新增请求或响应 schema，Sample 无需新增不可用的开关。

## 专用回归

使用私有专用 SQLite、fake executor 与 fixture cleanup，没有在线数据库、真实身份、模型或工具调用。

- API 后台批准/拒绝：唯一 attempt complete、lease absent、审批 consumed、Task paused；批准动作一次、拒绝零次。
- closed gate：API/启动都不登记 attempt、不消费决定、不调用 executor。
- API cleanup 中重复派发不执行第二次；Shutdown 等待 cleanup，保留 active owner/lease，完成后释放。
- 本地 slot 忙时 worker 不丢失：等待后恢复；等待中取消不登记 attempt，不移除其他 token。
- 启动批准/拒绝恢复使用 owned attempt；runner Stop/Wait 等待 detached cleanup。
- provider failure 与 Engine/Store 绑定变化 fail closed，错误不暴露 provider 内容，不走普通 owner。
- 前批 `/run`、`/run-all`、prepared scope、审批 recovery、legacy 与启动关闭回归继续执行。

## 边界与下一步

本轮补齐两个已列明调用点的显式 adapter，不代表真实部署已开启或完整 C03 stopped 证明已完成。后续先补 forced process-exit/最后 flush/cleanup 失败/续租失效矩阵与跨入口竞争验证，再推进真实 writer fencing、独立实例/宿主机清理及 C04 signer/trust。真实权威保持 NOT_RUN/unverified，恢复 blocked/restore_ready=false，source_preserved=true；HA/DAG 延后。没有修改运行配置、启动在线服务、在线恢复或清理源记录。

## 最终验证

最终代码定向、串行全仓、七包竞态、七包 vet 均 PASS，Go 1.26.8，`GOCACHE=/private/tmp/ai-agent-backlog-check-cache`：

```bash
go test ./internal/api ./internal/orchestrator ./cmd/server -run 'Test(SQLiteOwned|OwnedSQLite|DurableApprovalRecovery|PausedTaskScan)' -count=1 -timeout 90s
go test -p 1 ./... -timeout 120s
go test -race -p 1 ./internal/types ./internal/store ./internal/orchestrator ./internal/multiagent ./internal/api ./cmd/server ./cmd/task-restore -timeout 180s
go vet ./internal/types ./internal/store ./internal/orchestrator ./internal/multiagent ./internal/api ./cmd/server ./cmd/task-restore
git diff --check
```

最终日志为 `/private/tmp/ai-agent-c03b4-dispatch-final-target-20261014.log`、`/private/tmp/ai-agent-c03b4-dispatch-final-full-20261014.log`、`/private/tmp/ai-agent-c03b4-dispatch-final-race-20261014.log`。全仓和竞态按权限流程在沙箱外运行；types/store/CLI 等未变包有 Go 缓存命中，Engine/Coordinator/API/server 竞态实际执行通过，不宣称全包无缓存重跑。文档相对链接与差异检查通过。所有本轮与上轮工作区改动保留，未提交、推送或启用配置。
