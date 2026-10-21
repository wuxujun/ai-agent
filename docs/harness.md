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
| 在线 refinement | 对命中灰度的 Legacy 任务应用该团队最新未撤回 lesson；终态后异步记录 assessment |

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
并重载。恢复失败只打印错误，不改变返回码；强制终止进程也不能保证恢复，操作前保留备份。
评测期间不要并发修改该服务的团队配置。

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
合并组合没有单独联合评测，下一轮只是把它作为基线；上线前应另做复验。
延迟与费用不是当前 compose 的采用门槛。

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

费用上限是调用完成后按 token usage 与配置单价计算的检查，超过后提案失败；
它不是调用前硬预算，不能撤销已产生的费用，单价未配置也不能据此限制真实账单。
0 表示不检查。该选项只约束 editor，不覆盖 probe 中所有团队任务的费用。

## 5. Lesson、线上灰度与撤回

先将离线结果记录到独立目录，例如在 compose 或 run 中增加
`--lessons-dir "$hr_out/improvement"`。记录即成为未撤回 lesson，当前没有额外发布状态。
线上灰度开启后会读取指定目录中该团队最新的 lesson，因此先在隔离目录复验，
再把经过审核的记录纳入线上目录。输出与 lesson 可包含提示词、任务标识及运行证据，
应留在受控目录；仓库的 `workspace/`、`data/` 已被忽略，默认示例 `var/improvement`
和 `runs/` 并未被 `.gitignore` 排除。

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
保留基线。每次只应用最新一条 lesson，不会自动串联合并历史 lesson。
lesson 在单任务内存 overlay 中应用，Trace action 为
`harness_refine_selection`，不会改写全局 teams.yaml。读取或应用失败时回落基线并记录日志。
没有 lesson 时，命中灰度的任务仍可在终态后生成 assessment。
assessment 是观察和候选建议，不会自动 probe 或采用；当前没有直接导入 assessment 的 CLI，
需据此人工调整数据集或提案，再执行离线流程。`assessment_editor=llm` 会为命中任务增加
模型调用，费用检查同样在调用后执行；默认 mock 不产生 editor 模型费用。

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
