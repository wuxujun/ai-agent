import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { plan, provision } from './provision.mjs';

const config = JSON.parse(await readFile(new URL('./topology.example.json', import.meta.url)));

test('offline plan requires distinct private nodes and retains hard host anti-affinity', () => {
  const spec = plan(config);
  assert.equal(spec.placement.antiAffinity[0].topology, 'node');
  assert.equal(spec.publicInbound, false);
  assert.equal(spec.rules.length, 13);
  assert.throws(() => plan({ ...config, nodes: { ...config.nodes, 'node-b': config.nodes['node-a'] } }));
  assert.throws(() => plan({ ...config, cidr: '8.8.8.0/24' }));
  assert.throws(() => plan({ ...config, cidr: '10.77.33.1/24' }));
  assert.throws(() => plan({ ...config, snapshot: 'freestyle/busybox' }));
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
  const state = await provision(client, plan(config), s => snapshots.push(structuredClone(s)));
  assert.equal(Object.keys(state.nodes).length, 4);
  assert.equal(state.rules.length, 13);
  assert.equal(state.status, 'created-not-accepted');
  assert.equal(snapshots[1].vpc.id, 'vpc-test');
  assert.deepEqual(client.calls[0][1].firewall.rules, []);
  for (const [, options] of client.calls.filter(([kind]) => kind === 'rule')) {
    assert.equal(options.source.public, undefined);
    assert.ok([15432, 16379, 2049, 8088, 18080].includes(options.destination.port));
  }
});

test('missing placement and partial creation fail without retrying or dropping constraints', async () => {
  for (const client of [fakeClient(true), fakeClient(false, 'node-b')]) {
    const snapshots = [];
    await assert.rejects(provision(client, plan(config), s => snapshots.push(structuredClone(s))));
    assert.ok(snapshots.at(-1).nodes['node-a'].id);
    assert.equal(client.calls.filter(([kind]) => kind === 'rule').length, 0);
    assert.ok(client.calls.filter(([kind]) => kind === 'vm').length <= 2);
  }
});
