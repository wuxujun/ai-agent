# Brain Read-Only MVP P1 实施续接文档

日期：2026-09-03
状态：暂停，等待新会话从 Task 2 继续

## 1. 当前目标

按照已批准的 Brain Read-Only MVP P1 设计，实现项目级、只读、不可变快照式 Brain Wiki。发布、回滚和验证仅允许通过运维 CLI；HTTP 保持只读。Gemini 只生成不可信提案，不能发布。

规范与计划：

- 英文设计：`docs/superpowers/specs/2026-09-02-brain-readonly-mvp-design.md`
- 中文设计：`docs/superpowers/specs/2026-09-02-brain-readonly-mvp-design-zh.md`
- 实施计划：`docs/superpowers/plans/2026-09-02-brain-readonly-mvp.md`

## 2. Git 与工作区状态

- 开发分支：`feat/brain-readonly-mvp`
- 隔离工作树：`.worktrees/brain-readonly-mvp`
- 当前 HEAD：`39ae9aae679b7e669ece82d09a63ecd2d892e56a`
- 起始提交：`ed4a70fce35e91c48a3fa59ec379ecdbeba457ab`
- 分支尚未推送、合并或发布。
- 隔离工作树在暂停时无未提交代码修改。
- 主检出目录中用户自己的 `build.sh` 修改未被本分支读取、暂存或覆盖。

已完成提交：

1. `a1dadfe feat: add brain project configuration`
2. `39ae9aa fix: validate disabled brain compiler settings`

## 3. 已完成工作

### Task 1：Brain 配置与项目解析

状态：完成，规范与代码质量复审通过。

已实现：

- 默认关闭的 `BrainConfig`、`BrainCompilerConfig` 与租户级 `BrainProjectConfig`。
- `tenant_id + brain_project_id` 的显式解析和租户项目 allowlist。
- 严格项目 slug、唯一 Wiki space、稳定租户存储键和配置摘要。
- Brain 配置深复制、变更检测、热重载候选验证与旧快照保留。
- `brain.root` 标记为重启生效。
- `config.yaml` 与 `config_zh.yml` 中英文配置说明。

验证：

- Brain/config 聚焦测试通过。
- `go test ./...` 在允许本地 `httptest` 回环监听的环境中通过。
- 沙箱内的初次全量运行仅因禁止绑定 `[::1]:0` 失败，不是代码断言失败。

评审修复：

- 已修复“Brain 关闭时不验证编译器不变量”的问题。
- 编译器限制、有限且非负成本、Gemini provider 和非空 model 均在配置存在时验证；只有 root 非空要求依赖 `enabled=true`。

## 4. 当前暂停点

### Task 2：Task 身份、API 准入与持久化

状态：未开始代码修改。

原实现代理因账户用量上限终止。暂停时确认：

- 没有 `task-2-report.md`；
- 没有 Task 2 工作树修改；
- 没有 Task 2 提交；
- 可直接从 Task 2 的 RED 测试重新开始。

Task 2 要求：

- 为 `types.Task` 增加 `BrainProjectID`、`BrainSnapshotID`、`BrainConfigDigest`。
- Task 创建请求接受可选 `brain_project_id`。
- 使用已认证租户调用 `brain.ResolveProject`；未知或跨租户项目返回 403。
- SQLite、Postgres、Redis 必须完整往返三个字段。
- SQLite/Postgres 增加兼容旧数据的非空默认列和迁移测试。
- 不得从 `Workspace` 推断 Brain 项目。
- 更新 `Sample/agent-api.http`。

Task 2 简报位于忽略目录：

`.superpowers/sdd/2026-09-02-brain-readonly-mvp/task-2-brief.md`

## 5. 未完成任务

| Task | 内容 | 状态 |
|---|---|---|
| 2 | Task 身份字段、API 准入、SQLite/Postgres/Redis 持久化 | 下一项 |
| 3 | Provenance 数据模型、来源资格过滤、发现提示 | 未开始 |
| 4 | 撤回账本、不可变 staging/release、CAS 发布与回滚 | 未开始 |
| 5 | 确定性 Markdown/索引渲染与验证门 | 未开始 |
| 6 | 受限 Gemini 结构化合成与编译器编排 | 未开始 |
| 7 | `brain-compile` 运维 CLI 与 Store 工厂 | 未开始 |
| 8 | Brain Wiki Provider、`corpus=wiki|brain|all` | 未开始 |
| 9 | Task 快照固定、恢复、JIT 与只读页面授权 | 未开始 |
| 10 | 指标、就绪状态、配置与运维文档 | 未开始 |
| 11 | 端到端测试、全量验证、P1 证据记录 | 未开始 |

Task 11 的 Live Eval 必须再次停止并取得明确批准后才能执行。批准范围固定为：

- 最多 288 次 Writer/Judge 调用；
- 60,000 token 准入上限；
- 1.00 美元成本上限；
- 使用 `GEMINI_API_KEY`，不得打印或持久化密钥。

## 6. 已记录执行裁定

1. Task 3 的 `SourceSet` 增加 `DiscoveryHints []string`，仅接收经过边界限制和脱敏的 Task Memories/FinalAnswer；所有 claim 仍必须引用合格的成功 Trace evidence。
2. Task 11 可以使用测试专用的确定性 snapshot ID fixture；不得仅为测试增加生产环境调用者可控的 snapshot ID。
3. Task 7 使用 `go build -o /private/tmp/brain-compile ./cmd/brain-compile`，避免在仓库根生成未跟踪二进制。
4. SDD 忽略目录不得递归删除；仓库规则禁止批量删除目录，最终由用户决定是否手动清理。
5. `ProjectRef.StorageKey` 保持为 `tenant_id` 的稳定哈希；批准的目录布局是 `<tenant-key>/<project-id>`，完整作用域由 `StorageKey + ProjectID` 构成。
6. Task 4 必须测试同一租户下两个项目的完整解析根目录不同，而不是要求两个项目产生不同的租户 StorageKey。

## 7. 新会话恢复步骤

从仓库主目录执行：

```bash
cd .worktrees/brain-readonly-mvp
git status --short --branch
git log --oneline -5
```

预期分支为 `feat/brain-readonly-mvp`，HEAD 为 `39ae9aa`，工作树除本续接文档提交外应为空。

新会话应：

1. 使用 `superpowers:subagent-driven-development`；
2. 读取本文件、批准的英文设计和实施计划；
3. 读取计划专属账本：`.superpowers/sdd/2026-09-02-brain-readonly-mvp/progress.md`；
4. 将 Task 1 视为完成，不得重新派发；
5. 从 Task 2 的 RED 测试开始，使用新的实现代理；
6. 每个 Task 完成后执行一次规范合规与代码质量评审；
7. 不并行派发多个实现代理；
8. 不推送、合并或发布，除非用户另行明确授权。

## 8. 隐私与安全封印

本续接文档不包含 API key、Authorization、Cookie、DSN、提示词、模型原始响应、客户数据、会话转录正文或绝对用户目录。所有路径均为仓库相对路径，所有测试说明均为汇总结果。

恢复时不得把 SDD 会话摘要当作规范事实；英文设计文档仍是绑定权威。
