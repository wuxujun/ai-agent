# Console 浏览器回归

使用固定版本 `@playwright/test 1.58.2` 和 Chromium、Firefox、WebKit，访问 Go 的真实 Console/任务路由。依赖仅用于测试，不参与生产二进制构建。工具安装方式见 [Playwright 官方文档](https://playwright.dev/docs/library)，浏览器矩阵采用 [Projects 配置](https://playwright.dev/docs/test-projects)。

当前共 18 个场景，每个浏览器均运行，共 54 项：原有 11 项主流程、导航隐藏/键盘焦点、Trace 键盘展开折叠、320px 和 390px 两项窄屏验证，以及延迟 Team 响应时不抢占正在编辑的表单焦点，以及 SSE open/error 后立即对账。

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

可用 `UI_TEST_OUTPUT_DIR` 指定独立的测试产物目录（建议每次运行使用一个新的绝对路径），以保留不同引擎的截图和失败 trace，避免后续 Playwright 运行清理/覆盖前次结果。未设置时沿用 `tests/console/test-results`。

测试必须从 Go 入口启动；它创建临时工作区和内存 Store，设置测试会话密钥、假的 introspection 响应及确定性 Planner，并将临时服务 URL/workspace 传给 Playwright。不读取根目录 `.env`，不需要真实 Token、数据库、LLM 或 HA 环境。浏览器请求仅允许访问该测试服务。

普通 `go test ./...` 会跳过 `TestConsoleBrowser`；显式启用后缺少 Node/依赖/浏览器或任一用例失败都会使命令失败。CI 的 `console-browser` 矩阵按三个浏览器分别安装依赖、运行 18 项回归及 Go 竞态检测；失败不取消其他浏览器，证据名称按浏览器区分。

## 覆盖与边界

真实 Go 路由覆盖：

- 无效登录、凭证清空、加密 HttpOnly/SameSite Cookie、刷新与注销。
- 缺失/错误 CSRF、跨 Origin 写请求拒绝，UI 写请求发送正确 CSRF。
- 创建并执行到终态、运行与取消，以及其他租户任务/Trace/审批记录不可见。
- 250 条持久化 Trace 的分页、虚拟滚动、错误/动作筛选、详情展开与 HTML 文本转义。
- SSE 连接被浏览器夹具中断后，通过 UI 轮询读回真实持久化终态。
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


2026-10-12 SSE 更新：每个浏览器现有 18 个场景，共 54 项。新增 open/error 生命周期触发后的 300 ms 对账与终态关闭验证，Task/Trace/approvals 读取和取消使用真实认证路由；EventSource 生命周期信号由浏览器夹具驱动，不等于代理/LB 自动重连验收。服务端独立测试覆盖真实 HTTP 断开、两个独立事件总线/处理器共享 SQLite、非终态/终态轮询和慢消费者丢事件。
