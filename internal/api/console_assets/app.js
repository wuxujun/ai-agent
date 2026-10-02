(() => {
  "use strict";

  const view = document.getElementById("view");
  const nav = document.getElementById("nav");
  const account = document.getElementById("account");
  const notice = document.getElementById("notice");
  const state = {
    session: null, timer: null, stream: null, renderID: 0, refreshPending: false,
    taskStatus: "", taskSession: "", taskCursor: "", taskHistory: [], taskPage: 1,
    traceCursor: "", traceHistory: [], tracePage: 1,
    traceSearch: "", traceErrors: false, traceRole: "", currentTaskID: "",
    approvalStatus: "pending", approvalCursor: "", approvalHistory: [],
  };

  function el(tag, className = "", value = "") {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (value !== "") node.textContent = String(value);
    return node;
  }

  function button(label, className, action) {
    const node = el("button", `button ${className}`, label);
    node.type = "button";
    node.addEventListener("click", action);
    return node;
  }

  function link(label, href, className = "") {
    const node = el("a", className, label);
    node.href = href;
    return node;
  }

  function append(parent, ...children) {
    parent.append(...children.filter(Boolean));
    return parent;
  }

  function flash(message, isError = false) {
    notice.textContent = message;
    notice.className = `notice${isError ? " error" : ""}`;
    notice.hidden = false;
    clearTimeout(flash.timeout);
    flash.timeout = setTimeout(() => { notice.hidden = true; }, 6000);
  }

  async function api(path, options = {}) {
    const method = options.method || "GET";
    const headers = { ...(options.headers || {}) };
    if (options.body !== undefined) headers["Content-Type"] = "application/json";
    if (![/^GET$/, /^HEAD$/, /^OPTIONS$/].some(pattern => pattern.test(method)) && state.session?.csrf_token) {
      headers["X-Console-CSRF"] = state.session.csrf_token;
    }
    let response;
    try {
      response = await fetch(path, {
        method, credentials: "same-origin", headers,
        body: options.body === undefined ? undefined : JSON.stringify(options.body),
        cache: "no-store",
      });
    } catch (error) {
      throw new Error("网络连接失败，请稍后重试。");
    }
    const raw = await response.text();
    let data = null;
    try { data = raw ? JSON.parse(raw) : null; } catch { /* Keep status when a proxy returns non-JSON. */ }
    if (!response.ok) {
      if (response.status === 401 && path !== "/console/session") showLogin();
      const error = new Error(data?.error || `请求失败（HTTP ${response.status}）`);
      error.status = response.status;
      throw error;
    }
    return data;
  }

  function stopLive() {
    if (state.timer) clearInterval(state.timer);
    if (state.stream) state.stream.close();
    state.timer = null;
    state.stream = null;
    state.refreshPending = false;
  }

  function setAccount() {
    account.replaceChildren();
    nav.hidden = !state.session;
    if (!state.session) return;
    append(account, el("span", "mono", state.session.tenant_id), button("退出", "quiet", async () => {
      try { await api("/console/session", { method: "DELETE" }); } catch (error) { flash(error.message, true); }
      state.session = null;
      showLogin();
    }));
  }

  function showLogin() {
    state.renderID++;
    stopLive();
    state.session = null;
    setAccount();
    const card = el("section", "card login");
    append(card, el("p", "eyebrow", "Secure access"), el("h1", "", "登录工作台"),
      el("p", "subtle", "输入组织身份系统签发的 Bearer Token。验证后凭证只保存在服务端加密的 HttpOnly 会话 Cookie 中。"));
    const form = el("form");
    const field = el("div", "field");
    const label = el("label", "", "Bearer Token");
    label.htmlFor = "login-token";
    const input = el("input");
    input.id = "login-token"; input.name = "token"; input.type = "password"; input.required = true;
    input.autocomplete = "off"; input.spellcheck = false;
    append(field, label, input);
    const submit = el("button", "button primary", "登录");
    submit.type = "submit";
    append(form, field, submit);
    form.addEventListener("submit", async event => {
      event.preventDefault();
      submit.disabled = true;
      try {
        const session = await api("/console/session", { method: "POST", body: { token: input.value.trim().replace(/^Bearer\s+/i, "") } });
        input.value = "";
        state.session = session;
        setAccount();
        if (!location.hash) location.hash = "#/tasks";
        await renderRoute();
      } catch (error) {
        input.value = "";
        flash(error.message, true);
      } finally { submit.disabled = false; }
    });
    append(card, form);
    view.replaceChildren(card);
  }

  function routeParts() {
    const hash = location.hash.replace(/^#/, "") || "/tasks";
    try { return hash.split("?")[0].split("/").filter(Boolean).map(decodeURIComponent); }
    catch { return ["tasks"]; }
  }

  function updateNav(parts) {
    for (const anchor of nav.querySelectorAll("a")) {
      anchor.classList.toggle("active", anchor.dataset.nav === parts[0]);
    }
  }

  function pageHead(kicker, title, actions = []) {
    const head = el("div", "page-head");
    const titleWrap = el("div");
    append(titleWrap, el("p", "eyebrow", kicker), el("h1", "", title));
    append(head, titleWrap, append(el("div", "actions"), ...actions));
    return head;
  }

  function empty(title, description) {
    return append(el("div", "empty"), el("h3", "", title), el("p", "", description));
  }

  function statusBadge(status) {
    return el("span", `badge ${status || ""}`, status || "未知");
  }

  function formatTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("zh-CN");
  }

  function section(title) {
    return append(el("section", "card"), el("h2", "", title));
  }

  function field(label, name, type = "text", value = "") {
    const wrap = el("div", "field");
    const labelNode = el("label", "", label);
    labelNode.htmlFor = name;
    const input = el(type === "textarea" ? "textarea" : "input");
    input.name = name; input.id = name;
    if (type !== "textarea") input.type = type;
    input.value = value;
    append(wrap, labelNode, input);
    return wrap;
  }

  function addKV(parent, entries) {
    const list = el("dl", "kv");
    for (const [key, value] of entries) {
      append(list, el("dt", "", key), el("dd", "", value === null || value === undefined || value === "" ? "—" : value));
    }
    parent.append(list);
  }

  async function renderTasksList(id) {
    view.replaceChildren(pageHead("Tasks", "任务管理", [link("新建任务", "#/tasks/new", "button primary")]));
    const controls = el("div", "toolbar");
    const status = el("select");
    for (const [value, label] of [["", "全部状态"], ["created", "已创建"], ["running", "运行中"], ["awaiting_approval", "待审批"], ["paused", "已暂停"], ["completed", "已完成"], ["partial", "部分完成"], ["failed", "失败"]]) {
      const option = el("option", "", label); option.value = value; status.append(option);
    }
    status.value = state.taskStatus;
    status.setAttribute("aria-label", "状态筛选");
    status.addEventListener("change", () => { state.taskStatus = status.value; state.taskCursor = ""; state.taskHistory = []; state.taskPage = 1; renderRoute(); });
    const session = el("input");
    session.placeholder = "按 Session ID 筛选"; session.value = state.taskSession; session.setAttribute("aria-label", "Session ID");
    session.addEventListener("keydown", event => { if (event.key === "Enter") { state.taskSession = session.value.trim(); state.taskCursor = ""; state.taskHistory = []; state.taskPage = 1; renderRoute(); } });
    append(controls, status, session, button("查询", "", () => { state.taskSession = session.value.trim(); state.taskCursor = ""; state.taskHistory = []; state.taskPage = 1; renderRoute(); }), button("刷新", "", () => renderRoute()));
    view.append(controls);
    const params = new URLSearchParams({ view: "summary", limit: "20" });
    if (state.taskCursor) params.set("cursor", state.taskCursor);
    if (state.taskStatus) params.set("status", state.taskStatus);
    if (state.taskSession) params.set("session_id", state.taskSession);
    try {
      const result = await api(`/api/tasks?${params}`);
      if (id !== state.renderID) return;
      const tasks = result.tasks || [];
      const card = el("section", "card");
      if (!tasks.length) card.append(empty("没有匹配任务", "更改筛选条件，或创建一个新任务。"));
      else {
        const wrap = el("div", "table-wrap");
        const table = el("table");
        const thead = el("thead");
        const labels = ["任务", "状态", "模式 / Team", "步骤", "LLM 调用 / 预估成本", "更新时间"];
        append(thead, append(el("tr"), ...labels.map(label => el("th", "", label))));
        const tbody = el("tbody");
        for (const task of tasks) {
          const title = el("td", "goal-cell");
          const taskLink = link(task.goal || "（无目标）", `#/tasks/${encodeURIComponent(task.id)}`);
          taskLink.className = "goal-link";
          append(title, append(el("strong"), taskLink), el("span", "mono", task.id));
          const statusCell = append(el("td"), statusBadge(task.status));
          append(tbody, append(el("tr"), title, statusCell,
            el("td", "", [task.mode, task.team].filter(Boolean).join(" / ") || "—"),
            el("td", "", `${task.step_count || 0} / ${task.max_steps || 0}`),
            el("td", "", `${task.llm_calls || 0} / $${Number(task.llm_estimated_cost_usd || 0).toFixed(3)}`),
            el("td", "small", formatTime(task.updated_at || task.created_at))));
        }
        append(table, thead, tbody); wrap.append(table); card.append(wrap);
      }
      view.append(card);
      const pager = el("div", "pagination");
      const previous = button("上一页", "", () => { state.taskCursor = state.taskHistory.pop() || ""; state.taskPage--; renderRoute(); });
      previous.disabled = state.taskHistory.length === 0;
      const next = button("下一页", "", () => { state.taskHistory.push(state.taskCursor); state.taskCursor = result.next_cursor; state.taskPage++; renderRoute(); });
      next.disabled = !result.has_more;
      append(pager, previous, el("span", "subtle small", `第 ${state.taskPage} 页 · 本页 ${tasks.length} 项`), next);
      view.append(pager);
      state.timer = setInterval(() => { if (!document.hidden) renderRoute(); }, 15000);
    } catch (error) { if (id === state.renderID) view.append(empty("任务加载失败", error.message)); }
  }

  async function renderNewTask(id) {
    view.replaceChildren(link("← 返回任务列表", "#/tasks", "crumb"), pageHead("New task", "创建任务"));
    const card = section("任务配置");
    const form = el("form");
    const grid = el("div", "form-grid");
    const goal = field("任务目标", "goal", "textarea"); goal.classList.add("wide"); goal.querySelector("textarea").required = true;
    const workspace = field("工作区", "workspace", "text", "./workspace"); workspace.querySelector("input").required = true;
    const modeWrap = el("div", "field");
    const modeLabel = el("label", "", "执行模式"); modeLabel.htmlFor = "mode";
    const mode = el("select"); mode.id = "mode"; mode.name = "mode";
    for (const value of ["", "eino", "legacy", "adk", "step", "multiagent"]) {
      const option = el("option", "", value || "服务端默认"); option.value = value; mode.append(option);
    }
    append(modeWrap, modeLabel, mode);
    const teamWrap = el("div", "field");
    const teamLabel = el("label", "", "Team（仅 multiagent）"); teamLabel.htmlFor = "team";
    const team = el("select"); team.id = "team"; team.name = "team";
    const defaultTeam = el("option", "", "服务端默认"); defaultTeam.value = ""; team.append(defaultTeam);
    append(teamWrap, teamLabel, team);
    mode.addEventListener("change", () => { team.disabled = mode.value !== "multiagent"; });
    team.disabled = true;
    append(grid, goal, workspace, modeWrap, teamWrap,
      field("最大执行步数", "max_steps", "number", "5"),
      field("工具调用预算", "tool_budget", "number", "5"),
      field("Token 预算（0 表示不设置）", "token_budget", "number", "0"));
    const submit = el("button", "button primary", "创建任务"); submit.type = "submit";
    const run = el("button", "button", "创建并运行"); run.type = "submit"; run.name = "run"; run.value = "yes";
    append(form, grid, append(el("div", "actions"), submit, run));
    form.addEventListener("submit", async event => {
      event.preventDefault(); submit.disabled = true; run.disabled = true;
      const data = new FormData(form);
      const payload = { goal: String(data.get("goal") || "").trim(), workspace: String(data.get("workspace") || "").trim() };
      for (const key of ["mode", "team"]) if (data.get(key)) payload[key] = data.get(key);
      for (const key of ["max_steps", "tool_budget", "token_budget"]) payload[key] = Number(data.get(key) || 0);
      try {
        const created = await api("/api/tasks", { method: "POST", body: payload });
        if (event.submitter === run) {
          try { await api(`/api/tasks/${encodeURIComponent(created.id)}/run-all`, { method: "POST" }); }
          catch (error) { flash(`任务已创建，启动失败：${error.message}`, true); }
        }
        location.hash = `#/tasks/${encodeURIComponent(created.id)}`;
      } catch (error) { flash(error.message, true); }
      finally { submit.disabled = false; run.disabled = false; }
    });
    card.append(form); view.append(card);
    try {
      const teams = await api("/api/teams");
      if (id !== state.renderID) return;
      for (const item of teams.teams || []) {
        const option = el("option", "", item.name); option.value = item.name; team.append(option);
      }
    } catch { /* The server still validates a submitted team. */ }
  }

  function traceSection(page, taskID) {
    const card = section(`执行 Trace · 第 ${state.tracePage} 页`);
    const events = page.events || [];
    if (!events.length) card.append(empty("暂无执行记录", "任务运行后，持久化的步骤将显示在这里。"));
    const filter = el("div", "toolbar");
    const search = el("input"); search.placeholder = "筛选动作或角色"; search.setAttribute("aria-label", "筛选 Trace");
    search.value = state.traceSearch;
    const role = el("select"); role.setAttribute("aria-label", "按角色筛选当前页");
    for (const [value, label] of [["", "全部角色"], ["single", "单 Agent"], ["planner", "Planner"], ["critic", "Critic"], ["executor", "Executor"], ["verifier", "Verifier"], ["researcher", "Researcher"], ["writer", "Writer"]]) {
      const option = el("option", "", label); option.value = value; role.append(option);
    }
    role.value = state.traceRole;
    const errors = el("input"); errors.type = "checkbox"; errors.id = "trace-errors";
    errors.checked = state.traceErrors;
    const errorLabel = el("label", "small", "仅错误"); errorLabel.htmlFor = "trace-errors";
    append(filter, search, role, errors, errorLabel); card.append(filter);
    const list = el("ol", "trace-list"); card.append(list);
    card.append(el("p", "small subtle", "筛选范围：当前页。每页最多 100 条，按持久化顺序显示。"));
    function paint() {
      list.replaceChildren();
      const term = search.value.trim().toLowerCase();
      const matches = events.filter(({ trace: entry }) =>
        (!errors.checked || entry.error) && (!role.value || (entry.agent_role || "single") === role.value) &&
        (!term || `${entry.action || ""} ${entry.agent_role || ""} ${entry.step ?? ""}`.toLowerCase().includes(term)));
      for (const { trace: entry, sequence, event_id: eventID, recorded_at: recordedAt } of matches) {
        const item = el("li", `trace-item${entry.error ? " error" : ""}`);
        const head = el("div", "trace-head");
        append(head, el("span", "mono small subtle", `#${sequence} · Step ${entry.step ?? "—"}`),
          el("strong", "", entry.action || "事件"));
        if (recordedAt) head.append(el("span", "small subtle", `首次记录 ${formatTime(recordedAt)}`));
        if (entry.agent_role) head.append(el("span", "badge", entry.agent_role));
        if (entry.error) head.append(el("span", "badge failed", "错误"));
        item.append(head);
        const overview = entry.error || entry.observation || entry.goal || entry.query || "无文本记录";
        item.append(el("p", "text-block", overview.length > 300 ? `${overview.slice(0, 300)}…` : overview));
        const details = el("details"); details.append(el("summary", "", "事件元数据与完整记录"));
        if (eventID) details.append(el("p", "mono small subtle", `事件 ID：${eventID}`));
        if (overview.length > 300) details.append(el("pre", "", overview));
        if (entry.query) details.append(el("pre", "", `Query: ${entry.query}`));
        for (const evidence of entry.evidence || []) {
          details.append(el("pre", "", `${evidence.path || ""}\n${evidence.query || ""}\n${(evidence.lines || []).join("\n")}`));
        }
        if (entry.token_usage?.total_tokens) details.append(el("p", "small subtle", `Token: ${entry.token_usage.total_tokens}`));
        item.append(details);
        list.append(item);
      }
      if (!matches.length) list.append(empty("没有匹配记录", "调整筛选条件后重试。"));
    }
    search.addEventListener("input", () => { state.traceSearch = search.value; paint(); });
    role.addEventListener("change", () => { state.traceRole = role.value; paint(); });
    errors.addEventListener("change", () => { state.traceErrors = errors.checked; paint(); });
    paint();
    const pager = el("div", "pagination");
    const previous = button("上一页", "", () => { state.traceCursor = state.traceHistory.pop() || ""; state.tracePage--; renderTaskDetail(state.renderID, taskID, true); });
    previous.disabled = state.traceHistory.length === 0;
    const next = button("下一页", "", () => { state.traceHistory.push(state.traceCursor); state.traceCursor = page.next_cursor; state.tracePage++; renderTaskDetail(state.renderID, taskID, true); });
    next.disabled = !page.has_more;
    append(pager, previous, el("span", "small subtle", `本页 ${events.length} 条`), next); card.append(pager);
    return card;
  }

  async function renderTaskDetail(id, taskID, keepStream = false) {
    if (state.currentTaskID !== taskID) {
      state.currentTaskID = taskID;
      state.traceCursor = ""; state.traceHistory = []; state.tracePage = 1;
      state.traceSearch = ""; state.traceErrors = false; state.traceRole = "";
    }
    try {
      const traceParams = new URLSearchParams({ limit: "100" });
      if (state.traceCursor) traceParams.set("cursor", state.traceCursor);
      const requestedTraceCursor = state.traceCursor;
      const [task, approvalsResponse, tracePage] = await Promise.all([
        api(`/api/tasks/${encodeURIComponent(taskID)}?view=summary`),
        api(`/api/tasks/${encodeURIComponent(taskID)}/approvals`).catch(() => ({ approvals: [] })),
        api(`/api/tasks/${encodeURIComponent(taskID)}/trace?${traceParams}`),
      ]);
      if (id !== state.renderID || requestedTraceCursor !== state.traceCursor) return;
      const active = ["created", "running", "awaiting_approval", "paused"].includes(task.status);
      const actions = [];
      const doAction = async (method, path, confirmation) => {
        if (confirmation && !confirm(confirmation)) return;
        try { await api(path, { method }); await renderTaskDetail(id, taskID, true); }
        catch (error) { flash(error.message, true); await renderTaskDetail(id, taskID, true); }
      };
      if (task.allowed_actions?.includes("run_all")) actions.push(button("运行任务", "primary", () => doAction("POST", `/api/tasks/${encodeURIComponent(taskID)}/run-all`)));
      if (task.allowed_actions?.includes("cancel")) actions.push(button("取消任务", "danger", () => doAction("DELETE", `/api/tasks/${encodeURIComponent(taskID)}/cancel`, "确定取消此任务？")));
      if (task.allowed_actions?.includes("re_audit")) actions.push(button("重新审计", "", () => doAction("POST", `/api/tasks/${encodeURIComponent(taskID)}/re-audit`)));
      if (task.allowed_actions?.includes("delete")) actions.push(button("删除任务", "danger", async () => {
        if (!confirm("永久删除此任务及其记录？")) return;
        try { await api(`/api/tasks/${encodeURIComponent(taskID)}`, { method: "DELETE" }); location.hash = "#/tasks"; }
        catch (error) { flash(error.message, true); }
      }));
      const crumb = link("← 返回任务列表", "#/tasks", "crumb");
      const head = pageHead("Task detail", task.goal || task.id, actions);
      const statusLine = append(el("div", "toolbar"), statusBadge(task.status), el("span", "mono small subtle", task.id));
      const usage = (tracePage.events || []).reduce((sum, event) => sum + Number(event.trace.token_usage?.total_tokens || 0), 0);
      const stats = el("div", "stats");
      for (const [label, value] of [["执行步数", `${task.step_count || 0} / ${task.max_steps || 0}`], ["本页 Token", `${usage} · 预算 ${task.token_budget || "不限"}`], ["LLM 调用", String(task.llm_calls || 0)], ["预估成本", `$${Number(task.llm_estimated_cost_usd || 0).toFixed(3)}`]]) {
        append(stats, append(el("div", "stat"), el("span", "label", label), el("strong", "", value)));
      }
      const details = section("任务概览");
      addKV(details, [["模式", task.mode], ["Team", task.team], ["工作区", task.workspace], ["Session", task.session_id], ["OTel Trace ID", task.execution_trace_id], ["创建时间", formatTime(task.created_at)], ["更新时间", formatTime(task.updated_at)]]);
      const approvalCard = section("审批记录");
      const approvals = approvalsResponse.approvals || [];
      if (!approvals.length) approvalCard.append(el("p", "subtle", "此任务暂无审批记录。"));
      for (const approval of approvals) {
        const row = el("div", "toolbar");
        append(row, link(`${approval.action} · ${approval.id}`, `#/approvals/${encodeURIComponent(approval.id)}`, "mono small"), statusBadge(approval.status));
        approvalCard.append(row);
      }
      const right = el("div"); append(right, details, approvalCard);
      if (task.final_answer) append(right, append(section("最终答案"), el("p", "text-block", task.final_answer)));
      if (task.answer_audit) {
        const audit = section("答案质量审计");
        audit.append(el("pre", "preview", JSON.stringify(task.answer_audit, null, 2)));
        right.append(audit);
      }
      const left = el("div");
      if (task.error_code || task.error_message) left.append(el("div", "error-banner", `${task.error_code || "执行错误"}: ${task.error_message || ""}`));
      left.append(traceSection(tracePage, taskID));
      view.replaceChildren(crumb, head, statusLine, stats, append(el("div", "grid"), left, right));
      if (active && !state.stream) startTaskLive(id, taskID);
      if (!active) stopLive();
    } catch (error) { if (id === state.renderID) view.replaceChildren(empty("任务加载失败", error.message)); }
  }

  function startTaskLive(id, taskID) {
    if (state.stream) return;
    const stream = new EventSource(`/api/tasks/${encodeURIComponent(taskID)}/stream`);
    state.stream = stream;
    stream.addEventListener("message", () => {
      if (id !== state.renderID || state.refreshPending) return;
      state.refreshPending = true;
      setTimeout(async () => { state.refreshPending = false; if (id === state.renderID) await renderTaskDetail(id, taskID, true); }, 300);
    });
    stream.addEventListener("error", () => { if (id === state.renderID) flash("实时连接中断，正在从持久化任务恢复状态。", true); });
    state.timer = setInterval(() => { if (id === state.renderID && !document.hidden) renderTaskDetail(id, taskID, true); }, 8000);
  }

  async function renderApprovalsList(id) {
    view.replaceChildren(pageHead("Approvals", "审批待办", [button("刷新", "", () => renderRoute())]));
    const toolbar = el("div", "toolbar");
    const status = el("select"); status.setAttribute("aria-label", "审批状态");
    for (const value of ["pending", "approved", "rejected", "expired", "consumed"]) {
      const option = el("option", "", value); option.value = value; status.append(option);
    }
    status.value = state.approvalStatus;
    status.addEventListener("change", () => { state.approvalStatus = status.value; state.approvalCursor = ""; state.approvalHistory = []; renderRoute(); });
    toolbar.append(status); view.append(toolbar);
    const params = new URLSearchParams({ status: state.approvalStatus, limit: "20" });
    if (state.approvalCursor) params.set("cursor", state.approvalCursor);
    try {
      const page = await api(`/api/approvals?${params}`);
      if (id !== state.renderID) return;
      const card = el("section", "card");
      if (!(page.approvals || []).length) card.append(empty("暂无审批记录", "当前筛选状态没有可展示的审批。"));
      else {
        const wrap = el("div", "table-wrap"); const table = el("table");
        append(table, append(el("thead"), append(el("tr"), ...["审批", "状态", "任务", "风险", "创建时间"].map(text => el("th", "", text)))));
        const body = el("tbody");
        for (const approval of page.approvals) {
          append(body, append(el("tr"),
            append(el("td", "goal-cell"), append(el("strong"), link(approval.action, `#/approvals/${encodeURIComponent(approval.id)}`)), el("span", "mono", approval.id)),
            append(el("td"), statusBadge(approval.status)),
            append(el("td"), link(approval.task_id, `#/tasks/${encodeURIComponent(approval.task_id)}`, "mono small")),
            el("td", "", approval.risk_level), el("td", "small", formatTime(approval.created_at))));
        }
        append(table, body); wrap.append(table); card.append(wrap);
      }
      view.append(card);
      const pager = el("div", "pagination");
      const previous = button("上一页", "", () => { state.approvalCursor = state.approvalHistory.pop() || ""; renderRoute(); });
      previous.disabled = state.approvalHistory.length === 0;
      const next = button("下一页", "", () => { state.approvalHistory.push(state.approvalCursor); state.approvalCursor = page.next_cursor; renderRoute(); });
      next.disabled = !page.has_more;
      append(pager, previous, el("span", "small subtle", `本页 ${page.count || 0} 项`), next); view.append(pager);
      if (state.approvalStatus === "pending") state.timer = setInterval(() => { if (!document.hidden) renderRoute(); }, 15000);
    } catch (error) { if (id === state.renderID) view.append(empty("审批加载失败", error.message)); }
  }

  async function renderApprovalDetail(id, approvalID) {
    try {
      const approval = await api(`/api/approvals/${encodeURIComponent(approvalID)}`);
      if (id !== state.renderID) return;
      const crumb = link("← 返回审批待办", "#/approvals", "crumb");
      const head = pageHead("Approval detail", approval.action || approval.id);
      const card = section("待审批操作");
      addKV(card, [["审批 ID", approval.id], ["状态", approval.status], ["风险", approval.risk_level],
        ["工作区", approval.workspace], ["决策主体", approval.actor_id],
        ["创建时间", formatTime(approval.created_at)], ["更新时间", formatTime(approval.updated_at)]]);
      const taskLine = el("p", "small"); append(taskLine, el("span", "subtle", "关联任务："), link(approval.task_id, `#/tasks/${encodeURIComponent(approval.task_id)}`, "mono"));
      card.append(taskLine);
      if (approval.parameter_summary?.length) {
        card.append(el("h3", "", "参数摘要"));
        card.append(el("pre", "preview", approval.parameter_summary.join("\n")));
      }
      const preview = section("操作预览");
      preview.append(approval.preview ? el("pre", "preview", approval.preview) : empty("没有可用预览", "为避免误批，当前操作不能在控制台批准。"));
      view.replaceChildren(crumb, head, append(el("div", "toolbar"), statusBadge(approval.status), el("span", "mono small subtle", approval.id)), card, preview);
      if (approval.status === "pending") {
        const decision = section("处理审批");
        const message = field("说明 / 拒绝原因", "approval-message", "textarea");
        decision.append(message);
        const approve = button("批准", "primary", () => submitDecision(true));
        const reject = button("拒绝", "danger", () => submitDecision(false));
        approve.disabled = !approval.preview;
        append(decision, append(el("div", "decision"), approve, reject));
        view.append(decision);
        async function submitDecision(accepted) {
          const note = message.querySelector("textarea").value.trim();
          if (!accepted && !note) { flash("请填写拒绝原因。", true); return; }
          if (!confirm(`确定${accepted ? "批准" : "拒绝"}该操作？`)) return;
          approve.disabled = true; reject.disabled = true;
          try {
            const path = `/api/tasks/${encodeURIComponent(approval.task_id)}/${accepted ? "approve" : "reject"}`;
            await api(path, { method: "POST", body: { approval_id: approval.id, message: note } });
            flash("决策已提交，正在确认持久化状态。");
          } catch (error) {
            flash(error.status === 409 || error.status === 410 ? "审批状态已变化，正在刷新。" : error.message, true);
          }
          await renderApprovalDetail(id, approvalID);
        }
        if (!state.timer) state.timer = setInterval(() => { if (id === state.renderID && !document.hidden) renderApprovalDetail(id, approvalID); }, 10000);
      } else if (state.timer) {
        clearInterval(state.timer);
        state.timer = null;
      }
    } catch (error) { if (id === state.renderID) view.replaceChildren(empty("审批加载失败", error.message)); }
  }

  async function renderRoute() {
    if (!state.session) { showLogin(); return; }
    stopLive();
    const id = ++state.renderID;
    const parts = routeParts();
    updateNav(parts);
    view.replaceChildren(el("p", "subtle", "加载中…"));
    if (parts[0] === "tasks" && parts.length === 1) await renderTasksList(id);
    else if (parts[0] === "tasks" && parts[1] === "new") await renderNewTask(id);
    else if (parts[0] === "tasks" && parts[1]) {
      if (state.currentTaskID !== parts[1]) {
        state.traceShown = 100;
        state.traceSearch = "";
        state.traceErrors = false;
        state.currentTaskID = parts[1];
      }
      await renderTaskDetail(id, parts[1]);
    }
    else if (parts[0] === "approvals" && parts.length === 1) await renderApprovalsList(id);
    else if (parts[0] === "approvals" && parts[1]) await renderApprovalDetail(id, parts[1]);
    else location.hash = "#/tasks";
  }

  window.addEventListener("hashchange", renderRoute);
  (async () => {
    try {
      state.session = await api("/console/session");
      setAccount();
      if (!location.hash) location.hash = "#/tasks";
      await renderRoute();
    } catch { showLogin(); }
  })();
})();
