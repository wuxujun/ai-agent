import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { plan, provision, verifyFreeAccount } from './provision.mjs';

const config = JSON.parse(await readFile(new URL('./topology.example.json', import.meta.url)));
const boundedConfig = { ...config, runtimeBudget: { maxRunSeconds: 14400, maxRunTotalSeconds: 21600 } };
const apiKey = 'offline-fake-key';
const freeOptions = () => ({ apiKey, accountReview: { source: 'freestyle-dashboard', accountReference: 'acct-fixture',
  plan: 'free', paidOverageEnabled: false, quotaSufficient: true, checkedAt: new Date().toISOString(),
  apiKeySha256: createHash('sha256').update(apiKey).digest('hex'), evidenceReference: 'offline-test-evidence' } });

test('offline plan requires distinct private nodes and retains hard host anti-affinity', () => {
  const spec = plan(config);
  assert.equal(spec.placement.antiAffinity[0].topology, 'node');
  assert.equal(spec.publicInbound, false);
  assert.equal(spec.rules.length, 15);
  assert.deepEqual(spec.rules.filter(r => r.port === 9464), [
    { source: 'control', destination: 'node-a', port: 9464 },
    { source: 'control', destination: 'node-b', port: 9464 },
  ]);
  assert.equal(spec.budgetControl.runtimeLimitConfigured, true);
  assert.equal(spec.spendingPolicy.maxPaidSpendUsd, 0);
  assert.throws(() => plan({ ...config, nodes: { ...config.nodes, 'node-b': config.nodes['node-a'] } }));
  assert.throws(() => plan({ ...config, cidr: '8.8.8.0/24' }));
  assert.throws(() => plan({ ...config, cidr: '10.77.33.1/24' }));
  assert.throws(() => plan({ ...config, snapshot: 'freestyle/busybox' }));
  assert.throws(() => plan({ ...config, snapshot: 'freestyle/ubuntu-lg' }));
  assert.throws(() => plan({ ...config, spendingPolicy: { mode: 'paid', maxPaidSpendUsd: 5 } }));
});

test('dry-run works without credentials, SDK install or output writes', () => {
  const result = spawnSync(process.execPath, [new URL('./provision.mjs', import.meta.url).pathname,
    '--config', new URL('./topology.example.json', import.meta.url).pathname],
  { env: { PATH: process.env.PATH }, encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout).full_ha_acceptance, false);
});

function fakeClient(dropPlacement = false, failRole = '') {
  const calls = [];
  return { calls, vpc: { async create(options) { calls.push(['vpc', options]); return { vpcId: 'vpc-test' }; } },
    vms: { async create(options) {
      calls.push(['vm', options]);
      if (options.slug.endsWith(failRole) && failRole) throw Error('capacity');
      return { vmId: 'vm-' + options.slug, data: { ...options, placement: dropPlacement ? null : options.placement } };
    } }, firewall: { rules: { async create(options) { calls.push(['rule', options]); return { id: 'rule-' + calls.length }; } } } };
}

test('create checkpoints resource IDs and restricts inter-VM ports', async () => {
  const client = fakeClient();
  const snapshots = [];
  const state = await provision(client, plan(boundedConfig), s => snapshots.push(structuredClone(s)), freeOptions());
  assert.equal(Object.keys(state.nodes).length, 4);
  assert.equal(state.rules.length, 15);
  assert.equal(state.status, 'created-not-accepted');
  assert.equal(snapshots[1].vpc.id, 'vpc-test');
  assert.deepEqual(client.calls[0][1].firewall.rules, []);
  for (const [, options] of client.calls.filter(([kind]) => kind === 'vm')) {
    assert.equal(options.maxRunSeconds, 14400);
    assert.equal(options.maxRunTotalSeconds, 21600);
    assert.equal(options.ttlSeconds, -1);
    assert.equal(options.autoDeleteSeconds, -1);
  }
  for (const [, options] of client.calls.filter(([kind]) => kind === 'rule')) {
    assert.equal(options.source.public, undefined);
    assert.ok([15432, 16379, 2049, 8088, 9464, 18080].includes(options.destination.port));
  }
});

test('missing placement and partial creation fail without retrying or dropping constraints', async () => {
  for (const client of [fakeClient(true), fakeClient(false, 'node-b')]) {
    const snapshots = [];
    await assert.rejects(provision(client, plan(boundedConfig), s => snapshots.push(structuredClone(s)), freeOptions()));
    assert.ok(snapshots.at(-1).nodes['node-a'].id);
    assert.equal(client.calls.filter(([kind]) => kind === 'rule').length, 0);
    assert.ok(client.calls.filter(([kind]) => kind === 'vm').length <= 2);
  }
});

test('unconfigured or invalid runtime budgets fail before any cloud resources', async () => {
  const client = fakeClient();
  const snapshots = [];
  await assert.rejects(provision(client, plan({ ...config, runtimeBudget: null }), s => snapshots.push(s)), /finite cumulative/);
  assert.deepEqual(client.calls, []);
  assert.deepEqual(snapshots, []);
  for (const runtimeBudget of [
    { maxRunSeconds: -1, maxRunTotalSeconds: -1 },
    { maxRunSeconds: 0, maxRunTotalSeconds: 3600 },
    { maxRunSeconds: 7200, maxRunTotalSeconds: 3600 },
    { maxRunSeconds: 3600, maxRunTotalSeconds: null },
    { maxRunSeconds: 1.5, maxRunTotalSeconds: 3600 },
  ]) assert.throws(() => plan({ ...config, runtimeBudget }));
});

test('missing or expanded server runtime limits stop with a resource checkpoint', async () => {
  for (const returned of [undefined, -1, 21601]) {
    const client = fakeClient();
    const create = client.vms.create;
    client.vms.create = async options => {
      const result = await create(options);
      result.data.maxRunTotalSeconds = returned;
      return result;
    };
    const snapshots = [];
    await assert.rejects(provision(client, plan(boundedConfig), s => snapshots.push(structuredClone(s)), freeOptions()), /Runtime budget/);
    assert.ok(snapshots.at(-1).nodes['node-a'].id);
    assert.equal(client.calls.filter(([kind]) => kind === 'vm').length, 1);
    assert.equal(client.calls.filter(([kind]) => kind === 'rule').length, 0);
  }
});

test('unknown, paid, stale, wrong-key or insufficient-quota reviews create no resources', async () => {
  const now = Date.now();
  const valid = freeOptions().accountReview;
  assert.equal(verifyFreeAccount(valid, apiKey, now + 1000).plan, 'free');
  for (const review of [undefined, { ...valid, plan: 'hobby' }, { ...valid, paidOverageEnabled: true },
    { ...valid, quotaSufficient: false }, { ...valid, apiKeySha256: 'wrong-key' },
    { ...valid, checkedAt: new Date(now - 31 * 60 * 1000).toISOString() },
    { ...valid, checkedAt: new Date(now + 60000).toISOString() }]) {
    const client = fakeClient();
    const snapshots = [];
    await assert.rejects(provision(client, plan(config), s => snapshots.push(s), { accountReview: review, apiKey }), /Free-account/);
    assert.deepEqual(client.calls, []);
    assert.deepEqual(snapshots, []);
  }
});

test('explicit user-confirmed Free plan retains unknown remaining quota and enforces the monthly CPU ceiling', async () => {
  const options = freeOptions();
  options.accountReview = { ...options.accountReview, source: 'user-confirmation', accountReference: null,
    credentialReference: 'root-env', quotaSufficient: null, quotaPolicy: 'stop-on-platform-limit',
    monthlyCpuAllowanceHours: 200, evidenceReference: 'user-confirmed-free-plan-and-200-cpu-hours' };
  const state = await provision(fakeClient(), plan(config), () => {}, options);
  assert.equal(state.freeAccountReview.source, 'user-confirmation');
  assert.equal(state.freeAccountReview.quotaSufficient, null);
  assert.equal(state.freeAccountReview.accountReference, null);
  assert.equal(state.freeAccountReview.monthlyCpuAllowanceHours, 200);
  assert.equal(state.freeAccountReview.apiKeySha256, undefined);
  assert.equal(verifyFreeAccount(options.accountReview, apiKey, Date.now() + 60 * 60 * 1000).plan, 'free');
  assert.throws(() => verifyFreeAccount(options.accountReview, apiKey, Date.now() + 25 * 60 * 60 * 1000));
  const client = fakeClient();
  await assert.rejects(provision(client, plan({ ...config,
    runtimeBudget: { maxRunSeconds: 21600, maxRunTotalSeconds: 46800 } }), () => {}, options), /CPU time/);
  assert.deepEqual(client.calls, []);
  for (const change of [{ plan: 'hobby' }, { quotaSufficient: false }, { quotaPolicy: null },
    { monthlyCpuAllowanceHours: null }, { credentialReference: null }, { source: 'unknown' }]) {
    assert.throws(() => verifyFreeAccount({ ...options.accountReview, ...change }, apiKey));
  }
});
