import { test, expect } from '@playwright/test';

const token = 'browser-fixture-token';
const workspace = process.env.UI_WORKSPACE;
const pageErrors = new WeakMap();

test.beforeEach(async ({ page, context, baseURL }) => {
  pageErrors.set(page, []);
  page.on('pageerror', error => pageErrors.get(page).push(error.message));
  // Test traffic is restricted to the isolated Go fixture, including redirects.
  await context.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin
    ? route.continue() : route.abort());
  page.on('dialog', dialog => dialog.accept());
});

test.afterEach(async ({ page }) => {
  expect(pageErrors.get(page)).toEqual([]);
});

async function login(page) {
  await page.goto('/console');
  await expect(page.getByRole('heading', { name: '登录工作台' })).toBeVisible();
  await page.getByLabel('Bearer Token').fill(`Bearer ${token}`);
  await page.getByLabel('Bearer Token').press('Enter');
  await expect(page.getByRole('heading', { name: '任务管理', exact: true })).toBeVisible();
}

async function createTask(page, goal, run = false) {
  await page.getByRole('link', { name: '新建任务', exact: true }).click();
  await page.getByLabel('任务目标').fill(goal);
  await page.getByLabel('工作区', { exact: true }).fill(workspace);
  const created = page.waitForResponse(response => response.url().endsWith('/api/tasks') && response.request().method() === 'POST');
  await page.getByRole('button', { name: run ? '创建并运行' : '创建任务', exact: true }).click();
  const response = await created;
  expect(response.status()).toBe(201);
  const task = await response.json();
  await expect(page).toHaveURL(new RegExp(`/tasks/${task.id}$`));
  await expect(page.getByRole('heading', { name: goal, exact: true })).toBeVisible();
  return task.id;
}

const taskStatus = page => page.locator('#view > .toolbar .badge').first();

test('late Team options do not steal focus from a task form being edited', async ({ page }) => {
  await login(page);
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  await page.route('**/api/teams', async route => { await gate; await route.continue(); });
  await page.getByRole('link', { name: '新建任务', exact: true }).click();
  const goal = page.getByLabel('任务目标');
  await goal.fill('Keep typing while teams load');
  await expect(goal).toBeFocused();
  release();
  await expect(page.locator('#team option[value="wiki"]')).toHaveCount(1);
  await expect(goal).toBeFocused();
  await expect(goal).toHaveValue('Keep typing while teams load');
});

test('invalid login clears credentials; encrypted session, reload and logout', async ({ page, context }) => {
  await page.goto('/console');
  await page.getByLabel('Bearer Token').fill('invalid-fixture-token');
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('invalid credential');
  await expect(page.getByLabel('Bearer Token')).toHaveValue('');
  await login(page);
  const cookies = await context.cookies();
  const session = cookies.find(cookie => cookie.name === 'ai_agent_console');
  expect(session).toBeDefined();
  expect(session.httpOnly).toBe(true);
  expect(session.sameSite).toBe('Strict');
  expect(session.value).not.toContain(token);
  expect(await page.evaluate(() => ({ cookie: document.cookie, local: localStorage.length, session: sessionStorage.length })))
    .toEqual({ cookie: '', local: 0, session: 0 });
  await page.reload();
  await expect(page.getByRole('heading', { name: '任务管理', exact: true })).toBeVisible();
  await page.getByRole('button', { name: '退出', exact: true }).click();
  await expect(page.getByRole('heading', { name: '登录工作台' })).toBeVisible();
  expect((await context.request.get('/console/session')).status()).toBe(401);
});

test('real cookie-authenticated writes reject missing, wrong CSRF and foreign origin', async ({ page, context }) => {
  await login(page);
  const session = await (await context.request.get('/console/session')).json();
  const body = { goal: 'CSRF must not create a task', workspace };
  for (const headers of [{}, { 'X-Console-CSRF': 'wrong' },
    { 'X-Console-CSRF': session.csrf_token, Origin: 'https://foreign.invalid' }]) {
    expect((await context.request.post('/api/tasks', { data: body, headers })).status()).toBe(403);
  }
  const write = page.waitForRequest(request => request.url().endsWith('/api/tasks') && request.method() === 'POST');
  await createTask(page, 'browser-complete-ui', true);
  expect((await write).headers()['x-console-csrf']).toBe(session.csrf_token);
  await expect(taskStatus(page)).toHaveText('completed');
  const answer = page.locator('section').filter({ has: page.getByRole('heading', { name: '最终答案', exact: true }) });
  await expect(answer.getByText('Browser fixture completed', { exact: true })).toBeVisible();
});

test('task run and cancel use actual lifecycle routes', async ({ page }) => {
  await login(page);
  await createTask(page, 'browser-cancel-ui');
  await expect(taskStatus(page)).toHaveText('created');
  await page.getByRole('button', { name: '运行任务', exact: true }).click();
  await expect(taskStatus(page)).toHaveText('running');
  await page.getByRole('button', { name: '取消任务', exact: true }).click();
  await expect(taskStatus(page)).toHaveText('failed');
  await expect(page.getByRole('button', { name: '取消任务', exact: true })).toHaveCount(0);
});

test('foreign tenant tasks and Trace are invisible', async ({ page, context }) => {
  await login(page);
  await expect(page.getByText('Foreign private task')).toHaveCount(0);
  for (const path of ['/api/tasks/foreign-task', '/api/tasks/foreign-task/trace', '/api/tasks/foreign-task/approvals']) {
    expect((await context.request.get(path)).status()).toBe(404);
  }
  await page.goto('/console#/tasks/foreign-task');
  await expect(page.getByRole('heading', { name: '任务加载失败' })).toBeVisible();
  await expect(page.getByText('Foreign private task')).toHaveCount(0);
});

test('401 during detail loading preserves login instead of stale task content', async ({ page }) => {
  await login(page);
  await page.route('**/api/tasks/long-trace/trace?*', route => route.fulfill({ status: 401, json: { error: 'fixture expired identity' } }));
  await page.goto('/console#/tasks/long-trace');
  await expect(page.getByRole('heading', { name: '登录工作台' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Long trace fixture' })).toHaveCount(0);
  await expect(page.getByRole('heading', { name: '任务加载失败' })).toHaveCount(0);
});

// Decision outcomes are browser-network fixtures; auth and CSRF above are real.
async function approvalFixture(page, { status = 202, preview = 'Write <script>window.fixtureXSS=true</script>' } = {}) {
  const approval = { id: 'e2e-approval', task_id: 'e2e-task', action: 'write_file', status: 'pending',
    risk_level: 'high', preview, parameter_summary: ['path: fixture.txt'] };
  const decisions = [];
  let reads = 0;
  await page.route('**/api/approvals/e2e-approval', async route => {
    reads++;
    await route.fulfill({ json: approval });
  });
  await page.route(/\/api\/tasks\/e2e-task\/(approve|reject)$/, async route => {
    decisions.push({ action: route.request().url().split('/').pop(), body: route.request().postDataJSON(),
      csrf: route.request().headers()['x-console-csrf'] });
    if (status === 409) approval.status = 'consumed';
    if (status === 410) approval.status = 'expired';
    await route.fulfill({ status, json: { message: 'fixture decision', error: status >= 400 ? 'fixture conflict' : undefined } });
  });
  await page.goto('/console#/approvals/e2e-approval');
  await expect(page.getByRole('heading', { name: '处理审批' })).toBeVisible();
  return { decisions, reads: () => reads };
}

for (const [status, expectedState] of [[202, 'pending'], [409, 'consumed'], [410, 'expired']]) {
  test(`approval ${status} reconciles persisted state without duplicate submission`, async ({ page }) => {
    await login(page);
    const fixture = await approvalFixture(page, { status });
    await page.getByRole('button', { name: '批准', exact: true }).click();
    await expect.poll(fixture.reads).toBeGreaterThan(1);
    await expect(page.locator('#view > .toolbar .badge')).toHaveText(expectedState);
    expect(fixture.decisions).toHaveLength(1);
    expect(fixture.decisions[0].body).toEqual({ approval_id: 'e2e-approval', message: '' });
    expect(fixture.decisions[0].csrf).toBeTruthy();
    if (status === 202) {
      await expect(page.getByRole('status')).toContainText('正在确认持久化状态');
      await expect(page.getByRole('button', { name: '批准', exact: true })).toBeEnabled();
    } else {
      await expect(page.getByRole('status')).toContainText('审批状态已变化');
      await expect(page.getByRole('button', { name: '批准', exact: true })).toHaveCount(0);
    }
    expect(await page.evaluate(() => window.fixtureXSS)).toBeUndefined();
  });
}

test('missing preview disables approval; rejection requires a reason', async ({ page }) => {
  await login(page);
  const fixture = await approvalFixture(page, { preview: '' });
  await expect(page.getByRole('button', { name: '批准', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: '拒绝', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('请填写拒绝原因');
  expect(fixture.decisions).toHaveLength(0);
  await page.getByLabel('说明 / 拒绝原因').fill('Fixture rejection reason');
  await page.getByRole('button', { name: '拒绝', exact: true }).click();
  await expect.poll(() => fixture.decisions.length).toBe(1);
  expect(fixture.decisions[0].action).toBe('reject');
  expect(fixture.decisions[0].body.message).toBe('Fixture rejection reason');
});

test('long Trace virtualizes, scrolls, filters, expands and pages without executing HTML', async ({ page }) => {
  await login(page);
  await page.goto('/console#/tasks/long-trace');
  const viewport = page.getByRole('region', { name: '当前页 Trace 记录，滚动查看更多' });
  await expect(viewport).toBeVisible();
  await expect.poll(() => page.locator('.trace-item').count()).toBeGreaterThan(0);
  expect(await page.locator('.trace-item').count()).toBeLessThan(20);
  await viewport.focus();
  await page.keyboard.press('End');
  await expect(page.locator('.trace-item').last()).toContainText('Trace row 100');
  await page.getByLabel('仅错误', { exact: true }).check();
  await expect(page.locator('.trace-item')).toHaveCount(1);
  await expect(page.locator('.trace-item')).toContainText('Fixture read failure');
  await page.getByLabel('仅错误', { exact: true }).uncheck();
  await page.getByLabel('筛选 Trace', { exact: true }).fill('no-such-action');
  await expect(page.getByText('没有匹配记录', { exact: true })).toBeVisible();
  await page.getByLabel('筛选 Trace', { exact: true }).fill('read_file');
  await page.locator('.trace-item summary').first().click();
  await expect(page.locator('.trace-item').first()).toHaveClass(/expanded/);
  await expect(page.locator('.trace-item').first()).toContainText('事件 ID：');
  expect(await page.evaluate(() => window.fixtureXSS)).toBeUndefined();
  await page.getByRole('button', { name: '下一页', exact: true }).click();
  await expect(page.getByRole('heading', { name: '执行 Trace · 第 2 页', exact: true })).toBeVisible();
  await expect(page.locator('.trace-item').first()).toContainText('Trace row 101');
});

test('SSE interruption recovers terminal state through persistent polling', async ({ page, context }) => {
  await login(page);
  await page.clock.install();
  await page.route('**/stream', route => route.abort());
  const id = await createTask(page, 'browser-cancel-poll');
  await page.getByRole('button', { name: '运行任务', exact: true }).click();
  await expect(taskStatus(page)).toHaveText('running');
  const session = await (await context.request.get('/console/session')).json();
  const response = await context.request.delete(`/api/tasks/${id}/cancel`, { headers: { 'X-Console-CSRF': session.csrf_token } });
  expect(response.ok()).toBe(true);
  await page.clock.fastForward(8000);
  await expect(taskStatus(page)).toHaveText('failed');
});

test('signed-out navigation stays hidden and keyboard login moves focus to content', async ({ page, browserName }) => {
  // macOS WebKit uses Option-Tab to include links in native keyboard navigation.
  const tab = browserName === 'webkit' && process.platform === 'darwin' ? 'Alt+Tab' : 'Tab';
  const reverseTab = browserName === 'webkit' && process.platform === 'darwin' ? 'Alt+Shift+Tab' : 'Shift+Tab';
  await page.goto('/console');
  await expect(page.getByRole('heading', { name: '登录工作台' })).toBeVisible();
  await expect(page.locator('#nav')).toBeHidden();
  await expect(page.getByLabel('Bearer Token')).toBeFocused();
  await page.keyboard.press(reverseTab); // Brand.
  await page.keyboard.press(reverseTab);
  await expect(page.getByRole('link', { name: '跳转到主要内容', exact: true })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.locator('#view')).toBeFocused();
  await expect(page.getByRole('link', { name: '跳转到主要内容', exact: true })).toHaveCSS('opacity', '0');
  await page.keyboard.press(tab);
  await expect(page.getByLabel('Bearer Token')).toBeFocused();
  await page.keyboard.insertText(token);
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: '任务管理', exact: true })).toBeVisible();
  await expect(page.locator('#view')).toBeFocused();
  await expect(page.locator('#nav a[data-nav="tasks"]')).toHaveAttribute('aria-current', 'page');
  await page.keyboard.press(tab);
  await expect(page.getByRole('link', { name: '新建任务', exact: true })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: '创建任务', exact: true })).toBeVisible();
  await expect(page.locator('#view')).toBeFocused();
  await page.keyboard.press(tab); // Back link.
  await page.keyboard.press(tab);
  await expect(page.getByLabel('任务目标')).toBeFocused();
});

test('keyboard Trace expansion and collapse retain focus', async ({ page }) => {
  await login(page);
  await page.goto('/console#/tasks/long-trace');
  const first = page.locator('.trace-item summary').first();
  await first.focus();
  await page.keyboard.press('Enter');
  await expect(page.locator('.trace-item').first()).toHaveClass(/expanded/);
  await expect(first).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.locator('.trace-item').first()).not.toHaveClass(/expanded/);
  await expect(first).toBeFocused();
});

for (const width of [320, 390]) {
  test(`narrow ${width}px task, Trace and approval content stay within the viewport`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 844 });
    await login(page);
    const fits = () => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth);
    await expect.poll(fits).toBe(true);
    await page.goto('/console#/tasks/long-trace');
    await expect(page.getByRole('heading', { name: 'Long trace fixture' })).toBeVisible();
    await expect.poll(fits).toBe(true);
    const viewport = page.getByRole('region', { name: '当前页 Trace 记录，滚动查看更多' });
    await viewport.focus();
    await page.keyboard.press('End');
    await expect(page.locator('.trace-item').last()).toContainText('Trace row 100');
    await page.screenshot({ path: testInfo.outputPath(`trace-${width}.png`), fullPage: true });
    await approvalFixture(page, { preview: 'Read-only narrow preview\n' + 'long-text-'.repeat(80) });
    await expect.poll(fits).toBe(true);
    await page.getByRole('button', { name: '批准', exact: true }).scrollIntoViewIfNeeded();
    await expect(page.getByRole('button', { name: '批准', exact: true })).toBeInViewport();
  });
}

for (const signal of ['open', 'error']) {
  test(`SSE ${signal} immediately reconciles durable Task, Trace and approvals`, async ({ page, context }) => {
    // Drive browser connection lifecycle deterministically while every read
    // and cancel request still goes through the real authenticated API.
    await page.addInitScript(() => {
      window.fixtureStreams = [];
      window.EventSource = class extends EventTarget {
        constructor(url) { super(); this.url = url; this.closed = false; window.fixtureStreams.push(this); }
        close() { this.closed = true; }
      };
    });
    await login(page);
    await page.clock.install();
    const id = await createTask(page, `browser-cancel-reconcile-${signal}`);
    await page.getByRole('button', { name: '运行任务', exact: true }).click();
    await expect(taskStatus(page)).toHaveText('running');
    const reads = new Set();
    page.on('request', request => {
      const url = new URL(request.url());
      if (url.pathname.startsWith(`/api/tasks/${id}`)) reads.add(url.pathname);
    });
    const session = await (await context.request.get('/console/session')).json();
    expect((await context.request.delete(`/api/tasks/${id}/cancel`, { headers: { 'X-Console-CSRF': session.csrf_token } })).ok()).toBe(true);
    await page.evaluate(signal => window.fixtureStreams.findLast(stream => !stream.closed).dispatchEvent(new Event(signal)), signal);
    await page.clock.fastForward(300);
    await expect(taskStatus(page)).toHaveText('failed');
    expect(reads.has(`/api/tasks/${id}`)).toBe(true);
    expect(reads.has(`/api/tasks/${id}/trace`)).toBe(true);
    expect(reads.has(`/api/tasks/${id}/approvals`)).toBe(true);
    expect(await page.evaluate(() => window.fixtureStreams.every(stream => stream.closed))).toBe(true);
  });
}
