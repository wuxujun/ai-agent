# Harness 使用指南

本指南对应 `cmd/harness-refine`、`internal/harness`、`internal/refine`、
`internal/improvement` 与编排器的 refinement hooks。命令从仓库根目录执行，
Go 版本以 `go.mod` 为准。先运行 `go run ./cmd/harness-refine --help` 查看入口。

## 能力与边界

| 层次 | 当前行为 |
|---|---|
| Harness 快照 | 捕获解析后的 `teams.yaml` 与已发现技能的 `SKILL.md` SHA-256，以内容生成快照 ID |
| 提案（propose） | 为指定团队生成 general/domain 候选、编辑 JSON 和含候选团队的 YAML |
| 配对执行（probe） | 对同一用例分别创建基线和候选 Multi-Agent 任务，收集行为与校验结果 |
| 合成（compose） | 排序、判定是否改进，合并有效编辑，可记录 adopted lesson |
| 多轮（run） | 串联上述阶段；上一轮合成结果成为下一轮基线，无改进则收敛 |
| 在线 refinement | 对命中灰度的 Legacy 任务应用该团队最新已发布且未撤回 lesson；终态后异步记录 assessment |

快照不保存技能正文、模型权重、外部检索数据或完整服务配置，不能单凭快照
恢复整个执行环境。技能目前只参与摘要，不属于可编辑目标。

可编辑目标为 `teams.<team>.<role>.<field>`：角色包括 planner、critic、executor、
verifier、researcher、writer；字段包括 system_prompt、prompt_name、langfuse_prompt、
prompt_label、prompt_version、provider、model、llm_scene、tools。
另可修改 routing 的 reviewed_min_plan_steps、reviewed_min_remaining_llm_calls、
reviewed_min_remaining_tokens。普通字段使用 `set`；工具使用 `add-tools` /
`remove-tools`，`after` 为工具名 JSON 数组。团队名含 `.` 不受目标语法支持。
安全边界、生命周期、运行时选择与 allow_research_high_risk_tools 不在可编辑范围。

## 1. 无模型调用的本地提案

```bash
hr_out="$(mktemp -d /private/tmp/ai-agent-harness.XXXXXX)"
go build -o "$hr_out/harness-refine" ./cmd/harness-refine
"$hr_out/harness-refine" propose \
  --teams-yaml teams.yaml --skills-root skills \
  --team software --editor mock --k 1 --out "$hr_out/proposal"
```

`software` 是仓库自带团队；可替换成输入 YAML 中已有团队。`--k` 必须至少为 1，
表示每个 scope 最多生成多少候选，合计最多 `2k`。mock 使用固定启发式，
当前最多 3 个 general 与 2 个 domain 候选；它适合验证流程，不保证质量提升。
仅 `propose --editor mock` 不需要模型凭据，后续 probe 的服务任务仍可能调用真实模型。
每次使用新输出目录，避免上一次的 adopted 文件混入本次结果。

检查输出：

| 文件 | 用途 |
|---|---|
| `base-snapshot.json` | compose 必需的原始快照 |
| `candidate-00.json` 等 | 每个候选的 EditSet，包含 target、op、before、after、reason |
| `manifest.json` | 基线团队、候选团队名及文件映射 |
| `teams.candidates.yaml` | 原团队加 `<team>-cand-00` 等候选团队，用于服务加载 |

提案不会改写源 `teams.yaml`，也不会把候选自动加载到服务。

## 2. 分阶段配对评测

需要一套专用评测服务，具有可用的 Multi-Agent LLM 配置、工具和可访问的
`evals/runtime-fixtures`。服务与 CLI 可使用不同工作目录，但数据集中的 workspace
必须能在服务端解析并通过工作区策略；跨目录或跨机器时改为服务端可访问的路径。
保持评测服务的 harness refinement 关闭，避免基线/候选受到额外线上 overlay 影响。

服务当前按工作目录及父目录寻找 `teams.yaml`，没有 `--teams-yaml` 服务启动参数。
把 `teams.candidates.yaml` 安装为该专用服务实际读取的 `teams.yaml`，再重载或启动服务。
使用现有专用服务时，可在确认路径后执行：

```bash
export AI_AGENT_API_KEY='<评测服务的 API key>'
hr_service_url='http://127.0.0.1:8088'
hr_service_teams='/absolute/path/to/eval-service/teams.yaml'
cp "$hr_service_teams" "$hr_out/teams.original.yaml"
cp "$hr_out/proposal/teams.candidates.yaml" "$hr_service_teams"
curl -sS --fail -X POST -H "X-API-Key: $AI_AGENT_API_KEY" \
  "$hr_service_url/api/config/reload"
```

重载需要管理员权限；probe 使用管理员 key，或 allowed_multiagent_teams 同时允许
基线与 manifest 中全部候选团队的租户 key。CLI 当前通过 `X-API-Key` 认证。

先执行一个用例，确认加载和权限正确，再在新目录执行完整数据集：

```bash
"$hr_out/harness-refine" probe \
  --service-url "$hr_service_url" --candidates "$hr_out/proposal" \
  --input evals/harness_regression.yaml --case harness-plan-rigor \
  --repetitions 1 --case-timeout 3m --out "$hr_out/smoke"

"$hr_out/harness-refine" probe \
  --service-url "$hr_service_url" --candidates "$hr_out/proposal" \
  --input evals/harness_regression.yaml --repetitions 2 \
  --case-timeout 3m --out "$hr_out/evaluation"

"$hr_out/harness-refine" compose \
  --probe-results "$hr_out/evaluation/probe-results.json" \
  --base-snapshot "$hr_out/proposal/base-snapshot.json" \
  --out "$hr_out/evaluation"
```

`--case` 是精确名称匹配，`--poll-interval` 默认 250ms，二者仅 probe 提供。
默认数据集有 8 个用例；每个候选、每个用例、每次 repetition 都各跑一次基线和候选，
总任务数为 `候选数 × 用例数 × repetitions × 2`，不会跨候选复用基线。
run 的启动提示按最多 `2k` 个候选列出配对任务数上界；实际数量还取决于候选过滤、
用例数量、提前收敛和错误退出，不是费用承诺。
工具创建唯一任务 ID，不自动删除任务。单个任务失败会记录到结果；probe 返回 0
仅表示报告已生成，必须检查报告中的 error、pass 和 supported。
数据集的 thresholds 会被解析，但 harness-refine 不执行 DAG 发布门禁。

评测结束恢复专用服务的原始配置并确认重载成功：

```bash
cp "$hr_out/teams.original.yaml" "$hr_service_teams"
curl -sS --fail -X POST -H "X-API-Key: $AI_AGENT_API_KEY" \
  "$hr_service_url/api/config/reload"
```

## 3. 一键与多轮运行

`run` 的 `--service-teams-yaml` 指向 CLI 本机可写、且专用服务实际读取的 YAML，
不是远程服务路径下载功能。它会临时覆盖该文件，每轮重载，正常返回时尝试恢复原文件
并重载。恢复文件或重载失败都会返回非零退出码。每次重载（包含恢复）默认最多等待
30 秒，可通过 `--reload-timeout 10s` 调整；零或负值会被拒绝，HTTP 重定向不跟随。

自动部署以同目录临时文件、Sync 和 Rename 提交，保留原文件权限；仅支持普通文件，
拒绝符号链接。`<teams路径>.harness-refine.lock` 独占锁阻止多个 CLI 同时修改同一文件；
`<teams路径>.harness-refine.original` 保存权限 0600 的原始内容。每次替换与恢复前会检查
文件身份、权限和内容；发现外部修改则拒绝覆盖，保留原始备份并返回失败。此锁只约束
配合它的 CLI，不能为任意外部编辑器提供原子 CAS，评测期间仍应停止其他配置写者。

正常恢复且重载成功后自动清理本次锁与备份；恢复失败保留备份。强制终止可能留下锁、
备份或已安装候选，不能保证恢复。重新运行时会拒绝覆盖遗留备份；操作人员应核对运行
配置、备份与候选，在专用服务中完成恢复及重载，再逐个处理明确的遗留文件。


```bash
cp "$hr_service_teams" "$hr_out/teams.before-run.yaml"
"$hr_out/harness-refine" run \
  --teams-yaml teams.yaml --skills-root skills --team software \
  --editor mock --k 1 --rounds 2 \
  --service-url "$hr_service_url" --service-teams-yaml "$hr_service_teams" \
  --input evals/harness_regression.yaml --repetitions 1 \
  --out "$hr_out/run"
```

`--rounds` 范围 1–5，多轮必须提供 `--service-teams-yaml`。
不提供该参数时，run 会输出加载提示后立即 probe，不会暂停等待人工操作。
需要人工部署时使用上一节的三个独立命令。

每轮输出在 `round-01/` 等目录；`iterations.json` 记录收敛与采用情况。
至少一轮采用时才生成 `teams.final.yaml`。`compose.json` 和 `report.md` 包含排序、
采用理由、合并来源及冲突。采用时另有 refined-edits.json、refined-snapshot.json、
teams.refined.yaml；未采用时不生成这些文件。

排序依次比较通过率、verifier-supported 率、失败工具数、总步数。
采用要求最佳候选相对它自己的配对基线严格提升通过率，或在通过率相同时提升 supported 率；
仅减少失败工具或步数不足以采用。多个胜出候选的编辑会合并，同目标冲突保留排序更高者。
合并组合没有在 compose 中单独联合评测，下一轮只是把它作为基线。
compose 的 `adopted` 表示离线采用，仍只记录候选；线上选用还需要下述 prepare、联合
probe、verify 和显式 publish。延迟与费用不影响 compose 排序，但受 verify 发布门槛约束。

退出码：0 成功；1 执行失败；2 用法错误；compose 的 3 表示未采用，run 将其视为正常收敛。
用 `go run` 时，非零程序退出码会由 Go 工具包装；脚本需要精确区分 3 时使用编译后的二进制。

## 4. LLM editor

把 `--editor mock` 改为 `--editor llm`，可同时传 `--editor-max-cost-usd 0.10`。
CLI 从标准配置加载 `llm.scenes.harness_editor`，未指定字段继承 `llm.gateway`
及默认 provider 配置；需要配置
模型、供应商凭据及对应单价。沿用已有 `llm.scenes` 结构添加场景即可，例如：

```yaml
llm:
  scenes:
    harness_editor:
      provider: openai
      model: "<你的可用模型>"
```

凭据通过标准环境变量或秘密管理提供。Langfuse 启用时，editor prompt 使用
`harness-editor` 名称和 `harness-candidate` 标签，失败时回落到内置提示词。

正费用上限启用调用前估算准入：要求输入、输出单价都为正且有限，按完整 system/user
提示词 UTF-8 字节、两份 schema 和 1024 framing 字节计算保守输入 token 上界，
并预留完整输出上限的费用；预留估算超过上限时不调用模型。输出最多 4096 tokens，
已有更小的调用配置或 context 输出限制继续生效，不会为适应预算自动缩小输出。
使用与 Brain compiler 相同的字节估算方法，可能比实际 token 用量保守很多。

启用上限后冻结 editor 场景配置，仅尝试一次，不继承业务任务的条件路由，不自动重试或
回退到另一个场景。模型失败、限额截断导致无效 JSON 或 usage 缺失/不一致/超出预留范围，
提案都会失败；成功返回后仍按此次固定配置的单价核验费用。零单价在当前表示中无法区分
免费与未配置，因此带费用上限的 editor 一律拒绝零单价；需先审核准确单价再启用。

0 保留原有路由、重试、回退及输出设置，不执行此费用准入。负数、NaN、无穷值会被
CLI/启用的配置校验拒绝。该机制只约束单次 editor proposal，不是跨 assessment/租户共享
账本，也不覆盖 probe 中所有团队任务的费用。配置单价、token 统计及供应商的限额语义
决定估算有效性，不能据此保证最终账单或撤销已产生费用；失败尝试也可能计费。

Gemini 用量统一按输入 `promptTokenCount + toolUsePromptTokenCount`、输出
`candidatesTokenCount + thoughtsTokenCount` 统计；缓存 tokens 已包含在 prompt 中，
不会重复相加。保留供应商 `totalTokenCount` 原值，缺失或不一致的 total 不会被自动补成
可通过预算核验的数值。此口径用于结构化文本/视觉、planner 流式和 ADK 路径；因此思考
tokens 也占用 editor 的输出预留。费用仍按仓库配置的输入/输出单价估算，没有单独模拟
供应商的缓存折扣或其他特殊计费规则。

## 5. Lesson、线上灰度与撤回

先将离线结果记录到独立目录，例如在 compose 或 run 中增加
`--lessons-dir "$hr_out/improvement"`。记录生成的 lesson 状态为 `candidate`；旧版已有 lesson 也按候选处理，不自动授予发布状态。
状态顺序为 `candidate` → `validated` → `published`，撤回后为 `retracted`。
缺少 `baseline_active_team` 等新基线元数据的旧记录应重新 propose/compose；不要直接改写
已有内容寻址文件或补字段沿用旧 ID。来自 run 后续轮次的 lesson，prepare/verify 必须
使用该轮自己的 `base-snapshot.json`，不能替换成第一轮快照。
线上灰度只读取该团队最新的已发布且未撤回 lesson；恢复 Trace 中的选择也受该门槛约束。

先用完整 lesson ID（list 输出简写；完整 ID 在文件名或 JSON 中）生成联合候选：

```bash
lesson_id='<完整的 64 位 lesson ID>'
joint_out="$hr_out/joint"
"$hr_out/harness-refine" lessons prepare \
  --lessons-dir "$hr_out/improvement" --id "$lesson_id" \
  --base-snapshot "$hr_out/proposal/base-snapshot.json" --out "$joint_out"
```

prepare 要求新输出目录，核对原始快照及应用后摘要。lesson 的团队标签必须与每个
`teams.<team>...` 编辑目标的团队分段完全相等；不同团队、前缀相似团队及带点产生歧义
的标签都会被拒绝。此检查同时用于验证、发布、读取已发布结果及运行时 overlay；
跨团队草稿可以保留，但不能用只加载单个团队的联合评测证明其完整编辑集。
prepare 生成只包含这一完整合并方案的
候选清单。将其 `teams.candidates.yaml` 加载到专用评测服务（按第 2 节备份及恢复），
随后使用相同的受审数据集运行完整配对评测，不能仅运行 `--case` 子集：

```bash
"$hr_out/harness-refine" probe \
  --service-url "$hr_service_url" --candidates "$joint_out" \
  --input evals/harness_regression.yaml --repetitions 2 --out "$joint_out"
"$hr_out/harness-refine" lessons verify \
  --lessons-dir "$hr_out/improvement" --id "$lesson_id" \
  --base-snapshot "$hr_out/proposal/base-snapshot.json" \
  --probe-results "$joint_out/probe-results.json" \
  --input evals/harness_regression.yaml --repetitions 2 \
  --max-latency-ratio 1.2 --max-cost-ratio 1.2 --max-task-cost-usd 0.10 --max-llm-calls 20
"$hr_out/harness-refine" lessons publish \
  --lessons-dir "$hr_out/improvement" --id "$lesson_id" --reason '完整联合复验及预算审核通过'
```

verify 绑定精确编辑集、基线、数据集摘要、完整用例与重复次数；重新计算用例通过情况。
候选每个用例须完成并通过，不能降低已有 verifier 支持或增加失败工具数，且至少改善
一项通过/支持结果；每对延迟和估算费用必须在比例门槛内，另检查单任务估算费用及调用数。
费用读取任务 API 的 `llm_estimated_cost_usd`，报告新增可空 `estimated_cost_usd`；缺失费用、
非有限/负数费用、发生 LLM 调用但估算为零都会拒绝验证。必须配置准确模型单价；这些是
事后估算的回归门槛，不是供应商账单核对，也不是调用前硬预算。阈值可在审核后显式调整。
旧报告缺数据集摘要或费用证据，不能直接发布。verify 写入 validations，publish 写入
publications，均绑定完整 lesson ID 并保留报告摘要；这些是受控目录里的操作证据，
不是外部签名权威，仍须保护该目录写权限并人工审核数据集与实际服务加载的配置。

上线前把审核通过的整个存储目录（含 lessons、validations、publications、retractions）
纳入线上受控目录。运行时核对当前基线/技能摘要及结果摘要；配置或技能漂移时回落基线，
应重新提案、验证和发布。只复制 lesson 文件不会启用该结果。
输出与 lesson 可包含提示词、任务标识及运行证据，
应留在受控目录；仓库已忽略 `workspace/`、`data/`、`var/improvement/` 和 `runs/`。
忽略规则不改变现有文件，也不会自动移除已跟踪文件。

lesson 查询使用进程内按团队排序的轻量索引（ID、团队、观察时间及发布候选集合），
短生命周期 Store handle 共享同一规范化根目录的索引。最多保留 32 个根目录；每个根目录
的元数据仍为 O(N)。首次读取、失效及最长 5 秒到期时需要 O(N) 扫描/重建，冷读取仍可能
较慢；选中条目的 lesson、内容摘要、发布绑定和撤回标记均从磁盘现场读取，不缓存授权。
发布后已经持有的 overlay 仍按原契约执行，撤回不追溯中断在途任务。

Record（包括同 ID 重录）、Validate、Publish 和 Retract 的托管写入会原子更新根目录的
`.lesson-index-generation`。该标记是非秘密的 0644 随机变更代号，不是凭据或签名。
不同 Store/进程会读取此代号，并核对根/状态目录身份和修改时间；即使目录 mtime 合并，
托管写入成功后的新查询也会重建索引。变更代号写入失败时操作返回错误，但先前的状态文件
可能已经落地，需核对并重试；缓存不承担多文件事务或外部权威证明。

旧只读目录没有变更标记仍可读取。直接在文件内修改非选中元数据且保留目录 mtime，最多
5 秒后重建；因此应使用 CLI/Store 的托管操作更新状态。被选中条目的内容/发布记录篡改会
立即报错，已撤回条目会被跳过；变更标记损坏、无权限或重建期间持续变化时拒绝使用旧索引，
线上继续回落基线。索引本身不持久化，不需要后台 watcher；取消的查询不会无限等待重建锁。

在 `config.yaml` 的现有 multiagent 下合并配置：

```yaml
multiagent:
  harness_refine:
    enabled: true
    canary_percent: 5
    max_candidates: 3
    lessons_dir: "data/improvement"
    assessment_editor: "mock"
    assessment_max_cost_usd: 0
```

以上是启用示例，仓库默认 enabled=false、canary_percent=0。
灰度比例范围 0–100，启用且比例非零时 max_candidates 必须为 1–10，lessons_dir 必填。
max_candidates 对应 editor 每个 scope 的上限，并非候选总数。
配置支持重载；环境变量前缀为 `AI_AGENT_MULTIAGENT_HARNESS_REFINE_`，后缀分别为
ENABLED、CANARY_PERCENT、MAX_CANDIDATES、LESSONS_DIR、ASSESSMENT_EDITOR、
ASSESSMENT_MAX_COST_USD。

灰度按 team + task ID 确定性分桶，仅适用于 Legacy；DAG 团队或命中 DAG 灰度的任务
保留基线。每次只应用最新一条已发布 lesson，不会自动串联合并历史 lesson。
lesson 在单任务内存 overlay 中应用，Trace action 为
`harness_refine_selection`，不会改写全局 teams.yaml。读取或应用失败时回落基线并记录日志。
没有 lesson 时，命中灰度的任务仍可在终态后生成 assessment。
assessment 由每个 Engine 的延迟启动 worker 管理：最多 2 个并发、16 个排队，单项从入队
起共用 30 秒 context 期限，包含排队时间。队列满或停机后直接跳过并记录安全日志；
过期排队项不会开始 proposal，不影响业务任务结果。任务标识、步数、结果和有效团队
配置在入队前捕获，后续 task 修改或 overlay 清理不会改变已排队的快照。当前这些容量
和期限是固定内部默认值，没有新增运行配置或热重载开关。

服务退出（包括监听失败）时先停止 admission、取消运行中的 assessment 并释放排队项，
在主任务排空后等待 worker，再关闭审批后台资源；等待超过服务 shutdown 期限时返回
错误。嵌入式 Engine 调用者应在退出时调用 `StopAssessments()`，随后以有期限的 context
调用 `WaitAssessments(ctx)`；停止后同一 Engine 不会重新启动 assessment。取消依赖底层
操作遵守 context，不能强杀忽略 context 的回调或中断所有文件 I/O；不承诺强制终止时间。
lesson 扫描现会在入口及迭代间检查 context，提案前后也复查取消。

assessment 是观察和候选建议，不会自动 probe 或采用；当前没有直接导入 assessment 的 CLI，
需据此人工调整数据集或提案，再执行离线流程。`assessment_editor=llm` 会为命中任务增加
模型调用；`assessment_max_cost_usd > 0` 使用第 4 节的调用前预留估算、单次有界调用及
调用后 usage 核验。默认上限 0 不执行费用准入，默认 mock 不产生 editor 模型费用。

```bash
"$hr_out/harness-refine" lessons list \
  --lessons-dir data/improvement --team software --limit 20
"$hr_out/harness-refine" lessons assessments list \
  --lessons-dir data/improvement --team software --limit 20
"$hr_out/harness-refine" lessons retract \
  --lessons-dir data/improvement --id '<完整的 64 位 lesson ID>' \
  --reason '复验发现回归'
```

list 只显示 ID 前 12 位；完整 ID 在 `lessons/<id>.json` 文件名与 JSON 中。
撤回保留原记录，并写入 retractions，不删除文件；后续选择忽略已撤回 lesson，
已持有 overlay 的执行不会被即时中断。恢复任务会尝试重用 Trace 中的 lesson；
不可用或已撤回时回落基线。紧急停止新选择可将 enabled=false 或 canary_percent=0 后重载。

## 本地验证

```bash
go test ./internal/harness ./internal/refine ./internal/improvement ./cmd/harness-refine
go test ./...
```

以上测试使用本地 fixture 或假服务，不需要真实模型凭据。它们不等价于真实模型质量、
线上费用或 HA 验收。运行本文 probe/run 示例会创建服务任务并可能产生真实模型费用。
