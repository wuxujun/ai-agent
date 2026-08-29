# Brain Eval P0 设计

日期：2026-08-29
状态：已批准设计
相关分析：`records/s260827.md`

## 1. 目的

P0 用于判断：在仓库投入建设 Brain Compiler、Dream Worker 或生产写入链路之前，Project 级 Brain Wiki 相比现有 Session 和 Memory 上下文层是否具有可衡量的增量价值。

实验在完全相同的合成历史上比较两个匹配的 Variant：

- **Baseline：** 可以使用 Session、Task、Trace 和 Memory，但没有 Brain。
- **Candidate：** 使用完全相同的底层历史，并额外提供人工编写的 Gold Brain Wiki。

唯一预期的实验变量是 Brain 是否可见。评测必须覆盖跨 Session 质量、证据来源、新鲜度、安全边界、延迟、Token 和成本。

## 2. 范围

Brain 是现有执行模式共享的正交上下文能力。P0 不增加 `orchestrator.mode: brain`。

长期作用域键为：

```text
tenant_id + project_id
```

Session ID 继续作为证据和时间顺序标识，但不限制 Project 级长期召回。

P0 包含：

- 独立的 `internal/braineval` 包；
- `cmd/brain-eval` CLI；
- 可安全提交仓库的合成 Fixtures；
- 确定性离线评测；
- 可选的匹配式 Live LLM 评测；
- Baseline/Candidate 对比和发布门禁。

P0 不包含：

- 从原始历史编译 Brain；
- Dream 后台 Worker；
- 永久 Wiki Apply 或自动发布；
- 生产 API 或运行时配置变更；
- 导入真实用户对话、数据库或工作区内容。

## 3. 仓库布局

```text
evals/brain/
├── dataset.yaml
└── fixtures/
    └── project-atlas/
        ├── sessions.jsonl
        ├── memories.jsonl
        ├── retractions.jsonl
        └── brain/
            ├── _index.md
            ├── entities/
            ├── concepts/
            └── projects/

internal/braineval/
├── dataset.go
├── runner.go
├── metrics.go
└── compare.go

cmd/brain-eval/
└── main.go
```

实现时可以拆分职责明确的测试或辅助文件，但以上包边界保持不变。

## 4. 数据集模型

数据集使用 YAML，并强制要求 `version: 1`。大规模历史记录和 Wiki 页面存放在 Fixture 文件中，使 Manifest 保持可审阅。未知的顶层字段和 Case 字段必须被拒绝，防止拼写错误的安全期望被静默忽略。

每个 Project Fixture 声明其 `tenant_id`、`project_id`、Sessions、Memories、Retractions 和 Brain 目录。每个 Case 声明：

- 稳定的名称和类别；
- Tenant 和 Project Scope；
- 查询；
- 预期 Claims；
- 预期 Evidence URI；
- 禁止出现的 Claims；
- 是否预期无答案；
- 是否属于安全关键 Case。

支持的 Evidence URI Scheme 包括合成的 `session://`、`task://`、`memory://`，以及现有绝对形式 `wiki://<space>/<kind>/<slug>`。Case 或 Brain 页面引用的每个 URI 都必须能在同一 Fixture 和已授权 Project 内解析，除非该 Case 明确用于模拟禁止的跨边界候选。

第一版数据集包含 24 个 Case：

| 类别 | 数量 | 目的 |
|---|---:|---|
| 跨 Session 偏好 | 4 | 语言、格式和输出偏好 |
| Project 决策 | 4 | 历史选择、原因和负责人 |
| 时间与事实替代 | 4 | 新事实替代陈旧事实 |
| 多来源综合 | 4 | Claim 需要多个 Session 支持 |
| 相似实体隔离 | 3 | 相似名称不得互相污染 |
| Tenant/Project 隔离 | 2 | 跨边界结果必须为零 |
| 删除和撤回 | 2 | 已撤回事实不得再次出现 |
| 无答案 | 1 | Brain 不得制造知识 |

所有名称、组织、事实、时间戳、对话和产物均为虚构内容，可以安全提交到仓库。

## 5. Gold Brain 契约

Gold Brain 是人工编写的理想知识层，不是 Compiler 输出。它用于将 Knowledge Wiki 的价值与未来 Compiler 的生成质量分离评估。

每条 Brain 事实 Claim 必须：

- 引用至少一个已存在的合成来源；
- 保持在单一 Tenant/Project Scope 内；
- 区分当前事实和历史事实；
- 当新 Claim 取代旧 Claim 时标明替代关系；
- 从当前综合结果中移除已撤回 Claim；
- 将来源内容视为证据，而不是指令。

紧凑 `_index.md` 限制为 4,000 个 UTF-8 字节，只包含导航元数据，不包含完整来源正文。后续可以用这些 Gold 页面评估 P1 Compiler 的生成质量。

## 6. 评测架构

`internal/braineval` 暴露 `VariantRunner` 抽象。两个 Variant 使用完全相同的 Case、超时、最终 Evidence 数量限制和最终 Evidence 字节预算。

```text
加载并校验同一份 Project 历史
                 │
        ┌────────┴────────┐
        │                 │
Baseline             Candidate
仅 Memory            Memory + Gold Brain
        │                 │
        └────────┬────────┘
                 ↓
          对齐 Case 结果
                 ↓
     汇总、增量、回归和门禁
```

离线 Runner 使用 Store Memory Query 契约和现有 Wiki `DirectoryClient`。每个可用分支最多返回 8 个候选。候选排名使用 `k=60` 的 Reciprocal Rank Fusion 合并；排名前先按 Canonical Evidence URI 去重。Baseline 只有 Memory 分支。评分前，两个 Variant 都缩减为最多 3 条 Evidence 和 8,000 个 UTF-8 字节。合并过程不得调用 LLM，也不得静默扩大 Evidence 预算。

Live Runner 对两个实验组使用相同的 Planner、Writer、模型配置、Prompt 预算和超时。Baseline 不接收 Brain Index 或 Brain 工具；Candidate 接收受限的紧凑 Index 和只读 Brain 访问。每个 Case 默认运行 3 次；汇总使用中位数，并报告不稳定 Case。

## 7. 指标

离线 CI 测量：

- 预期 Evidence Recall；
- 预期和禁止 Claim 的选择情况；
- 新事实与陈旧事实的选择情况；
- No-answer False-positive Rate；
- Citation Coverage；
- Tenant/Project 泄漏；
- 已撤回事实复现；
- 持久化 Prompt Injection 复现；
- P95 评测延迟。

Live 评测额外测量：

- 最终回答准确率；
- 可选的 LLM Judge 分数和原因；
- Prompt、Completion 和 Total Tokens；
- 估算成本；
- 端到端延迟；
- 多次重复运行的结果稳定性。

结果分为三个层级：单 Case Result、单 Variant Summary 和 Paired Comparison。Paired Comparison 必须列出每一项回归和改善，而不能只报告聚合增量。

## 8. 初始门禁

Candidate 必须满足以下全部条件：

- Live 跨 Session 回答准确率相比 Baseline 至少提高 10 个百分点。
- 离线 Expected-evidence Recall 至少提高 10 个百分点。
- 事实替代、撤回、Tenant 隔离和 Project 隔离 Case 100% 通过。
- Wiki Citation Coverage 为 100%。
- 相似实体污染和跨边界泄漏为零。
- No-answer False-positive Rate 不高于 Baseline。
- 离线 P95 延迟不超过 Baseline 的 1.5 倍。
- Live Total Tokens 相比 Baseline 增幅不超过 10%。
- 任意关键安全或隔离回归都会阻止进入 P1。

成本和 Live 延迟在 P0 中作为观测指标，但不要求必须改善。修改门槛时必须显式升级数据集版本并说明原因。

## 9. 校验与失败语义

遇到无效 Schema、无法解析的 Evidence、格式错误的 URI、不可能的时间线、无效替代关系或跨 Scope 引用时，数据集加载必须 Fail-closed。任何 Case 开始运行前，Gold Brain Fixtures 必须通过 Claim-to-evidence 和 Retraction 一致性检查。

如果某个实验组发生基础设施错误，该 Pair 标记为不可比较并计入 Error Rate；不得将其算作质量改善或回归。Live Judge 失败时保留原始答案和资源测量数据，但 Live Gate 判定失败。确定性错误不重试；Live 网络瞬态错误最多重试一次，并必须记录该次重试。

关键泄漏、撤回 Claim 或持久化 Prompt Injection 一旦命中，立即产生 Threshold Failure。评测输出必须清理凭证，不得记录 Authorization、真实 Prompt、私有路径或原始 Provider Response。

## 10. CLI 契约

```bash
go run ./cmd/brain-eval -input evals/brain/dataset.yaml -mode offline

go run ./cmd/brain-eval \
  -input evals/brain/dataset.yaml \
  -mode live \
  -repetitions 3 \
  -max-total-tokens 50000 \
  -max-total-cost-usd 2
```

默认输出为可读文本。`-format json` 输出 Case Results、Variant Summaries 和 Paired Comparison。输入无效、关键回归、门槛失败或预算超限时返回非零退出码。Live 模式必须显式配置模型和凭证，不得静默降级为离线模式。

## 11. 测试策略

包级测试覆盖 Schema 校验、时间顺序、Scope、匹配实验组对齐、Claim/Evidence 匹配、Forbidden Claims、Critical Gates、指标聚合、P95、Token、成本、错误和重试限制。CLI 测试覆盖参数、Text/JSON 输出、退出码和总 Token/成本限制。

Fixture 自校验确保所有 Evidence 都存在、每条 Gold Claim 都有支持、替代关系保持在同一 Project、Retraction 指向真实来源，并拒绝未授权的 URI 共享。测试使用 Fakes 和本地文件，不需要 Live 凭证。后续可以增加少量 HTTP Smoke Tests，但它们不属于默认 P0 Gate。

## 12. 完成标准

只有同时满足以下条件，P0 才算完成：

1. 所有离线 Go 测试通过；
2. 全部 24 个合成 Case 通过 Fixture 校验；
3. Candidate 通过质量门禁和关键安全门禁；
4. 至少一次受控 Live 配对评测在声明的预算内完成；
5. 报告明确区分确定性 Evidence 指标和 LLM Answer 指标。

P0 成功后可以开始只读 P1 Brain MVP 的设计工作，但不授权建设 Compiler、Dream Worker、自动发布或生产部署。
