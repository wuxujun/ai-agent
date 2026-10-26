# Console 浏览器回归

使用固定版本 `@playwright/test 1.58.2` 和 Chromium、Firefox、WebKit，访问 Go 的真实 Console/任务路由。依赖仅用于测试，不参与生产二进制构建。工具安装方式见 [Playwright 官方文档](https://playwright.dev/docs/library)，浏览器矩阵采用 [Projects 配置](https://playwright.dev/docs/test-projects)。

当前共 25 个场景，每个浏览器均运行，共 75 项。覆盖账号设置/导航、UA_TOKEN 与手动登录、任务/审批/Trace、键盘和窄屏、延迟请求焦点、SSE 中断轮询与 open/error 对账，以及原生 EventSource 在有限 EOF 后自动重连真实 Go 路由。

## 运行

准备 Go 1.26.8、Node.js 22 及 npm，在仓库根目录执行：

```bash
cd tests/console
npm ci --ignore-scripts --no-audit --no-fund
npx playwright install chromium firefox webkit
cd ../..
AI_AGENT_RUN_CONSOLE_BROWSER_TESTS=1 go test -race -v -count=1 -timeout=5m ./internal/api -run '^TestConsoleBrowser$'
```

Linux CI 安装浏览器时增加 `--with-deps`。本地也可将 `PLAYWRIGHT_BROWSERS_PATH` 指向仓库外的浏览器缓存；安装与执行时须使用同一位置。

默认运行全部三个浏览器。只运行某个浏览器时，在 Go 命令前增加 `UI_BROWSER=chromium`、`UI_BROWSER=firefox` 或 `UI_BROWSER=webkit`。未安装选定浏览器会失败；非法名称也会明确拒绝。可选 `UI_BROWSER_EXECUTABLE` 只允许与 `UI_BROWSER=chromium` 一起使用，避免错误地把同一可执行文件用于其他引擎。

定向复验可设置 `UI_TEST_GREP='narrow|SSE'` 或 `UI_TEST_GREP='native EventSource'`，透传为 Playwright 的 `--grep`，不改变正常全套测试范围。每次运行使用新的 `UI_TEST_OUTPUT_DIR`，避免覆盖前次证据。

可用 `UI_TEST_OUTPUT_DIR` 指定独立的测试产物目录（建议每次运行使用一个新的绝对路径），以保留不同引擎的截图和失败 trace，避免后续 Playwright 运行清理/覆盖前次结果。未设置时沿用 `tests/console/test-results`。

测试必须从 Go 入口启动；它创建临时工作区和内存 Store，设置测试会话密钥、假的 introspection 响应及确定性 Planner，并将临时服务 URL/workspace 传给 Playwright。不读取根目录 `.env`，不需要真实 Token、数据库、LLM 或 HA 环境。浏览器请求仅允许访问该测试服务。

普通 `go test ./...` 会跳过 `TestConsoleBrowser`；显式启用后缺少 Node/依赖/浏览器或任一用例失败都会使命令失败。CI 的 `console-browser` 矩阵按三个浏览器分别安装依赖、运行 25 项回归及 Go 竞态检测；失败不取消其他浏览器，证据名称按浏览器区分。

## 运行实例的匿名检查

`live-anonymous.mjs` 使用独立浏览器上下文，不读取 `.env`、Token、用户浏览器配置或登录状态，只允许明确的 loopback HTTP origin、Console 静态资源及匿名会话路径。应用自身的空 `{}` 自动登录请求在服务端的 Token 长度检查处拒绝，不进入身份查询；脚本不提交凭据或任务操作。另以无凭据 GET 验证 `/api/teams` 返回 401。

```bash
UI_LIVE_BASE_URL=http://127.0.0.1:8088 UI_BROWSER=firefox npm --prefix tests/console run test:live-anonymous
npm --prefix tests/console run test:live-policy
```

使用自定义浏览器缓存时沿用相同的 `PLAYWRIGHT_BROWSERS_PATH`。macOS 已安装 Chrome 可指定 `UI_BROWSER=chromium` 和 `UI_BROWSER_EXECUTABLE='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'`。不指定 `UI_BROWSER` 时运行三个已安装引擎；自定义 executable 仍只支持明确选择 chromium。

每次运行创建新的系统临时目录，保存筛选后的 JSON 与 1440/390/320px 匿名页面截图。检查脚本异常、非预期控制台错误、静态资源状态、空输入、隐藏导航、键盘跳转、匿名路由及横向溢出。Playwright 1.58.2 的 WebKit 截图准备会注入 inline `body {}`，被页面 CSP 拒绝；因此 WebKit 保留所有 DOM/布局/键盘与控制台断言，但跳过截图，不关闭 CSP 或忽略相关错误。Chrome/Firefox 保留截图。

`live-anonymous-policy.test.mjs` 使用 Node 内置测试验证远程地址、userinfo、path/query/fragment、非法 URL 和浏览器选项在启动浏览器前被拒绝，错误不回显带凭据的输入。匿名检查不等于真实用户登录、TLS/代理或审批验收。

## 覆盖与边界

真实 Go 路由覆盖：

- 无效登录、凭证清空、加密 HttpOnly/SameSite Cookie、刷新与注销。
- 缺失/错误 CSRF、跨 Origin 写请求拒绝，UI 写请求发送正确 CSRF。
- 创建并执行到终态、运行与取消，以及其他租户任务/Trace/审批记录不可见。
- 250 条持久化 Trace 的分页、虚拟滚动、错误/动作筛选、详情展开与 HTML 文本转义。
- SSE 连接被浏览器夹具中断后，通过 UI 轮询读回真实持久化终态。
- 原生 EventSource 首连接注入有限 EOF 后自动重连真实 Go SSE 路由，收到 snapshot header，随后重新读取 Task/Trace/approvals 并显示已持久化取消状态；未替换 EventSource 为伪事件类。
- 未登录导航隐藏；键盘跳转主要内容、登录、新建任务切换时的焦点和当前导航语义；Trace 使用 Enter 展开/折叠后保留焦点。
- 320px/390px 窄屏任务列表、Trace 和审批预览无横向溢出；审批操作可通过滚动到达。用例生成窄屏 Trace 截图供人工复核。
- 新建任务的 Team 列表延迟返回时，保持输入框焦点和已填内容。

浏览器网络夹具覆盖：

- 详情读取返回 401 后保留登录页，旧请求不覆盖页面。
- 审批 202 后仍显示待确认状态；409/410 后重新读取 consumed/expired 状态。
- 无预览禁止批准、拒绝必须填写原因、决策不自动重复提交。

macOS WebKit 的链接导航使用 Option-Tab（反向 Option-Shift-Tab），其他项目使用 Tab/Shift-Tab。此处按原生键盘行为验证，不修改系统偏好，依据见 [Apple 键盘说明](https://support.apple.com/en-lamr/guide/safari/cpsh003/mac)。

审批状态响应在 Playwright 中模拟，未验证真实加密审批恢复/CAS、跨实例或真实身份服务。当前覆盖三个 Playwright 引擎的桌面与两种窄屏宽度；不等于正式 Safari/Chrome/Firefox 各发行版本或真实移动设备的完整验收。其他设备/字体缩放、读屏、完整可访问性及页面性能仍待验证。

失败时保留截图和 Playwright Trace 到 `tests/console/test-results/`（Git 已忽略）；CI 仅在失败时上传，保留 7 天。材料只包含本套测试夹具，勿将真实生产会话复用到此测试入口。


2026-10-12 SSE 历史更新：当时每个浏览器 18 个场景，共 54 项。新增 open/error 生命周期触发后的 300 ms 对账与终态关闭验证，Task/Trace/approvals 读取和取消使用真实认证路由；EventSource 生命周期信号由浏览器夹具驱动，不等于代理/LB 自动重连验收。服务端独立测试覆盖真实 HTTP 断开、两个独立事件总线/处理器共享 SQLite、非终态/终态轮询和慢消费者丢事件。2026-10-25 新增原生 EventSource 的 EOF→真实路由重连；当前总量及实际运行边界以顶部和 [本次浏览器验收](../../docs/Console浏览器验收_2026-10-25.md) 为准。

## 候选二进制隔离运行（可选）

`candidate-smoke.py` 验证已构建二进制，不使用 `httptest` 替换应用。先校验包内 MANIFEST.json 中的 server SHA-256，再启动本地 Postman simulation、临时 HTTPS 身份转发和应用入口、全新 SQLite 与随机测试会话密钥。测试只创建一个任务，不执行任务，不调用模型；所有产物保留在新的 `/private/tmp/ai-agent-candidate-runtime-*` 目录，退出时只停止自身进程/容器。

前提：Python 3、支持 `-addext` 的 OpenSSL、Postman CLI >= 1.70。macOS 的 Go 系统证书验证不使用本脚本的 SSL_CERT_FILE，因此 macOS 上需使用匹配架构的 Linux 测试二进制和已缓存、按 SHA-256 固定的 Docker 镜像；不修改系统钥匙串、不跳过 TLS 校验。容器不拉取镜像，使用非 root、只读根、资源限额和仅回环端口映射。

```bash
python3 tests/console/candidate-smoke.py \
  --package /absolute/path/to/linux-smoke-package \
  --docker-image sha256:<locally-cached-image-id>
```

包目录需含 server、teams.yaml、skills/、MANIFEST.json；manifest 需有 version、goos 及 file_sha256.server，运行配置由夹具单独生成。Docker Desktop 的 `host.docker.internal` 必须能访问主机回环 HTTPS fixture。Linux 本机也可不加 `--docker-image`；该分支本轮未执行。实际运行命令、镜像、二进制哈希和结果见[候选二进制隔离运行验收](../../docs/Web-UI候选二进制隔离运行验收_2026-10-15.md)。

Postman 文件位于 `postman/simulations/console-candidate.sim.yaml` 与 `postman/mocks/candidate-auth/default.js`。它们根据仓库现有 introspection 测试契约构造固定假身份；未知路径返回 501。这里只验证该契约下的应用行为，不代替生产身份提供方验收。不会查询/上传远端工作区，simulation 使用 `--no-history`；测试日志、临时证书私钥、数据库和真实凭据都不提交 Git。


## macOS 候选本地模型隔离运行

```sh
python3 tests/console/candidate-local-smoke.py \
  --package dist/verification/0.0.0-verify.6a910e0/darwin-arm64
```

该入口校验 manifest 中的二进制哈希，使用独立端口、空环境文件、随机测试密钥、临时 SQLite 和 macOS loopback 出站限制。默认通过本地 Postman simulation 返回固定流式 planner 决策，真实创建并完成一个隔离任务，核验终态 SSE、重启持久性、运行版本及自身进程清理。不会访问仓库 `.env` 或写入现有 8088 服务。需要 Python 3、Node、Postman CLI >=1.70 和 sandbox-exec；缺失隔离机制会拒绝运行。

mock/simulation 位于 `postman/mocks/candidate-model/default.js` 与 `postman/simulations/candidate-runtime.sim.yaml`，源自仓库 planner 契约，未知调用返回 501，不发布远端资源。显式 `--runner node` 分支未在本次验收中运行。证据与限制见[验收记录](../../docs/候选隔离启动验收_2026-10-25.md)。
