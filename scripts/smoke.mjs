#!/usr/bin/env node
// Credential-free smoke against the real executable. Never starts a model turn.
import { spawn, execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdtemp, mkdir, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createServer } from 'node:net';
import assert from 'node:assert/strict';

const exec = promisify(execFile);
const binary = resolve(process.env.MESHRA_BIN || join(dirname(fileURLToPath(import.meta.url)), '../meshra'));
const directory = await mkdtemp(join(tmpdir(), 'meshra-smoke-'));
const reservation = createServer();
await new Promise((resolve, reject) => { reservation.once('error', reject); reservation.listen(0, '127.0.0.1', resolve); });
const port = reservation.address().port;
await new Promise(resolve => reservation.close(resolve));
const address = `127.0.0.1:${port}`;
const flags = ['--data-dir', join(directory, 'state'), '--addr', address, '--json'];
const environment = { ...process.env, MESHRA_TOKEN: '' };
let daemon;
let stopped;
async function start() {
  let diagnostics = '';
  daemon = spawn(binary, [...flags, 'serve'], { env: environment, stdio: ['ignore', 'ignore', 'pipe'] });
  daemon.stderr.on('data', chunk => { diagnostics = (diagnostics + chunk).slice(-8192); });
  let launchError;
  daemon.once('error', error => { launchError = error; });
  stopped = new Promise(resolve => daemon.once('close', code => resolve(code)));
  const deadline = Date.now() + 10000;
  while (Date.now() < deadline) {
    if (launchError) throw launchError;
    if (daemon.exitCode !== null) throw new Error(`Daemon exited: ${diagnostics}`);
    try {
      const token = (await readFile(join(directory, 'state/admin.token'), 'utf8')).trim();
      const response = await fetch(`http://${address}/v1/agents`, { headers: { Authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(1000) });
      if (response.ok) return;
    } catch { /* Startup is bounded by the deadline, not a fabricated fallback. */ }
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  throw new Error(`Daemon readiness timed out: ${diagnostics}`);
}
async function stop() {
  if (!daemon) return;
  const child = daemon;
  child.kill('SIGTERM');
  let timer;
  try {
    const code = await Promise.race([stopped, new Promise((_, reject) => {
      timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('Daemon shutdown timed out')); }, 10000);
    })]);
    assert.equal(code, 0, 'graceful daemon exit');
  } finally {
    clearTimeout(timer);
    daemon = undefined;
  }
}
async function cli(...args) {
  const { stdout } = await exec(binary, [...flags, ...args], { env: environment, timeout: 15000 });
  return JSON.parse(stdout);
}
try {
  await start();
  for (const [harness, name] of [['claude', 'exampleone'], ['claude', 'exampletwo'], ['omp', 'exampleworker'], ['codex', 'reviewer'], ['opencode', 'testengine']]) {
    const cwd = join(directory, `${harness}-${name}`);
    await mkdir(cwd);
    const agent = await cli('agent', 'create', '--harness', harness, '--name', name, '--cwd', cwd);
    assert.equal(agent.id, `${harness}:${name}`);
  }
  const request = ['request', '--from', 'omp:exampleworker', '--to', 'claude:exampleone', '--message', 'durable offline smoke', '--idempotency-key', 'smoke-001'];
  const task = await cli(...request);
  assert.equal(task.state, 'queued');
  assert.equal((await cli(...request)).taskId, task.taskId, 'deduplicated admission');
  const denied = await fetch(`http://${address}/v1/agents`, { headers: { Authorization: 'Bearer invalid' } });
  assert.equal(denied.status, 401);
  await stop();
  await start();
  const recovered = await cli('task', 'status', task.taskId);
  assert.equal(recovered.taskId, task.taskId);
  assert.equal(recovered.state, 'queued');
  assert.equal(recovered.attempts, 0);
  assert.equal(recovered.to, 'claude:exampleone');
  const agents = await cli('agent', 'list');
  assert.deepEqual(agents.filter(agent => agent.harness === 'claude').map(agent => agent.id), ['claude:exampleone', 'claude:exampletwo']);
  assert.equal((await cli('agent', 'status', 'claude:exampletwo')).state, 'registered');
  assert.equal((await cli('task', 'cancel', task.taskId)).state, 'cancelled');
  console.log(JSON.stringify({ smoke: 'passed', registeredAgents: agents.length, independentClaudeIdentities: 2, queuedRestartRecovery: true, duplicateAdmissionDeduplicated: true, invalidCredentialStatus: denied.status, providerTurns: 0 }));
} finally {
  try { await stop(); } finally { await rm(directory, { recursive: true, force: true }); }
}
