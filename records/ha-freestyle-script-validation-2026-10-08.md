# Freestyle HA 方案与脚本本地验证

日期：2026-10-08。应用基线 HEAD：`fbec15693a9b34eaf83ea45c7e35e011decec9f0`。

本轮新增[实施方案](../docs/阶段3.3-Freestyle-VM实施方案.md)和 [Freestyle 脚本](../deploy/ha/freestyle/)，并在原 HA 手册添加入口。开始时已有两份文档修改与两份发布前验证记录未提交，本轮保留这些内容，不改变现有发布包身份、主配置或灰度。

## 验证结果

- `node --test deploy/ha/freestyle/provision.test.mjs`：4 项通过。验证离线规划、硬性宿主机反亲和、创建检查点、规则范围，以及丢失 placement/部分创建失败时停止。
- `python3 -m unittest discover -s deploy/ha/freestyle -p 'test_*.py' -v`：4 项通过。验证私密资产权限、相同 A/B 配置、初始 0% 与只读 Team、默认计划模式、stub 拒绝未知 schema，以及契约缺项/失败/跳过时拒绝通过。
- `bash -n`：五个新增 shell 入口通过；`node --check` 通过。
- 在仓库外新目录生成随机测试秘密及四角色资产，PostgreSQL/Redis 与 Prometheus 两份 `docker compose ... config --quiet` 均通过；未启动容器。
- 使用临时 Go 校验器调用项目实际的 `config.Get()`、`Config.Validate()` 和 `multiagent.LoadTeamsConfigStrict()`，生成配置与 Team 读取成功：`mode=multiagent`、`runtime=legacy`、`canary=0`、`active_team=software`。不启动 API，不调用模型或外部存储。
- 全仓 `go test -p=4 ./... -count=1 -timeout=5m`：最终 49 个含测试的包全部通过。使用临时原生 `go1.26.8 darwin/arm64`，`GOTOOLCHAIN=local`、`GOCACHE=/private/tmp/ai-agent-go-cache`。
- `git diff --check` 通过。

首次全仓测试在沙箱内因 `httptest` 绑定 loopback 端口被禁止而失败；之后经沙箱外执行授权原命令重跑通过。系统默认 Go 工具链探测亦受沙箱代理访问限制，验证沿用先前已校验的临时原生 arm64 工具链，未更改系统工具链。Docker daemon 探测在沙箱内被 socket 权限阻断；Compose 静态校验不依赖 daemon，已单独通过，没有据此执行 Docker 部署。

临时资产与 Go 配置校验器保留在 `/private/tmp/ai-agent-freestyle-validation.eSjOdT`。其中含随机测试秘密，不能整包提交或打印。自动测试使用的临时目录同样保留，不执行批量清理。

## 能力依据与未执行项

核对官方 OpenAPI 和 npm 发布的 `freestyle@0.2.16` 类型契约，使用 `vpcs`、`placement.antiAffinity`、`topology=node` 及明确的 firewall/lifecycle 字段。Prometheus 按官方配置文档使用 header 凭据文件，测试镜像固定为 v3.5.5。资料链接见实施方案。

本轮没有安装云端服务、创建 Freestyle 资源、调用真实模型、执行一小时在线负载、注入故障或提升灰度。SDK 创建流程的本地测试使用 fake client；NFS 内核能力、私网规则、有效运行配额、systemd 和实际恢复行为仍须在目标 VM 验证。只读 stub 不覆盖审批、副作用、Replan 或故障中的实时 SSE，完整验收状态保持未完成。
