import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const script = fileURLToPath(new URL('./live-anonymous.mjs', import.meta.url));
const sentinel = 'never-print-fixture-credential';
const cases = [
  ['missing target', undefined, {}, /UI_LIVE_BASE_URL is required/],
  ['remote origin', 'http://example.invalid', {}, /loopback HTTP origin/],
  ['unsupported TLS origin', 'https://localhost:8088', {}, /loopback HTTP origin/],
  ['userinfo', `http://user:${sentinel}@127.0.0.1:8088`, {}, /without credentials/],
  ['path', 'http://127.0.0.1:8088/console', {}, /without credentials/],
  ['query', `http://127.0.0.1:8088/?token=${sentinel}`, {}, /without credentials/],
  ['fragment', `http://127.0.0.1:8088/#${sentinel}`, {}, /without credentials/],
  ['invalid URL', `invalid ${sentinel}`, {}, /valid loopback HTTP origin/],
  ['invalid browser', 'http://127.0.0.1:8088', { UI_BROWSER: 'unknown' }, /UI_BROWSER must be/],
  ['wrong executable override', 'http://127.0.0.1:8088', {
    UI_BROWSER: 'webkit', UI_BROWSER_EXECUTABLE: sentinel,
  }, /UI_BROWSER_EXECUTABLE requires/],
];

for (const [name, target, overrides, message] of cases) {
  test(`anonymous runner rejects ${name} before launching a browser`, () => {
    const env = { ...process.env };
    for (const key of ['UI_LIVE_BASE_URL', 'UI_BROWSER', 'UI_BROWSER_EXECUTABLE']) delete env[key];
    if (target !== undefined) env.UI_LIVE_BASE_URL = target;
    Object.assign(env, overrides);
    const result = spawnSync(process.execPath, [script], { env, encoding: 'utf8', timeout: 3000 });
    assert.equal(result.status, 1);
    assert.equal(result.stdout, '');
    assert.match(result.stderr, message);
    assert.equal(result.stderr.includes(sentinel), false, 'error exposed a credential-bearing input');
  });
}
