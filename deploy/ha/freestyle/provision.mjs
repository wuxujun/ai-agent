#!/usr/bin/env node
// Plan is entirely local. Only --apply loads the SDK or contacts Freestyle.
import { readFile, mkdir, writeFile, realpath, lstat } from 'node:fs/promises';
import { resolve, dirname, basename } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createHash } from 'node:crypto';

export const roles = ['node-a', 'node-b', 'storage', 'control'];
const canonical = value => JSON.stringify(value, function (key, item) {
  return item && typeof item === 'object' && !Array.isArray(item)
    ? Object.fromEntries(Object.entries(item).sort(([a], [b]) => a.localeCompare(b))) : item;
});

function ipv4(value) {
  if (typeof value !== 'string' || !/^(\d{1,3}\.){3}\d{1,3}$/.test(value)) throw Error('Invalid IPv4');
  const parts = value.split('.').map(Number);
  if (parts.some((n, i) => n > 255 || String(n) !== value.split('.')[i])) throw Error('Invalid IPv4');
  return parts;
}

export function plan(config) {
  if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(config.prefix) || config.prefix.length > 50) throw Error('Invalid prefix');
  const network = ipv4(String(config.cidr).split('/')[0]);
  if (config.cidr !== network.join('.') + '/24' || network[3] !== 0 ||
      !(network[0] === 10 || (network[0] === 172 && network[1] >= 16 && network[1] <= 31) ||
        (network[0] === 192 && network[1] === 168))) throw Error('Use a canonical RFC1918 /24');
  if (!['freestyle/ubuntu-sm', 'freestyle/ubuntu', 'freestyle/ubuntu-lg'].includes(config.snapshot)) throw Error('Use an Ubuntu snapshot');
  if (config.snapshot === 'freestyle/ubuntu-lg') throw Error('Snapshot exceeds the Free plan');
  if (config.spendingPolicy != null &&
      (config.spendingPolicy.mode !== 'free-only' || config.spendingPolicy.maxPaidSpendUsd !== 0)) {
    throw Error('This deployment only permits Free accounts and zero paid spend');
  }
  if (!config.nodes || Object.keys(config.nodes).sort().join() !== [...roles].sort().join()) throw Error('Four named roles are required');
  const addresses = roles.map(role => config.nodes[role]);
  for (const address of addresses) {
    const p = ipv4(address);
    if (p.slice(0, 3).join() !== network.slice(0, 3).join() || p[3] < 2 || p[3] > 254) throw Error('Node outside usable subnet');
  }
  if (new Set(addresses).size !== 4) throw Error('Node addresses must be distinct');
  const budget = config.runtimeBudget;
  let maxRunSeconds = null;
  let maxRunTotalSeconds = null;
  if (budget != null) {
    if (typeof budget !== 'object' || Array.isArray(budget)) throw Error('Invalid runtime budget');
    if (budget.maxRunSeconds != null || budget.maxRunTotalSeconds != null) {
      maxRunSeconds = budget.maxRunSeconds;
      maxRunTotalSeconds = budget.maxRunTotalSeconds;
      if (!Number.isSafeInteger(maxRunSeconds) || maxRunSeconds <= 0 ||
          !Number.isSafeInteger(maxRunTotalSeconds) || maxRunTotalSeconds < maxRunSeconds) {
        throw Error('Configure positive per-run and cumulative runtime limits');
      }
    }
  }
  const rules = [];
  for (const source of ['node-a', 'node-b', 'control']) {
    for (const port of [15432, 16379, 2049]) rules.push({ source, destination: 'storage', port });
  }
  for (const node of ['node-a', 'node-b']) {
    rules.push({ source: 'control', destination: node, port: 8088 });
    rules.push({ source: 'control', destination: node, port: 9464 });
    rules.push({ source: node, destination: 'control', port: 18080 });
  }
  return { ...config, vpcSlug: config.prefix + '-vpc', roles, rules,
    spendingPolicy: { mode: 'free-only', maxPaidSpendUsd: 0, allowPlanUpgrade: false },
    placement: { antiAffinity: [{ topology: 'node', selector: { matchLabels: { ha_cluster: config.prefix, ha_role: 'agent' } } }] },
    lifecycle: { idleTimeoutSeconds: -1, ttlSeconds: -1, maxRunSeconds, maxRunTotalSeconds, autoDeleteSeconds: -1 },
    budgetControl: { runtimeLimitConfigured: maxRunTotalSeconds !== null, fullDollarSpendCapEnforced: false },
    publicInbound: false, packageEgress: ['tcp/80', 'tcp/443', 'udp/53', 'tcp/53'], full_ha_acceptance: false };
}

export function verifyFreeAccount(review, apiKey, now = Date.now()) {
  const checked = Date.parse(review?.checkedAt);
  const dashboard = review?.source === 'freestyle-dashboard';
  const confirmedByUser = review?.source === 'user-confirmation';
  const referenceValid = dashboard
    ? typeof review.accountReference === 'string' && /^[a-zA-Z0-9_-]{1,128}$/.test(review.accountReference)
    : confirmedByUser && typeof review.credentialReference === 'string' &&
      /^[a-zA-Z0-9_-]{1,128}$/.test(review.credentialReference) && review.accountReference === null;
  // Free hard allowances cannot turn into paid usage. Unknown remaining allowance
  // can interrupt acceptance, so retain the uncertainty and stop on platform limits.
  const unknownAllowance = confirmedByUser && review.quotaSufficient === null &&
    review.quotaPolicy === 'stop-on-platform-limit' &&
    Number.isSafeInteger(review.monthlyCpuAllowanceHours) && review.monthlyCpuAllowanceHours > 0;
  // User confirmation persists for this day's continuation; dashboard quota
  // snapshots remain short-lived because observed remaining usage changes.
  const maxAge = confirmedByUser ? 24 * 60 * 60 * 1000 : 30 * 60 * 1000;
  if (!apiKey || review?.plan !== 'free' || review.paidOverageEnabled !== false ||
      (review.quotaSufficient !== true && !unknownAllowance) || !referenceValid ||
      typeof review.evidenceReference !== 'string' || !review.evidenceReference.trim() ||
      !Number.isFinite(checked) || checked > now || now - checked > maxAge ||
      review.apiKeySha256 !== createHash('sha256').update(apiKey).digest('hex')) {
    throw Error('A fresh Free-account and quota review bound to this API key is required');
  }
  // This is a recorded dashboard review or explicit user confirmation, never an API billing response.
  return { plan: 'free', paidOverageEnabled: false, quotaSufficient: review.quotaSufficient,
    source: review.source, accountReference: review.accountReference,
    ...(confirmedByUser ? { credentialReference: review.credentialReference,
      monthlyCpuAllowanceHours: review.monthlyCpuAllowanceHours ?? null,
      quotaPolicy: review.quotaPolicy ?? null } : {}),
    checkedAt: review.checkedAt, evidenceReference: review.evidenceReference };
}

export async function provision(client, spec, checkpoint, options = {}) {
  if (!Number.isSafeInteger(spec.lifecycle.maxRunSeconds) || spec.lifecycle.maxRunSeconds <= 0 ||
      !Number.isSafeInteger(spec.lifecycle.maxRunTotalSeconds) ||
      spec.lifecycle.maxRunTotalSeconds < spec.lifecycle.maxRunSeconds) {
    throw Error('Resource creation requires a finite cumulative runtime budget');
  }
  if (spec.spendingPolicy?.mode !== 'free-only' || spec.spendingPolicy.maxPaidSpendUsd !== 0) {
    throw Error('Free-only spending policy is required');
  }
  const freeAccountReview = verifyFreeAccount(options.accountReview, options.apiKey);
  if (freeAccountReview.quotaSufficient === null) {
    const cpu = spec.snapshot === 'freestyle/ubuntu-sm' ? 2 : 4;
    if (roles.length * cpu * spec.lifecycle.maxRunTotalSeconds / 3600 > freeAccountReview.monthlyCpuAllowanceHours) {
      throw Error('Requested cumulative CPU time exceeds the confirmed monthly allowance');
    }
  }
  const state = { schema: 1, status: 'creating', sdk: '0.2.16', full_ha_acceptance: false,
    requestedLifecycle: spec.lifecycle, budgetControl: spec.budgetControl,
    spendingPolicy: spec.spendingPolicy, freeAccountReview, vpc: null, nodes: {}, rules: [] };
  await checkpoint(state);
  const vpc = await client.vpc.create({ slug: spec.vpcSlug, cidr: spec.cidr, firewall: { rules: [] } });
  state.vpc = { id: vpc.vpcId, slug: spec.vpcSlug, cidr: spec.cidr };
  await checkpoint(state);
  for (const role of roles) {
    const agent = role.startsWith('node-');
    const created = await client.vms.create({
      slug: spec.prefix + '-' + role, snapshotId: spec.snapshot,
      metadata: { ha_cluster: spec.prefix, ha_role: agent ? 'agent' : role },
      ...spec.lifecycle,
      ...(agent ? { placement: spec.placement } : {}),
      vpcs: [{ vpcId: vpc.vpcId, ipv4: spec.nodes[role], ipv6: false }],
      firewall: { rules: [[80, 'tcp'], [443, 'tcp'], [53, 'udp'], [53, 'tcp']].map(([port, protocol]) => ({
        action: 'allow', source: {}, destination: { public: true, port, protocol },
      })) },
    });
    state.nodes[role] = { id: created.vmId, slug: spec.prefix + '-' + role, ipv4: spec.nodes[role],
      placement: created.data.placement ?? null, snapshotId: created.data.snapshotId ?? null,
      resources: created.data.resources ?? null,
      effectiveLifecycle: Object.fromEntries(Object.keys(spec.lifecycle).map(k => [k, created.data[k] ?? null])) };
    await checkpoint(state); // Keep IDs even when a subsequent policy check fails.
    const data = created.data;
    if (!data.vpcs?.some(n => (n.vpcId ?? n.vpc) === vpc.vpcId && n.ipv4 === spec.nodes[role])) throw Error('Network attachment mismatch');
    if (agent && canonical(data.placement) !== canonical(spec.placement)) throw Error('Anti-affinity not confirmed');
    for (const field of ['maxRunSeconds', 'maxRunTotalSeconds']) {
      if (data[field] !== spec.lifecycle[field]) throw Error('Runtime budget not confirmed');
    }
    for (const field of ['idleTimeoutSeconds', 'ttlSeconds']) {
      if (data[field] != null && data[field] !== -1) throw Error('Unexpected runtime/deletion limit');
    }
    if (data.autoDeleteSeconds === 0) throw Error('VM is ephemeral');
  }
  for (const rule of spec.rules) {
    const created = await client.firewall.rules.create({ action: 'allow',
      source: { vmId: state.nodes[rule.source].id },
      destination: { vmId: state.nodes[rule.destination].id, port: rule.port, protocol: 'tcp' },
      description: `${spec.prefix}: ${rule.source} to ${rule.destination}:${rule.port}` });
    state.rules.push({ id: created.id, ...rule });
    await checkpoint(state);
  }
  state.status = 'created-not-accepted';
  await checkpoint(state);
  return state;
}

async function main() {
  const args = process.argv.slice(2);
  const options = {};
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--apply') options.apply = true;
    else if (['--config', '--output', '--account-review'].includes(args[i]) && args[i + 1]) options[args[i].slice(2)] = args[++i];
    else throw Error('usage: node provision.mjs --config topology.json [--account-review PRIVATE_JSON --output NEW_DIRECTORY --apply]');
  }
  if (!options.config) throw Error('--config is required');
  const spec = plan(JSON.parse(await readFile(options.config, 'utf8')));
  if (!options.apply) { process.stdout.write(JSON.stringify(spec, null, 2) + '\n'); return; }
  if (!process.env.FREESTYLE_API_KEY || !options.output || !options['account-review']) {
    throw Error('Apply requires FREESTYLE_API_KEY, a Free account review and a new output directory');
  }
  const reviewPath = options['account-review'];
  const reviewStat = await lstat(reviewPath);
  if (!reviewStat.isFile() || (reviewStat.mode & 0o077)) throw Error('Account review must be a private regular file');
  const accountReview = JSON.parse(await readFile(reviewPath, 'utf8'));
  verifyFreeAccount(accountReview, process.env.FREESTYLE_API_KEY);
  const kit = dirname(fileURLToPath(import.meta.url));
  const repo = resolve(kit, '../../..');
  const requested = resolve(options.output);
  try {
    await lstat(requested);
    throw Error('Output already exists');
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
  const output = resolve(await realpath(dirname(requested)), basename(requested));
  if (output === repo || output.startsWith(repo + '/')) throw Error('Evidence must be outside the repository');
  const pkg = JSON.parse(await readFile(resolve(kit, 'node_modules/freestyle/package.json'), 'utf8'));
  if (pkg.version !== '0.2.16') throw Error('Install pinned freestyle 0.2.16 beside this script');
  const { Freestyle } = await import('freestyle');
  await mkdir(output, { mode: 0o700 }); // Exclusive: never reuse a partial run.
  await writeFile(resolve(output, 'plan.json'), JSON.stringify(spec, null, 2) + '\n', { mode: 0o600, flag: 'wx' });
  let sequence = 0;
  await provision(new Freestyle({ apiKey: process.env.FREESTYLE_API_KEY }), spec, async state => {
    await writeFile(resolve(output, `state-${String(sequence++).padStart(3, '0')}.json`),
      JSON.stringify(state, null, 2) + '\n', { mode: 0o600, flag: 'wx' });
  }, { accountReview, apiKey: process.env.FREESTYLE_API_KEY });
  console.log('Resources created; retain state files and verify effective policies before bootstrap.');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(() => {
    // SDK errors can include response bodies/headers. Do not echo them.
    console.error('Provisioning stopped. Check input, SDK version, account limits and saved state. No resources were deleted or replaced.');
    process.exitCode = 2;
  });
}
