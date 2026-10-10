#!/usr/bin/env node
// Observe an existing dedicated run, export private evidence, then pause its VMs.
import { readFile, writeFile, mkdir, lstat, realpath } from 'node:fs/promises';
import { resolve, dirname, basename } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createHash } from 'node:crypto';

const pauseOrder = ['node-a', 'node-b', 'control', 'storage'];
const wait = ms => new Promise(done => setTimeout(done, ms));
const httpStatus = error => Number.isInteger(error?.status) ? error.status : null;
const hash = bytes => createHash('sha256').update(bytes).digest('hex');

export function observationPlan(state, run) {
  if (!/^(smoke|soak)-[a-z0-9]+(?:-[a-z0-9]+)*$/.test(run) || run.length > 48) throw Error('Invalid load run');
  const prefix = state.vpc?.slug?.replace(/-vpc$/, '');
  if (!prefix || !/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(prefix) || prefix.length > 50 ||
      state.spendingPolicy?.mode !== 'free-only' || state.spendingPolicy.maxPaidSpendUsd !== 0 ||
      state.freeAccountReview?.plan !== 'free' || state.freeAccountReview.paidOverageEnabled !== false) {
    throw Error('Use the saved dedicated Free-only creation state');
  }
  const lifecycle = state.requestedLifecycle;
  if (!Number.isSafeInteger(lifecycle?.maxRunSeconds) || lifecycle.maxRunSeconds <= 0 ||
      !Number.isSafeInteger(lifecycle.maxRunTotalSeconds) || lifecycle.maxRunTotalSeconds < lifecycle.maxRunSeconds) {
    throw Error('Finite saved runtime limits required');
  }
  if (Object.keys(state.nodes ?? {}).sort().join() !== [...pauseOrder].sort().join() ||
      new Set(pauseOrder.map(role => state.nodes[role].id)).size !== 4 ||
      pauseOrder.some(role => !/^vm-[a-zA-Z0-9]+$/.test(state.nodes[role].id) || state.nodes[role].slug !== prefix + '-' + role)) {
    throw Error('Four distinct dedicated VM identities required');
  }
  return { prefix, run, nodes: state.nodes, lifecycle, pollSeconds: 45, maximumWatchSeconds: 5400,
    exportsBeforePause: true, pauseOrder, full_ha_acceptance: false };
}

export async function observeAndPause(client, state, run, output, options = {}) {
  const spec = observationPlan(state, run);
  const sleep = options.sleep ?? wait;
  const now = options.now ?? Date.now;
  const progress = options.progress ?? (() => {});
  const handles = {};
  let controlWasRunning = false;
  // Validate every identity before stopping any service. Never substitute a slug.
  for (const role of pauseOrder) {
    const vm = client.vms.ref(state.nodes[role].id);
    const data = await vm.data();
    if (data.id !== state.nodes[role].id || data.slug !== spec.prefix + '-' + role ||
        data.metadata?.ha_cluster !== spec.prefix ||
        data.metadata?.ha_role !== (role.startsWith('node-') ? 'agent' : role) ||
        data.maxRunSeconds !== spec.lifecycle.maxRunSeconds ||
        data.maxRunTotalSeconds !== spec.lifecycle.maxRunTotalSeconds) throw Error('VM identity or budget mismatch');
    handles[role] = vm;
    if (role === 'control') controlWasRunning = data.state === 'running';
  }
  await mkdir(output, { mode: 0o700 }); // Exclusive; preserve partial evidence.
  const save = (name, bytes) => writeFile(resolve(output, name), bytes, { mode: 0o600, flag: 'wx' });
  const manifest = { run, startedAt: new Date(now()).toISOString(), files: [], paused: [], monitorErrors: [],
    full_ha_acceptance: false };
  let sequence = 0;
  const checkpoint = async () => {
    try { await save(`checkpoint-${String(sequence++).padStart(3, '0')}.json`, JSON.stringify(manifest, null, 2) + '\n'); }
    catch { manifest.localWriteErrors = (manifest.localWriteErrors ?? 0) + 1; }
  };
  await checkpoint();
  const base = '/var/lib/ai-agent-ha/results/' + run + '/';
  const control = handles.control;
  const deadline = now() + spec.maximumWatchSeconds * 1000;
  let consecutiveErrors = 0;
  while (controlWasRunning && !manifest.localWriteErrors && now() < deadline) {
    try {
      if ((await control.data()).state !== 'running') { manifest.stopReason = 'control-not-running'; break; }
      if (await control.fs.exists(base + 'status.json')) {
        const bytes = await control.fs.readFile(base + 'status.json', { length: 65536 });
        if (bytes.length) {
          const status = JSON.parse(Buffer.from(bytes).toString('utf8'));
          if (!Number.isInteger(status.exit_code) || status.mode !== run.split('-')[0] || !status.finished_utc) {
            throw Error('Invalid final status');
          }
          manifest.status = status;
          manifest.stopReason = 'run-finished';
          break;
        }
      }
      const bytes = await control.fs.readFile(base + 'load/results.jsonl', { length: 1024 * 1024 });
      if (bytes.length >= 1024 * 1024) throw Error('Progress output bound reached');
      const lines = Buffer.from(bytes).toString('utf8').split('\n');
      lines.pop(); // A partially written final line is still pending.
      const results = lines.filter(Boolean).map(line => JSON.parse(line));
      manifest.progress = { checkedAt: new Date(now()).toISOString(), finished: results.length,
        passed: results.filter(result => result.passed === true).length,
        failed: results.filter(result => result.passed !== true).length };
      progress({ run, ...manifest.progress });
      consecutiveErrors = 0;
      await checkpoint();
    } catch (error) {
      manifest.monitorErrors.push({ at: new Date(now()).toISOString(), httpStatus: httpStatus(error) });
      if (++consecutiveErrors >= 3) { manifest.stopReason = 'three-consecutive-monitor-errors'; break; }
    }
    await sleep(spec.pollSeconds * 1000);
  }
  manifest.stopReason ??= !controlWasRunning ? 'control-not-running' :
    manifest.localWriteErrors ? 'local-checkpoint-write-failed' : 'ninety-minute-watch-deadline';
  // Export bounded raw files privately. No task response or error body is logged.
  let canExport = false;
  try { canExport = (await control.data()).state === 'running'; }
  catch (error) { manifest.exportStateError = { httpStatus: httpStatus(error) }; }
  if (!canExport) manifest.evidenceExportSkipped = 'control-not-running-or-state-unavailable';
  for (const path of canExport ? ['status.json', 'load/report.json', 'load/results.jsonl', 'runner.log'] : []) {
    try {
      const bytes = await control.fs.readFile(base + path, { length: 20 * 1024 * 1024 });
      if (bytes.length >= 20 * 1024 * 1024) throw Error('Evidence output bound reached');
      await save(path.replaceAll('/', '-'), bytes);
      manifest.files.push({ path, bytes: bytes.length, sha256: hash(bytes) });
      if (path === 'load/report.json') manifest.report = JSON.parse(Buffer.from(bytes).toString('utf8'));
    } catch (error) { manifest.files.push({ path, exportFailed: true, httpStatus: httpStatus(error) }); }
  }
  await checkpoint();
  // Export failures must not leave dedicated compute running until the hard cap.
  for (const role of pauseOrder) {
    const vm = handles[role];
    let data;
    try { data = await vm.data(); }
    catch (error) { manifest[role + 'StateError'] = { httpStatus: httpStatus(error) }; }
    if (role.startsWith('node-') && data?.state === 'running') {
      try {
        const result = await vm.linuxUser('root').exec({ command: 'systemctl stop ai-agent.service', timeoutMs: 60000 });
        manifest[role + 'GracefulStopStatus'] = result.statusCode;
      } catch (error) { manifest[role + 'GracefulStopError'] = { httpStatus: httpStatus(error) }; }
    }
    try {
      if (data?.state !== 'paused') data = await vm.pause();
      for (let attempt = 0; data.state === 'pausing' && attempt < 20; attempt++) {
        await sleep(1000);
        data = await vm.data();
      }
      if (data.state !== 'paused' || data.id !== state.nodes[role].id) throw Error('Paused identity not confirmed');
      manifest.paused.push({ role, id: data.id, state: data.state, totalRunSeconds: data.totalRunSeconds,
        maxRunTotalSeconds: data.maxRunTotalSeconds });
      progress({ role, paused: true });
    } catch (error) { manifest.paused.push({ role, pauseFailed: true, httpStatus: httpStatus(error) }); }
    await checkpoint();
  }
  manifest.finishedAt = new Date(now()).toISOString();
  manifest.evidenceExported = manifest.files.length === 4 && manifest.files.every(file => !file.exportFailed);
  manifest.pausesConfirmed = manifest.paused.length === 4 && manifest.paused.every(vm => !vm.pauseFailed);
  manifest.loadChecksPassed = manifest.status?.exit_code === 0 && manifest.report?.load_checks_passed === true &&
    manifest.report?.window_complete === true && manifest.report?.full_ha_acceptance === false;
  const report = manifest.report;
  manifest.smokeChecksPassed = manifest.status?.exit_code === 0 && Number.isSafeInteger(report?.scheduled) &&
    report.scheduled > 0 && report.passed === report.scheduled && report.failed === 0 &&
    Array.isArray(report.by_node) && report.by_node.length === 2 && report.by_node.every(count => count > 0) &&
    report.window_complete === false && report.full_ha_acceptance === false;
  await save('manifest.json', JSON.stringify(manifest, null, 2) + '\n');
  return manifest;
}

async function main() {
  const args = process.argv.slice(2), options = {};
  for (let index = 0; index < args.length; index++) {
    if (args[index] === '--apply') options.apply = true;
    else if (['--state', '--run', '--output'].includes(args[index]) && args[index + 1]) options[args[index].slice(2)] = args[++index];
    else throw Error('usage: node observe-load.mjs --state PRIVATE_JSON --run soak-ID [--output NEW_PRIVATE_DIRECTORY --apply]');
  }
  if (!options.state || !options.run) throw Error('State and run required');
  const info = await lstat(options.state);
  if (!info.isFile() || (info.mode & 0o077)) throw Error('State must be a private regular file');
  const state = JSON.parse(await readFile(options.state, 'utf8'));
  const spec = observationPlan(state, options.run);
  if (!options.apply) { console.log(JSON.stringify(spec, null, 2)); return; }
  if (!process.env.FREESTYLE_API_KEY || !options.output) throw Error('Apply requires key and new private output');
  const kit = dirname(fileURLToPath(import.meta.url));
  const repo = resolve(kit, '../../..');
  const requested = resolve(options.output);
  try { await lstat(requested); throw Error('Output exists'); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  const output = resolve(await realpath(dirname(requested)), basename(requested));
  if (output === repo || output.startsWith(repo + '/')) throw Error('Evidence must be outside repository');
  const pkg = JSON.parse(await readFile(resolve(kit, 'node_modules/freestyle/package.json'), 'utf8'));
  if (pkg.version !== '0.2.16') throw Error('Install pinned SDK 0.2.16');
  const { Freestyle } = await import('freestyle');
  const manifest = await observeAndPause(new Freestyle({ apiKey: process.env.FREESTYLE_API_KEY }), state, options.run,
    output, { progress: value => console.log(JSON.stringify(value)) });
  console.log(JSON.stringify({ run: options.run, loadChecksPassed: manifest.loadChecksPassed,
    evidenceExported: manifest.evidenceExported, pausesConfirmed: manifest.pausesConfirmed, full_ha_acceptance: false }));
  const checksPassed = spec.run.startsWith('soak-') ? manifest.loadChecksPassed : manifest.smokeChecksPassed;
  if (!manifest.evidenceExported || !manifest.pausesConfirmed || !checksPassed || manifest.localWriteErrors) process.exitCode = 2;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(() => {
    console.error('Observation stopped. Inspect saved checkpoints and confirm dedicated VM states. No budgets were changed.');
    process.exitCode = 2;
  });
}
