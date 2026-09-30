# Brain 只读 MVP P1 设计

日期：2026-09-02
状态：已批准设计
相关分析：`records/s260827.md`
P0 证据：`records/brain-eval-p0.md`

## 1. 目的

P1 将已经通过验证的人工 Brain 实验转化为接近生产形态的只读能力。系统把
符合条件的 Task、Trace、Memory 和撤回记录编译为 Project 级 Brain Wiki，
生成完整的不可变 staging 快照，执行确定性校验，并且只允许管理员通过显式
CLI 命令发布。

Brain 继续作为 `eino`、`step` 和 `multiagent` 共享的正交上下文层。P1 不增加
`orchestrator.mode: brain`。

长期作用域键为：

```text
tenant_id + brain_project_id
```

Session ID 继续表示来源和时间顺序，但不限制 Project 级长期召回。

## 2. 已批准决策与非目标

P1 采用以下决策：

- 发布仅通过 CLI 完成，HTTP 保持只读；
- Task 显式携带 `brain_project_id`，不从 Workspace 路径推导身份；
- 发布完整不可变快照，不进行逐页修改，也不使用运行时 Overlay；
- Gemini 可以通过现有 LLM 抽象生成 Proposal，并读取 `GEMINI_API_KEY`，但
  Gemini 永远没有发布权限；
- 每次发布都必须通过确定性校验和显式人工操作；
- Brain 页面复用现有 `wiki_search`、`wiki_fetch` 和图检索工具，不注册重复的
  Brain 工具。

P1 不包含 Dream/后台调度、增量编译、Agent 自主写入、Connector 刷新、低风险
自动发布和物理介质擦除自动化。这些能力需要单独设计和批准。

## 3. 架构

```text
Task Store / Trace / Memory / Retraction
                  │
                  ▼
           Brain Source Reader
                  │
                  ▼
        Normalizer + Claim Builder
                  │
                  ▼
        Immutable Staging Snapshot
                  │
                  ▼
 Validator → Proposal Manifest → CLI Publish
                                      │
                                      ▼
                         Published Brain Snapshot
                                      │
                                      ▼
                    Existing wiki_search/wiki_fetch
```

### 3.1 `ProjectResolver`

`ProjectResolver` 使用租户 allowlist 解析已经认证的
`TenantID + BrainProjectID`，返回受管存储键、公开 Wiki Space 和策略快照。
未知 Project、跨 Tenant 引用、非法 ID 和缺失配置全部 fail-closed。Workspace
路径永远不充当 Project 身份。

### 3.2 `SourceReader`

`SourceReader` 从 Store 获取有界来源快照。它只接受绑定到相同 Tenant 和 Brain
Project 的 completed Task，并要求 Answer Audit 存在且结果为
`publishable=true`。failed、partial、无审计以及旧版未分配 Project 的 Task
默认排除。

Memory 和 FinalAnswer 可以用于发现候选主题，但不能证明 Claim。每条 Claim
必须引用成功 Trace 中经过现有 Evidence Filter 的证据。任何模型调用之前必须
先应用 Retraction。

### 3.3 `Compiler`

Compiler 首先生成确定性、经过脱敏的 Evidence Record。可选的 Gemini 综合阶段
随后提出 `concepts/`、`entities/`、`projects/` 和 `sources/` 页面。每条 Claim
记录证据引用、观察时间、置信度以及 supersession/retraction 状态。`_index.md`
是紧凑的派生缓存，不是主要来源。

### 3.4 `Validator`

Validator 检查 Markdown/frontmatter、内容边界、Claim-to-Evidence 覆盖、证据
存在性、Project 隔离、撤回复现、链接完整性、Prompt Injection Findings 和
全部哈希。任一硬门禁失败都会使快照保持不可发布状态。

### 3.5 `SnapshotRepository`

Repository 管理不可变 staging/release 目录、manifest、内容哈希、父版本和
`CURRENT` 指针。publish 与 rollback 使用 expected-current CAS，并在同一文件
系统内原子替换指针。旧的已验证 release 保留，除非其被撤销。

### 3.6 `BrainWikiProvider`

Provider 只读取已经验证的 release，并适配到现有 Wiki Client/Tool 契约。它不
提供写入能力。Candidate Cache 按 Task、Tenant、Project、Snapshot 和 Corpus
完整隔离。

## 4. 配置与 Task 契约

Brain 默认关闭：

```yaml
brain:
  enabled: false
  root: ./data/brain
  compact_index_max_bytes: 4000
  compiler:
    provider: gemini
    model: gemini-3.5-flash-lite
    max_input_bytes: 200000
    max_output_tokens: 12000
    max_cost_usd: 0.25
```

每个 Tenant 显式声明允许使用的 Project 及其公开 Wiki Space：

```yaml
api:
  auth:
    tenants:
      tenant-a:
        brain_projects:
          atlas:
            wiki_space: brain-atlas
```

创建 Task 时可以提供 `brain_project_id`。持久化 Task 还保存
`BrainProjectID`、`BrainSnapshotID` 和 `BrainConfigDigest`。所有 Store 后端都
必须完整往返这些字段。缺少 Project ID 表示不加载 Brain；未知或未授权的
Project ID 会拒绝创建 Task。

## 5. 快照与来源模型

受管目录结构为：

```text
data/brain/<tenant-key>/<project-id>/
├── CURRENT
├── staging/<snapshot-id>/
│   ├── wiki/
│   ├── evidence.jsonl
│   └── manifest.json
└── releases/<snapshot-id>/
    ├── wiki/
    ├── evidence.jsonl
    └── manifest.json
```

`tenant-key` 是 Tenant ID 的稳定哈希，用于阻止路径穿越并避免在存储路径中
暴露租户名称。Project ID 使用严格的 slug 语法。

Manifest 记录 Snapshot 与 Parent ID、Tenant/Project 身份、来源截止点、来源
ID 和哈希、Retraction Watermark、模型和 Prompt 版本、配置摘要、Token/成本、
文件哈希、校验结果及 expected current release。Evidence Record 使用不可变的
`brain-evidence://<tenant>/<project>/tasks/<task>#trace/<step>` 标识，并且只包含
审计所需的有界、已脱敏内容。

最终答案引用完整的 `wiki://<brain-space>/<kind>/<slug>` 页面 URI；页面内 Claim
再链接 Brain Evidence Record，形成“答案 → 页面 → 来源”的两级证据链。原始
Brain Evidence URI 不作为最终答案引用。

## 6. CLI 与发布流程

CLI 接口为：

```text
brain-compile build --tenant <tenant> --project <project>
brain-compile inspect --tenant <tenant> --project <project> --snapshot <id>
brain-compile verify --tenant <tenant> --project <project> --snapshot <id>
brain-compile publish --tenant <tenant> --project <project> --snapshot <id> --expected-current <parent-id>
brain-compile rollback --tenant <tenant> --project <project> --to <release-id> --expected-current <current-id>
brain-compile status --tenant <tenant> --project <project>
```

`build` 固定来源截止点和精确来源哈希，应用 Retraction，规范化并脱敏 Evidence，
在配置的调用、Token、成本、超时和输出限制内调用 Gemini，渲染完整 staging
Wiki 并写入 Proposal Manifest。模型输出始终是不可信数据。

`verify` 执行全部确定性门禁。`publish` 重新验证完整性，检查当前 Release CAS
和最新 Retraction Watermark，在同一文件系统内把验证后的目录移动到 releases，
然后原子更新 `CURRENT`。任何不一致都要求重新 build。`rollback` 只接受已经
验证且未被撤销的 Release，并使用相同 CAS 规则。

build 和 verify 失败只影响 staging。指针更新失败时，旧 Release 继续有效。
启动时忽略不完整 staging，绝不自动恢复或发布。

## 7. 前台检索与快照固定

`wiki_search` 新增可选 `corpus` 参数：

- `wiki` 只查询现有 Wiki，并继续作为默认值；
- `brain` 查询 Task 已固定的 Brain Release；
- `all` 分别查询两者，再执行有界、确定性合并。

`wiki_fetch` 继续只接受同一执行作用域内先前搜索产生的 Candidate ID。Cache Key
绑定 `task_id`、`tenant_id`、`brain_project_id`、`brain_snapshot_id` 和
`corpus`。

首次执行之前，Runtime 解析 `CURRENT`，并将 Snapshot ID 和 Brain 配置摘要持久
固定在 Task 上。之后的每个步骤和恢复都使用该 Release。已固定 Release 缺失、
损坏、被撤销或与策略不兼容时，任务 fail-closed。只有新 Task 才采用新发布的
Release。

Planner 初始只获得有界紧凑索引、已固定 Snapshot ID 和 Corpus 使用规则。完整
页面通过 Search → Fetch 渐进加载。`eino`、`step` 和 `multiagent` 使用相同的
固定逻辑与工具上下文。

只读页面 API 允许当前 Tenant 的普通 Wiki Space，以及其 Project allowlist 中的
Brain Space；它不提供 build、publish 或 rollback 路由。

## 8. 安全、撤回与失败策略

Evidence 在进入模型前进行脱敏和大小限制。现有 Prompt Injection 与 Secret
Detector 在综合前以及生成页面后各运行一次。指令覆盖内容、凭据、私有路径、
跨作用域 URI、缺失引用和不支持的链接都会导致校验失败。日志、错误、指标和
Manifest 不得包含 Prompt、原始模型响应、凭据或完整 Evidence 正文。

Project 级追加式 Retraction Ledger 的优先级高于快照固定与 rollback。每次 fetch
都查询实时 Ledger。新 Retraction 会使受影响的 Candidate Cache 失效，即使运行
中的 Task 固定在旧快照，也会阻止相关 Claim；包含该来源的 Release 标记为
`revoked`。被撤销 Release 不能成为 `CURRENT` 或 rollback 目标，下一次 build
必须删除相应 Claim。

P1 保证被撤回数据不可检索、不可引用，也不可通过 rollback 恢复。物理介质擦除
属于 P1 之外的显式保留/销毁流程，不能用逻辑撤销冒充物理删除已经完成。

允许存在并发 build，但 publish 按 Tenant/Project 串行并使用 CAS。读取必须拒绝
符号链接逃逸、路径穿越、过大文件和配置根目录外的 Release。禁止跨文件系统
发布。

## 9. 可观测性

P1 增加有界指标，覆盖 compile 次数/耗时、publish 结果/冲突、Snapshot Age、
Retraction Block、Search/Fetch 次数和 Cache Hit。标签仅限 outcome、corpus、
provider 等低基数值；禁止把 Tenant、Project、Task、Snapshot、Prompt 或内容
作为标签。

结构化日志在可用时保留 Request/Trace ID，并且只输出脱敏 ID、状态转换、哈希、
计数、耗时和错误类别。

## 10. 验证与验收门禁

测试必须覆盖：配置校验及拒绝 Reload 后保留旧配置；SQLite/Postgres/Redis Task
字段持久化与迁移；Resolver 隔离；来源准入；确定性规范化；校验失败；CLI 的
build、verify、publish、rollback、并发 CAS、崩溃注入和符号链接边界；所有执行
模式的快照固定和恢复；Cache 作用域；只读 API 授权；Retraction 优先级；指标
基数。

自动化测试使用 Fake Gemini Client，不要求凭据，也不产生费用。只有在明确批准
后，Live Eval 才能读取 `GEMINI_API_KEY`。

发布门禁要求：

- Brain 关闭时行为零变化；
- Scope Leak、Entity Contamination、Retraction Recurrence、Prompt Injection
  Recurrence 和 No-answer Hallucination 均为零；
- 匹配式 Live Answer Accuracy 增量至少为 `+0.10`；
- Brain/Baseline Token Ratio 不高于 `1.20`，`1.10` 保持为非阻塞优化目标；
- Brain/Baseline P95 Latency Ratio 不高于 `1.20`；
- `go test ./...`、相关 Race Test、`go vet ./...` 和 `git diff --check` 全部通过。

P1 成功只授权单独讨论 P2 Dream Proposal Worker 的设计，不授权后台调度或自动
发布。
