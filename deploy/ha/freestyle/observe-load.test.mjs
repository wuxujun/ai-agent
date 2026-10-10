import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { observationPlan, observeAndPause } from './observe-load.mjs';

const roles = ['node-a', 'node-b', 'storage', 'control'];
const state = () => ({ vpc: { slug: 'offline-ha-vpc' }, spendingPolicy: { mode: 'free-only', maxPaidSpendUsd: 0 },
  freeAccountReview: { plan: 'free', paidOverageEnabled: false },
  requestedLifecycle: { maxRunSeconds: 21600, maxRunTotalSeconds: 21600 },
  nodes: Object.fromEntries(roles.map((role, i) => [role, { id: 'vm-' + i, slug: 'offline-ha-' + role }])) });
const finalStatus = { mode: 'soak', finished_utc: '2026-10-10T06:06:00Z', exit_code: 0, full_ha_acceptance: false };
const finalReport = { scheduled: 120, passed: 120, failed: 0, by_node: [60, 60], window_complete: true,
  load_checks_passed: true, full_ha_acceptance: false };

function fake({ status = finalStatus, report = finalReport, monitorFails = false, exportFails = false,
  wrongRole = false, stopFails = false, pauseFails = false, alreadyPaused = false } = {}) {
  const events = [], cfg = state();
  const handles = Object.fromEntries(roles.map(role => {
    let paused = alreadyPaused;
    const data = () => ({ id: cfg.nodes[role].id, slug: cfg.nodes[role].slug,
      metadata: { ha_cluster: 'offline-ha', ha_role: wrongRole && role === 'node-b' ? 'other' : role.startsWith('node-') ? 'agent' : role },
      maxRunSeconds: 21600, maxRunTotalSeconds: 21600, totalRunSeconds: 12600, state: paused ? 'paused' : 'running' });
    return [role, { data: async () => data(),
      fs: { exists: async () => { if (monitorFails) throw { status: 500, message: 'secret body' }; return status !== null; },
        readFile: async path => {
          events.push('export:' + path.split('/').at(-1));
          if (exportFails && path.endsWith('report.json')) throw { status: 500, message: 'secret response' };
          return Buffer.from(path.endsWith('status.json') ? JSON.stringify(status) :
            path.endsWith('report.json') ? JSON.stringify(report) : path.endsWith('results.jsonl') ? '' : 'private runner log');
        } },
      linuxUser: () => ({ exec: async () => { events.push('stop:' + role); if (stopFails) throw { status: 500 }; return { statusCode: 0 }; } }),
      pause: async () => { events.push('pause:' + role); if (pauseFails && role === 'node-a') throw { status: 500 };
        paused = true; return data(); } }];
  }));
  let time = 0;
  return { cfg, events, client: { vms: { ref: id => handles[roles.find(role => cfg.nodes[role].id === id)] } },
    options: { now: () => time, sleep: async ms => { time += ms; } } };
}

async function output() {
  // Retain test artifacts: the repository prohibits bulk cleanup.
  return join(await mkdtemp(join(tmpdir(), 'ai-agent-ha-observer-')), 'evidence');
}

test('observation rejects paid, unlimited, duplicate and unrelated identities before cloud actions', async () => {
  for (const change of [s => { s.spendingPolicy.maxPaidSpendUsd = 1; },
    s => { s.freeAccountReview.plan = 'pro'; }, s => { s.requestedLifecycle.maxRunTotalSeconds = null; },
    s => { s.nodes['node-b'].id = s.nodes['node-a'].id; }, s => { s.nodes.control.slug = 'other'; }]) {
    const cfg = state(); change(cfg);
    assert.throws(() => observationPlan(cfg, 'soak-001'));
  }
  assert.throws(() => observationPlan(state(), '../run'));
  const f = fake({ wrongRole: true });
  await assert.rejects(observeAndPause(f.client, f.cfg, 'soak-001', await output(), f.options));
  assert.deepEqual(f.events, []);
});

test('finished load exports evidence privately before graceful stops and all four pauses', async () => {
  const f = fake(), out = await output();
  const result = await observeAndPause(f.client, f.cfg, 'soak-001', out, f.options);
  assert.equal(result.loadChecksPassed, true);
  assert.equal(result.evidenceExported, true);
  assert.equal(result.pausesConfirmed, true);
  assert.equal(result.full_ha_acceptance, false);
  assert.deepEqual(f.events.slice(-6), ['stop:node-a', 'pause:node-a', 'stop:node-b', 'pause:node-b', 'pause:control', 'pause:storage']);
  assert.equal((await stat(out)).mode & 0o077, 0);
  assert.equal((await stat(join(out, 'load-report.json'))).mode & 0o077, 0);
  assert.equal(JSON.parse(await readFile(join(out, 'manifest.json'))).paused.length, 4);
});

test('export, monitor or graceful-stop failures still attempt all dedicated VM pauses', async () => {
  for (const settings of [{ exportFails: true }, { monitorFails: true, status: null }, { stopFails: true }]) {
    const f = fake(settings), result = await observeAndPause(f.client, f.cfg, 'soak-001', await output(), f.options);
    assert.equal(result.pausesConfirmed, true);
    assert.equal(f.events.filter(event => event.startsWith('pause:')).length, 4);
    if (settings.exportFails) assert.equal(result.evidenceExported, false);
    if (settings.monitorFails) {
      assert.equal(result.monitorErrors.length, 3);
      assert.equal(result.loadChecksPassed, false);
      assert.equal(result.stopReason, 'three-consecutive-monitor-errors');
      assert.ok(!JSON.stringify(result).includes('secret body'));
    }
  }
});

test('short or failed windows cannot pass, and one pause failure does not skip other VMs', async () => {
  for (const settings of [{ report: { ...finalReport, window_complete: false } },
    { status: { ...finalStatus, exit_code: 1 } }, { pauseFails: true }]) {
    const f = fake(settings), result = await observeAndPause(f.client, f.cfg, 'soak-001', await output(), f.options);
    assert.equal(result.full_ha_acceptance, false);
    if (settings.pauseFails) {
      assert.equal(result.pausesConfirmed, false);
      assert.equal(f.events.filter(event => event.startsWith('pause:')).length, 4);
    } else assert.equal(result.loadChecksPassed, false);
  }
  const f = fake({ alreadyPaused: true });
  const result = await observeAndPause(f.client, f.cfg, 'soak-001', await output(), f.options);
  assert.equal(result.pausesConfirmed, true);
  assert.deepEqual(f.events, []); // Never exec or read guest files on already paused VMs.
  assert.equal(result.evidenceExported, false);
  assert.equal(result.stopReason, 'control-not-running');
});
