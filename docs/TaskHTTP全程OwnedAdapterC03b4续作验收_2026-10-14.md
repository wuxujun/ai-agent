# HTTP run-all Owned Adapter C03b4 续作验收

本批提交状态：基线 `8fd19bf` 后的 prepared/run-all、审批/启动 dispatcher、竞争与六边界崩溃回归、Web UI 发布检查及后期优化文档，一并纳入本次本地提交，准确提交号以 Git 记录为准。正文较早阶段的“工作区未提交”描述保留为历史记录。未推送、创建 tag、部署或启用 owned 配置；真实登录仍待确认，恢复控制面后续仍暂缓。

后续进展：启动扫描和 API 后台审批 dispatcher 已接显式 owned adapter，见 [后续验收](Task审批Dispatcher与启动Owned接入C03b4验收_2026-10-14.md)，仍默认关闭。下文保留本批 run-all 历史停点。

2026-10-14，基线 `8fd19bf`。本轮实现 prepared owned scope 与 `/run-all` 显式 adapter；工作区改动尚未提交。启动扫描、后台审批 dispatcher 仍待接入，C03b4 整体未完成。

## 实现范围

- `Engine.PrepareOwnedSQLiteExecution` 同事务登记唯一 attempt/lease、启动续租并 reload 最新 checkpoint，返回 scope。可信调用者以 scope.Context 执行前置状态 CAS，再将 scope 交给异步执行者。
- scope 的 Execute/Abort 只能消费一次；两者共享退出、最后 flush、必需 cleanup、停止续租与 finalize。Abort 在 flush 前重新读取 checkpoint，避免拒绝前置 CAS 时覆盖较新状态。异常/panic 不编造完成回执。
- `Handler.SetOwnedSQLiteRunAllAdapter` 仅可信构造期显式启用，要求相同 SQLite Store/Engine/ExecutionAdmission；没有 server/config 默认启用或新增 HTTP 参数。原 `/run` setter 仍只影响单步入口。
- `/run-all` 在普通 lease 获取前分支；active token 保留到完整生命周期结束。登记、reload、终态复查、状态 CAS 都在 JSON 202 或 SSE headers 前完成。CAS 失败或重读为终态走 Abort，不执行 Planner。
- semaphore 排队在同一 scope 内，排队取消保存取消结果并清理；pausable timeout、请求 trace/context values 与租约续期保留。JSON 后台执行脱离 HTTP 请求取消，SSE 断开取消执行但保留清理中的 active token。
- SSE 过滤上游/缓存终态，只在生命周期成功返回后发送本响应的 terminal event。cleanup 失败或 panic 不发成功 terminal；active token 缺失仍不构成 stopped 权威证明。
- 默认 nil，未切换运行配置，没有在线库/真实工具/付费模型或恢复调用。真实权威 NOT_RUN/unverified，恢复 blocked/restore_ready=false，HA/DAG 延后。

## 回归与验证

专用 SQLite + fake Planner/Cleanup 覆盖 JSON/SSE、blocked gate 在响应前拒绝、202 前登记/CAS、排队取消与清理中保留 owner、SSE 等待清理及断开取消、初次读取后 checkpoint 已变终态、Abort 重读防覆盖、prepared 等待续租、scope 禁止重复消费、Execute 前取消仍进入取消处理 callback。原单步及审批 owned 回归继续执行。

最终验证命令与结果完成后记录于本文末尾。首轮新增 SSE 测试误断言 `event: done`，项目既有协议为 `data:` JSON，修正后重新执行；一次默认 Go cache 读取被沙箱拒绝，随后使用专用缓存按权限流程重跑，未把失败记录为 PASS。

## 下一步

接启动扫描及 API 后台审批 dispatcher 的显式 owned adapter，继续默认关闭。后续仍需完整入口/强制崩溃矩阵、真实写者保护/fencing、宿主机 cleanup 与独立 signer/trust；本轮 dedicated fixture 不代表这些能力已通过。

## 最终验证结果

最终代码冻结后以下命令均 PASS，Go 1.26.8，使用 `GOCACHE=/private/tmp/ai-agent-backlog-check-cache`：

```bash
go test ./internal/api ./internal/orchestrator -run 'Test(OwnedSQLite|SQLiteOwned)' -count=1 -timeout 90s
go test -p 1 ./... -timeout 120s
go test -race -p 1 ./internal/types ./internal/store ./internal/orchestrator ./internal/multiagent ./internal/api ./cmd/server ./cmd/task-restore -timeout 180s
go vet ./internal/types ./internal/store ./internal/orchestrator ./internal/multiagent ./internal/api ./cmd/server ./cmd/task-restore
git diff --check
```

定向、全仓与七包竞态最终日志分别为 `/private/tmp/ai-agent-c03b4-all-final-target-20261014.log`、`/private/tmp/ai-agent-c03b4-all-final-full-20261014.log`、`/private/tmp/ai-agent-c03b4-all-final-race-20261014.log`。七包竞态部分未变包命中 Go 测试缓存，API 实际执行通过；不宣称所有包均强制无缓存重跑。专项 vet、文档相对链接与差异检查也通过。

最终全仓和竞态曾在沙箱内因 httptest 监听禁止而失败，最终日志来自按权限流程在沙箱外的重跑。早期 `all-race` 日志存在并行进程写入交错，不作为最终证据；以 `all-final-*` 日志及对应命令退出码为准。所有结果仅为专用 fixture/fake 回归，没有真实权威、生产身份、宿主机清理或崩溃验收通过声明。本轮未提交、推送、部署或切换配置。
