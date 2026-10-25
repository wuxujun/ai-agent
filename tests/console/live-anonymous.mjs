import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { createRequire } from 'node:module';

if (!process.env.UI_LIVE_BASE_URL) {
  throw new Error('UI_LIVE_BASE_URL is required; explicitly select a loopback service.');
}
let target;
try { target = new URL(process.env.UI_LIVE_BASE_URL); }
catch { throw new Error('UI_LIVE_BASE_URL must be a valid loopback HTTP origin.'); }
if (target.protocol !== 'http:' || !['127.0.0.1', 'localhost', '[::1]'].includes(target.hostname)
    || target.username || target.password || target.pathname !== '/' || target.search || target.hash) {
  throw new Error('UI_LIVE_BASE_URL must be a plain loopback HTTP origin without credentials, path, query or fragment.');
}
const selected = process.env.UI_BROWSER;
if (selected && !['chromium', 'firefox', 'webkit'].includes(selected)) {
  throw new Error('UI_BROWSER must be chromium, firefox or webkit.');
}
if (process.env.UI_BROWSER_EXECUTABLE && selected !== 'chromium') {
  throw new Error('UI_BROWSER_EXECUTABLE requires UI_BROWSER=chromium.');
}

const require = createRequire(import.meta.url);
const playwright = require('playwright');
const origin = target.origin;
const output = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-agent-console-live-'));
const engines = selected ? [selected] : ['chromium', 'firefox', 'webkit'];
const allowedPaths = new Set([
  '/console', '/console/session', '/assets/console/app.js', '/assets/console/style.css', '/favicon.ico',
]);
const expectedHTTPError = /(?:status of|status code) (?:400|401|404)\b|(?:400|401|404) \((?:Bad Request|Unauthorized|Not Found)\)/i;
function check(condition, message) { if (!condition) throw new Error(message); }

async function inspect(engine) {
  const row = {
    engine, passed: false, viewports: [], pageErrors: [], unexpectedConsoleErrors: [],
    blockedRequests: [], unexpectedResponses: [], resources: {},
    screenshots: engine !== 'webkit',
  };
  if (engine === 'webkit') {
    // Playwright 1.58.2 syncs WebKit screenshots by inserting <style>body {}</style>.
    // Keep the application's CSP intact and console checks strict. Other engines
    // retain screenshots; WebKit still verifies all DOM/layout/keyboard behavior.
    row.screenshotSkipReason = 'WebKit screenshot preparation injects a CSP-blocked inline stylesheet';
  }
  let browser;
  try {
    const launchOptions = { headless: true };
    if (engine === 'chromium' && process.env.UI_BROWSER_EXECUTABLE) {
      launchOptions.executablePath = process.env.UI_BROWSER_EXECUTABLE;
    }
    browser = await playwright[engine].launch(launchOptions);
    row.version = browser.version();
    // No persistent profile, cookies, storage state or real credentials.
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
    context.setDefaultTimeout(5000);
    context.setDefaultNavigationTimeout(10000);
    await context.route('**/*', async route => {
      const request = route.request();
      const url = new URL(request.url());
      const headers = request.headers();
      // The app tries UA_TOKEN auto-login after an anonymous 401. With an
      // empty body and no cookies it is rejected before identity lookup.
      const emptySession = request.method() === 'POST' && url.pathname === '/console/session'
        && request.postData() === '{}' && !headers.cookie && !headers.authorization;
      if (url.origin !== origin || !allowedPaths.has(url.pathname)
          || (request.method() !== 'GET' && !emptySession) || headers.authorization) {
        row.blockedRequests.push({
          method: request.method(), sameOrigin: url.origin === origin,
          path: url.origin === origin ? url.pathname : 'external',
        });
        await route.abort();
        return;
      }
      await route.continue();
    });
    const page = await context.newPage();
    page.on('response', response => {
      const url = new URL(response.url());
      if (url.origin !== origin) return;
      const method = response.request().method();
      const status = response.status();
      row.resources[`${method} ${url.pathname}`] = status;
      const expected = url.pathname === '/console/session' ? (method === 'GET' ? 401 : 400)
        : url.pathname === '/favicon.ico' ? 404 : 200;
      if (status !== expected) row.unexpectedResponses.push({ method, path: url.pathname, status });
    });
    page.on('pageerror', error => row.pageErrors.push(error.message.slice(0, 240)));
    page.on('console', message => {
      if (message.type() === 'error' && !expectedHTTPError.test(message.text())) {
        row.unexpectedConsoleErrors.push(message.text().slice(0, 240));
      }
    });
    const response = await page.goto(`${origin}/console`, { waitUntil: 'networkidle' });
    check(response.status() === 200, 'Console did not return 200');
    const login = page.getByRole('heading', { name: '登录工作台', exact: true });
    await login.waitFor();
    const token = page.getByLabel('Bearer Token');
    check(await token.getAttribute('type') === 'password' && await token.inputValue() === '',
      'Token field is not empty and masked');
    check(!await page.locator('#nav').isVisible() && await page.locator('#account').textContent() === '',
      'Anonymous navigation/account exposed');
    check(await token.evaluate(node => node === document.activeElement), 'Login token did not receive initial focus');
    await page.getByRole('button', { name: '登录', exact: true }).click();
    check(await token.evaluate(node => node.validity.valueMissing), 'Empty credential validation missing');

    const reverse = engine === 'webkit' && process.platform === 'darwin' ? 'Alt+Shift+Tab' : 'Shift+Tab';
    await token.focus();
    await page.keyboard.press(reverse);
    await page.keyboard.press(reverse);
    check(await page.getByRole('link', { name: '跳转到主要内容', exact: true })
      .evaluate(node => node === document.activeElement), 'Keyboard skip link unreachable');
    await page.keyboard.press('Enter');
    check(await page.locator('#view').evaluate(node => node === document.activeElement), 'Skip link did not focus main');

    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: width === 1440 ? 1000 : 800 });
      const fits = await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth);
      const loginVisible = await login.isVisible();
      row.viewports.push({ width, noHorizontalOverflow: fits, loginVisible });
      if (row.screenshots) {
        await page.screenshot({ path: path.join(output, `${engine}-${width}.png`), fullPage: true });
      }
      check(fits && loginVisible, `Anonymous layout failed at ${width}`);
    }
    for (const hash of ['#/tasks', '#/approvals', '#/settings']) {
      await page.goto(`${origin}/console${hash}`, { waitUntil: 'networkidle' });
      await login.waitFor();
      check(!await page.locator('#nav').isVisible(), 'Anonymous route exposed navigation');
    }
    check((await context.cookies()).length === 0, 'Anonymous browsing created an authentication cookie');
    // Fixed read-only request; no key or Bearer is attached.
    const denied = await context.request.get(`${origin}/api/teams`);
    check(denied.status() === 401, 'Anonymous protected API was not rejected');
    check(!row.pageErrors.length && !row.unexpectedConsoleErrors.length
      && !row.blockedRequests.length && !row.unexpectedResponses.length,
      'Unexpected browser error or request');
    row.passed = true;
    await context.close();
  } catch (error) {
    row.error = error.message.slice(0, 300);
  } finally {
    if (browser) await browser.close();
  }
  return row;
}

const results = [];
for (const engine of engines) results.push(await inspect(engine));
const report = {
  target: origin, output, results, realTokenUsed: false,
  emptyAutoSessionRequestAllowed: true, modelOrTaskExecutionAllowed: false,
};
fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify(report, null, 2));
console.log(JSON.stringify(report, null, 2));
if (results.some(result => !result.passed)) process.exitCode = 1;
