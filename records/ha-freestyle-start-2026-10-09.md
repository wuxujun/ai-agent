# Freestyle HA 实施记录（2026-10-09）

## 当前结果

已开始执行[Freestyle HA 实施方案](../docs/阶段3.3-Freestyle-VM实施方案.md)。固定源码为 `9aeb24b54df2db6888222502d4125fafc9a9df6e`，从干净工作区创建仓库外的隔离检出；完成发布回归、双架构重建、操作 SDK 安装与四角色私密资产生成。

用户已明确确认“免费计划，CPU限制200小时”。套餐依据为用户确认；200 是月度 vCPU·小时总额，实际剩余额度未知。已创建专用 VPC、四台 4 vCPU/8 GiB/32 GiB VM 与 31 条限定规则，未配置公网入站。每台单次及累计运行最多 6 小时，四台最多 96 vCPU·小时；额度耗尽依靠 Free 平台硬限制停止，不升级、绑卡或启用付费超额。详情见[仅免费策略](ha-freestyle-free-only-2026-10-09.md)。

四角色初始化完成，A/B 双向 NFS 哨兵读取及重挂载验证、两端 root 写入拒绝均通过；目标专用 PostgreSQL/Redis 的七项存储契约在竞态检测下全部通过，含子用例 28 项，没有缺失或 SKIP。A/B 部署同一干净 `9aeb24b` 包，服务健康且服务器、配置摘要一致；OTLP collector 指标采集正常，Prometheus 两个 target 均 `up=1`。`smoke-003` 的 12 条任务全部通过，A/B 各 6 条，P95 570ms。`soak-001` 应用户“如无异常，是否可停止当前任务了”的要求，于 2026-10-10T05:37:46Z 主动停止，实际运行约 31 分 40 秒：64/64 条任务通过，A/B 各 32 条，P95 570ms。逐任务结果和汇总已导出，退出码 143 为主动 SIGTERM，`window_complete=false`；不能计作完整一小时验收。阶段 3.3 仍未完成，`runtime=legacy`、灰度 0%、`full_ha_acceptance=false`。

根目录 `.env` 只读取 Freestyle 字段，保持忽略、未跟踪和 0600，未将操作端密钥上传 guest。2026-10-09T14:11:32.488Z 首次认证后的空资源结果保留为历史；创建前再次查询为空，不能把该旧查询描述为当前资源状态。

2026-10-10T07:27:50Z 资源清单显示四台 VM 的 `automaticRestart=true`；A/B 当时 paused，Control、Storage 正在运行。旧暂停核对脚本因有效上限字段不匹配而停止，只留下状态摘要，没有更改资源。用户指出自动启动并建议用完直接删除后，按专用 ID/slug/metadata 及 Free 预算上限二次核验，确认四台都 `automaticRestart=true`。2026-10-10T07:28:09Z 开始删除四台已结束测试的 VM，07:28:10Z Freestyle inventory 确认四个 VM ID 全部消失。Control/Storage 在此期间仍运行；A/B 原为 paused。云端 VM 和其磁盘数据已永久删除，未提高运行预算、未恢复负载。此前的暂停状态不再适用。

平台报告本批累计计算量约 50.008 vCPU·小时，本批运行上限内剩余约 45.992 vCPU·小时；这两个数均不能用于断言账号月度剩余额度。四台 VM 磁盘已随 VM 删除；专用 VPC 仍存在，CIDR 为 `10.77.34.0/24`；删除 VM 后按该 VPC 复核到 0 条专用防火墙规则。VPC 本身未删除。

数据库连接采样 50 次、零采样错误，观察到峰值 4（A/B 各 2），上限 150；采样只覆盖部分窗口，不能宣称整个窗口绝对峰值。结束前 Prometheus 两个 target 均 up=1，Legacy observed 指标存在；0% 门禁结果 HOLD，缺少 DAG 成功样本、可比 P95 及审批/Replan 人工复核。

完整非敏感摘要见[结构化记录](ha-freestyle-start-2026-10-09.json)、[在线负载及契约证据](ha-freestyle-load-2026-10-09.json)、[验收状态](ha-freestyle-acceptance-2026-10-09.json)。

## 已完成的检查

- 全仓 `go test -p=4 ./... -count=1 -timeout=5m`：49 个含测试的包通过，退出码 0。使用既有原生 Go 1.26.8 arm64、`GOTOOLCHAIN=local`；测试需要本机 loopback，已获沙箱外执行授权。
- 最新 Freestyle 操作工具 Node 回归 12 项、Python 回归 6 项通过，包含 Free 预算保护、NFS 监听和只读工作目录下日志路径回归。
- 从同一干净提交构建 Linux/amd64、Linux/arm64 两包，不使用 `--allow-dirty`；每包 12 项 SHA256 校验通过。六个二进制均核对 Linux、对应架构、`CGO_ENABLED=0`、同一 VCS 提交和 `vcs.modified=false`。
- 安装 `freestyle@0.2.16` 于仓库外，禁用安装脚本，保留 lockfile 与摘要。显式使用 `/opt/homebrew/bin/node` 22.22.0，避免隔离目录 shell 选择旧 Node 16。
- 独立 prefix `ai-agent-ha-20261009-105jkxi`、私网 `10.77.34.0/24` 已实际创建；服务端返回 A/B 硬宿主机反亲和与四台有效运行上限，初始 29 条规则已核对，指标修正新增 Control 到 A/B 的两条私网 9464 规则，共 31 条。
- 新生成的秘密文件为 0600、四角色资产目录为 0700；Storage PostgreSQL/Redis 与 Control Prometheus、离线 stub 已运行，配置明文和密钥不进入记录。
- 七项目标存储契约使用 Control Linux/amd64 Go 1.26.8、干净源码 `/srv/ha-source-clean`，运行窗口为 2026-10-10T03:27:29Z 至 03:31:58Z，退出码 0，全部实际执行。

此前 `fbec156` 的本地七项真实存储契约证据保留为历史记录；本轮目标契约明确对应 `9aeb24b`。未重新运行额外全仓竞态或 vet，不能沿用旧证据改标当前提交。

Storage guest 内核没有 nfsd，改用 Ubuntu 官方包 Ganesha/VFS，限定私网地址、三个客户端及 root_squash，实际双向 I/O 通过。Prometheus 初始错误采集 JSON `/api/metrics`，已改为 A/B 本机固定 0.162.0 collector 的 `/metrics`。首次服务部署因专用工作目录只读、默认日志路径相对而失败；日志改为 `/opt/ai-agent/logs`，不放宽 systemd 隔离。macOS 源码 tar 的 AppleDouble 元数据导致 Git 检出检查失败，改用禁用扩展属性的新归档与新目录；旧目录保留，没有批量删除。

在线负载失败记录均保留：`smoke-001` 为 12/12 创建 HTTP 500，专用 WorkingDirectory 使 workspace 位于应用根目录外；改回 `/opt/ai-agent`，以只读 bind 挂载测试 Team。`smoke-002` 为 12/12 未完成，最小配置未包含 `llm.api_key`，导致 Viper 解码遗漏环境 token、stub 返回 401；添加空字段后仍由私密环境文件注入 token。两节点最终配置 SHA256 为 `a94ab1039b55c6d74b7fd8182b0a4fb46009fc7bae87b3465926b725c9e4cb63`，服务内部 Team SHA256 为 `fc7fd580ffcbf5fe1b69a0e754833c24b7055bda4992f106e0ad6aefbed8c3fc`。`smoke-003` 于 2026-10-10T05:03:51Z–05:05:52Z 通过，底层 `window_complete=false`，不能计作一小时窗口。运行时样本如实记为 `unknown:12`。

Docker Compose 临时容器曾消耗 `bash -s` 的后续 stdin；入口已增加 `-T` 与 `</dev/null`，实际服务运行状态另行核对。原失败配置、日志和每次 run 目录均保留。原始日志只存私有目录，仓库仅保存脱敏结果与文件摘要。

## 执行目录与发布身份

私有执行根目录：

`/private/tmp/ai-agent-ha-freestyle-20261009.105jkxi_`

| 发布包 | 目录（相对执行根目录） | Server SHA256 |
|---|---|---|
| Linux/amd64 | `bundle-linux-amd64` | `7b06e2b0df0c08fbff8ad8db6eacb447510d6abaea6b23ae5bf20f983e86ed7c` |
| Linux/arm64 | `bundle-linux-arm64` | `dc7521f9c7744488951d05d7725540953c4b2d3e8f568cff6f509f206080dea0` |

`source` 是固定干净检出；`kit` 保存原始 SDK、lockfile 和历史操作工具副本。预算保护加入后，创建资源必须使用 `free-only-tools/deploy/ha/freestyle` 中的最新创建器；`source` 中的旧创建器不能用于实施。继续保持完整的 `deploy/ha/freestyle` 路径布局及仓库输出保护。已忽略的 `node_modules` 指向仓库外 SDK；固定 Git 检出仍干净。上传 guest 的脚本包不得包含 node_modules 或操作端密钥。

`topology.json`、`plan.json`、`bundle-verification.json` 与 `full-suite.log` 位于执行根目录；`secrets.json` 和 `assets` 含随机测试凭据，仅在私有目录保存。**禁止整包提交、上传到无关节点或打印目录内容。** 临时目录可能被系统清理，续接时重新核对文件存在性、权限和发布摘要。

准备时 npm 首次因沙箱代理连接被拒绝，授权重试后完成安装；从独立 kit 调用 render 时路径保护拒绝输出，随后从固定源码布局生成成功。构建期间 Go 尝试写入沙箱外模块 stat cache 被拒绝，但两次构建均退出 0，发布清单与所有二进制身份随后已独立校验。没有降低路径保护、修改系统工具链或删除文件。

## 续接顺序

1. 使用既有 `resources/state-*.json` 中的 VM ID 续接；禁止重新执行创建器或重复建同一环境。只读取根目录 `.env` 的 Freestyle 字段。
2. 四台测试 VM 已删除，原 ID 仅供审计，不能续接或恢复；专用 VPC 尚存但没有关联专用防火墙规则。若继续 HA 工作，须重新准备部署资源并重新核验免费额度。通用 `observe-load.mjs` 尚需补充暂停后延时清单核对。
3. `smoke-003` 已通过；`soak-001` 已主动停止并导出，不能继续累加为一小时。再次获准续接后，恢复所需服务、复核健康与监控，以新 run 完整执行至少一小时并保存证据；不能重复启动相同 run。
4. 基础夹具仅提供 read_file，不足以产生审批、Replan 或重复副作用证据；先准备并复核专门夹具，再执行独立审批和故障场景。
5. 所有独立 HA 断言及人工复核通过后才进入 20%、50%、100% 观察窗口；不将基础负载通过写成完整 HA 验收。
6. 本轮已导出证据并删除四台测试 VM；原磁盘不可恢复，私密测试证据仍留在本机仓库外。专用 VPC 仍存在，关联专用防火墙规则为 0 条。后续重启 HA 实施需重新创建资源、核验额度并重新设置 `automaticRestart=false` 或采用不会自动恢复的生命周期配置；不提高上限或启用付费续跑。

完整 HA 验收及灰度放量仍待执行；已通过的目标环境检查按实际结果分别记录。


2026-10-10T07:28:10Z 追加清理：因资源清单确认四台 VM 均开启 `automaticRestart`，按用户建议删除四台专用测试 VM。删除前完整负载报告、逐任务 JSONL、worker 状态和门禁结果已保存在私密执行目录，仓库记录只保存脱敏摘要与本地证据摘要。Freestyle inventory 显示零个匹配 VM；专用 VPC 保留，关联专用防火墙规则为 0 条。
