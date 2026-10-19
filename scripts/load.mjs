#!/usr/bin/env node
// Real relay/HTTP load only. No harness starts, provider turns, or benchmark fixtures.
import { spawn, execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdtemp, mkdir, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createServer } from 'node:net';
import assert from 'node:assert/strict';

const exec = promisify(execFile);
const binary = resolve(process.env.MESHRA_BIN || join(dirname(fileURLToPath(import.meta.url)), '../meshra'));
const agents = Number(process.env.MESHRA_LOAD_AGENTS || 8);
assert.ok(Number.isInteger(agents) && agents >= 1 && agents <= 32, 'MESHRA_LOAD_AGENTS must be 1..32');
const dir = await mkdtemp(join(tmpdir(), 'meshra-load-'));
const reservation = createServer();
await new Promise((resolve, reject) => { reservation.once('error', reject); reservation.listen(0, '127.0.0.1', resolve); });
const port = reservation.address().port;
await new Promise(resolve => reservation.close(resolve));
const base = `http://127.0.0.1:${port}`;
const child = spawn(binary, ['--data-dir', join(dir, 'state'), '--addr', `127.0.0.1:${port}`, 'serve', '--retry-base', '30s', '--retry-cap', '30s'], { env: { ...process.env, MESHRA_TOKEN: '' }, stdio: ['ignore', 'ignore', 'pipe'] });
let logs = '';
child.stderr.on('data', chunk => { logs = (logs + chunk).slice(-4096); });
let launchError;
child.on('error', error => { launchError = error; });
const stopped = new Promise(resolve => child.once('close', resolve));
let token;
async function call(path, body, expected = 200) {
  const response = await fetch(base + path, { method: body === undefined ? 'GET' : 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(15000) });
  assert.equal(response.status, expected, `${path}: ${await response.clone().text()}`);
  return response.json();
}
async function sample() {
  const status = await readFile(`/proc/${child.pid}/status`, 'utf8');
  const raw = await readFile(`/proc/${child.pid}/stat`, 'utf8');
  const fields = raw.slice(raw.lastIndexOf(')') + 2).split(' ');
  const children = (await readFile(`/proc/${child.pid}/task/${child.pid}/children`, 'utf8')).trim();
  return { time: performance.now(), ticks: Number(fields[11]) + Number(fields[12]), rssKiB: Number(status.match(/^VmRSS:\s+(\d+)/m)[1]), peakRssKiB: Number(status.match(/^VmHWM:\s+(\d+)/m)[1]), childProcesses: children ? children.split(/\s+/).length : 0 };
}
try {
  const readyDeadline = Date.now() + 15000;
  let ready = false;
  while (Date.now() < readyDeadline) {
    if (launchError) throw launchError;
    if (child.exitCode !== null) throw new Error(`relay exited: ${logs}`);
    try {
      token = (await readFile(join(dir, 'state/admin.token'), 'utf8')).trim();
      const response = await fetch(base + '/v1/agents', { headers: { Authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(500) });
      if (response.ok) { ready = true; break; }
    } catch { /* Only startup readiness is retried, never a submitted request. */ }
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  assert.ok(ready, `relay did not start: ${logs}`);
  await mkdir(join(dir, 'workspace'));
  await call('/v1/agents', { harness: 'omp', name: 'source', cwd: join(dir, 'workspace') }, 201);
  for (let i = 0; i < agents; i++) await call('/v1/agents', { harness: 'claude', name: `dest${i}`, cwd: join(dir, 'workspace') }, 201);
  const before = await sample();
  const started = performance.now();
  const first = [];
  // One sequential producer per destination, bounded concurrency <=32.
  await Promise.all(Array.from({ length: agents }, async (_, i) => {
    for (let n = 0; n < 128; n++) {
      const task = await call('/v1/request', { from: 'omp:source', to: `claude:dest${i}`, text: 'x'.repeat(512), idempotencyKey: `load-${i}-${n}`, timeoutSeconds: 300 }, 202);
      assert.equal(task.to, `claude:dest${i}`);
      assert.equal(task.attempts, 0);
      if (n === 0) first[i] = task.taskId;
    }
  }));
  const admissionSeconds = (performance.now() - started) / 1000;
  await call('/v1/send', { from: 'omp:source', to: 'claude:dest0', text: 'overload' }, 429);
  const duplicate = await call('/v1/request', { from: 'omp:source', to: 'claude:dest0', text: 'x'.repeat(512), idempotencyKey: 'load-0-0', timeoutSeconds: 300 }, 202);
  assert.equal(duplicate.taskId, first[0]);
  const metrics = await call('/v1/metrics');
  assert.equal(metrics.queued, agents * 128);
  assert.equal(metrics.inFlight, 0);
  const sampleStart = await sample();
  await new Promise(resolve => setTimeout(resolve, 5000));
  const sampleEnd = await sample();
  const hz = Number((await exec('getconf', ['CLK_TCK'])).stdout.trim());
  assert.ok(sampleEnd.rssKiB < 6 * 1024 * 1024, 'relay alone exceeded 6 GiB');
  assert.equal(sampleEnd.childProcesses, 0);
  console.log(JSON.stringify({ load: 'passed', liveRelay: true, providerTurns: 0, queuedMessages: metrics.queued, destinations: agents, payloadBytes: 512, producerConcurrency: agents, admissionSeconds: Number(admissionSeconds.toFixed(3)), admissionsPerSecond: Number((metrics.queued / admissionSeconds).toFixed(2)), baselineRssKiB: before.rssKiB, finalRssKiB: sampleEnd.rssKiB, peakRssKiB: sampleEnd.peakRssKiB, sampledSeconds: Number(((sampleEnd.time - sampleStart.time) / 1000).toFixed(3)), sampledCpuPercentOneCore: Number(((sampleEnd.ticks - sampleStart.ticks) / hz / ((sampleEnd.time - sampleStart.time) / 1000) * 100).toFixed(3)), harnessChildren: sampleEnd.childProcesses, overloadStatus: 429, duplicateAtCapacityDeduplicated: true }));
} finally {
  if (child.exitCode === null) child.kill('SIGTERM');
  let timer;
  try { await Promise.race([stopped, new Promise((_, reject) => { timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('relay shutdown exceeded 10s')); }, 10000); })]); }
  finally { clearTimeout(timer); await rm(dir, { recursive: true, force: true }); }
}
