# Freestyle 预算限制与环境文件核对（2026-10-09）

后续用户已选择仅免费，付费预算为 US$0；根目录 `.env` 已配置并通过只读认证，权限收紧至 0600。最新限制、操作工具和验证结果见[仅免费策略记录](ha-freestyle-free-only-2026-10-09.md)。下文保留提出预算选择时的历史核对记录，不代表当前凭据或示例拓扑状态；不再等待金额选择，也不启用付费方案。

## 结果

按用户新增的预算要求，已在 `provision.mjs` 增加创建前的有限运行上限检查：单次和累计秒数都必须是正整数，单次不得大于累计；未填写时在任何云资源调用前拒绝。每台创建后严格核对服务端返回的有效上限，丢失或放宽时停止后续操作并保存已创建资源 ID。示例拓扑两个字段为 null，未选择费用额度或运行窗口。

**本轮美元预算及 Freestyle 凭据仍待配置，云端资源未创建。** 已向用户询问仅免费、US$5 或 US$10 三种预算选择，未把预选项或等待时间视为同意。新增检查约束计算运行时间，`fullDollarSpendCapEnforced=false`；不宣称存在美元账单硬上限。

## 费用依据及范围

根据 [Freestyle 定价](https://www.freestyle.sh/docs/vms/pricing-and-limits)与[默认镜像规格](https://www.freestyle.sh/docs/vms/base-snapshots)，四台 4 vCPU / 8 GiB / 32 GB 的公开原价估算：

| 项目 | 原价估算（未扣免费额度） |
|---|---|
| 四台计算资源 | US$1.05792/小时 |
| 四台磁盘 | US$0.011008/小时 |
| 计算与磁盘合计 | US$1.068928/小时 |
| 连续 4 小时 | US$4.275712 |
| 连续 6 小时 | US$6.413568 |
| 暂停后磁盘保留 24 小时 | US$0.264192 |

估算不含额外流量、其他资源或开通付费套餐的月费。实际费用以账号剩余额度与账单为准。Free 没有付费超额，计算额度耗尽会暂停；Hobby/Pro 允许超额计费，不能假设有试验总额硬上限。只允许免费时须先核验实际账号为 Free 及剩余额度，不自动升级套餐。

[平台生命周期](https://www.freestyle.sh/docs/vms/lifecycle)说明累计运行上限到期会暂停，重新开始不会重置；暂停仍保存磁盘。本轮不设置自动删除 TTL，也不提高运行上限。存储、流量、套餐费与其他账号操作不受本脚本的累计运行上限约束；选择 US$5/10 仍需要明确账单控制与运行时间安排，不能据此自动启动资源。

## 项目环境文件清单

检查项目根目录及其中两个子工作树，包含隐藏与 Git 忽略文件，排除 Git 内部和依赖缓存。实际配置均未配置 `FREESTYLE_API_KEY` 或其他 FREESTYLE 字段，没有打印凭据值。

| 实际文件（相对项目根目录） | 权限 | Git 状态 |
|---|---|---|
| `.env` | 0644 | 已忽略、未跟踪 |
| `deploy/litellm/.env` | 0644 | 已忽略、未跟踪 |
| `deploy/opentelemetry/.env` | 0644 | 已忽略、未跟踪 |

建议承载新增密钥的文件使用 0600。本轮只核对文件，未修改既有环境文件或权限。Freestyle 操作端只需要 API key；建议在根目录已有、被忽略的 `.env` 配置，不复制到 LiteLLM、OpenTelemetry 或任何 guest。

另有九份模板：

- `deploy/litellm/.env.example`
- `deploy/opentelemetry/.env.example`
- `deploy/systemd/ai-agent-canary.env.example`
- `.worktrees/brain-readonly-mvp/deploy/litellm/.env.example`
- `.worktrees/brain-readonly-mvp/deploy/opentelemetry/.env.example`
- `.worktrees/brain-readonly-mvp/deploy/systemd/ai-agent-canary.env.example`
- `.worktrees/brain-eval-p0/deploy/litellm/.env.example`
- `.worktrees/brain-eval-p0/deploy/opentelemetry/.env.example`
- `.worktrees/brain-eval-p0/deploy/systemd/ai-agent-canary.env.example`

按[官方 CLI 文档](https://www.freestyle.sh/docs/cli)，CLI 自动读取的是当前目录的 `.env`。自定义 `provision.mjs` 读取进程环境，不自动遍历目录。实际执行时仅从明确私密文件提取 Freestyle 所需字段，不能 `source` 整份应用环境或依赖模板提供凭据。

本次私有执行目录的 `assets` 另有九份生成的 `*.env`，均为 0600，均不含 Freestyle API key：A/B 各 `bootstrap.env`、`ai-agent.env`；Storage 的 `bootstrap.env`、`storage.env`；Control 的 `bootstrap.env`、`control.env`、`stub.env`。它们含各角色测试秘密，只按角色分发，不能整包提交。没有扫描整台电脑或其他项目。

## 操作工具与验证

新增预算保护尚未提交，操作工具身份为 `9aeb24b` 上的本次工作区修改。应用源码未变，先前验证的干净 `9aeb24b` 发布包身份保持有效；预算控制属于独立操作工具，不伪造应用包提交。

已在私有执行目录准备最新工具副本：

`/private/tmp/ai-agent-ha-freestyle-20261009.105jkxi_/budget-guard-tools/deploy/ha/freestyle`

待预算配置的拓扑与规划为 `topology.pending-budget.json`、`plan.pending-budget.json`；使用既有固定 SDK 0.2.16。旧 `kit` 与固定 `source` 内的创建器为历史版本，**不得再用其关闭运行上限的逻辑创建资源**。预算选定后，从最新工具或对应新提交执行创建，并保存操作工具摘要及实际限制。

- Node 回归 6 项通过，含未配置/无效上限拒绝、服务端预算丢失或扩大时停止。
- Python 4 项通过。
- 当前工作区全仓 `go test -p=4 ./... -count=1 -timeout=5m`，49 个含测试的包通过；经沙箱外授权，退出码 0，日志为执行根目录的 `full-suite-budget-guard.log`。
- 新工具离线规划成功，明确 `runtimeLimitConfigured=false`、`fullDollarSpendCapEnforced=false`。
- `git diff --check` 通过。

仍等待预算选择、凭据位置与账号核验；阶段 3.3 保持未完成。
