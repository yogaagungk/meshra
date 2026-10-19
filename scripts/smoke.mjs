#!/usr/bin/env node
// Real CLI/daemon smoke. Provider turns require explicit MESHRA_NATIVE_SMOKE=1.
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
const native = process.env.MESHRA_NATIVE_SMOKE === '1';
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
  const { stdout } = await exec(binary, [...flags, ...args], { env: environment, timeout: native ? 120000 : 15000 });
  return JSON.parse(stdout);
}
try {
  await start();
  for (const [harness, name] of [['claude', 'exampleone'], ['claude', 'exampletwo'], ['omp', 'exampleworker'], ['codex', 'reviewer'], ['opencode', 'testengine']]) {
    const cwd = join(directory, `${harness}-${name}`);
    await mkdir(cwd);
    const policy = harness === 'omp' ? ['--allow-to', 'codex:reviewer', '--allow-operation', 'request,status,events'] : [];
    const agent = await cli('agent', 'create', '--harness', harness, '--name', name, '--cwd', cwd, ...policy);
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
  const trace = await cli('task', 'trace', task.taskId);
  assert.equal(trace.task.state, 'cancelled');
  assert.equal(trace.events.at(-1).type, 'cancelled');
  assert.ok(trace.events.every(event => event.correlationId === task.correlationId && event.to === 'claude:exampleone'));
  const waiting = await cli('request', '--from', 'omp:exampleworker', '--to', 'codex:reviewer', '--message', 'bounded wait smoke', '--timeout', '1s', '--wait');
  let expired = waiting;
  const deadline = Date.now() + 2000;
  while (expired.state !== 'timed_out' && Date.now() < deadline) {
    await new Promise(resolve => setTimeout(resolve, 20));
    expired = await cli('task', 'status', waiting.taskId);
  }
  assert.equal(expired.state, 'timed_out', JSON.stringify({ observedAt: new Date().toISOString(), task: expired }));
  assert.equal(expired.attempts, 0);
  assert.equal(expired.ambiguous || false, false);
  const metrics = await cli('metrics');
  assert.equal(metrics.expired, 1);
  assert.equal(metrics.offlineAgents, 5);
  const diagnostics = await cli('doctor');
  assert.ok(diagnostics.checks.some(check => check.name === 'sqlite' && check.status === 'ok'));
  const sender = await cli('agent', 'token', 'omp:exampleworker');
  const forbidden = await fetch(`http://${address}/v1/send`, { method: 'POST', headers: { Authorization: `Bearer ${sender.token}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ to: 'claude:exampletwo', from: 'omp:notme', text: 'unauthorized smoke marker' }) });
  assert.equal(forbidden.status, 403);
  const proxy = spawn(binary, [...flags, 'mcp', 'stdio'], { env: { ...environment, MESHRA_TOKEN: sender.token }, stdio: ['pipe', 'pipe', 'pipe'] });
  let protocol = '';
  let proxyDiagnostics = '';
  proxy.stdout.on('data', chunk => { protocol += chunk; });
  proxy.stderr.on('data', chunk => { proxyDiagnostics += chunk; });
  const proxyDone = new Promise((resolve, reject) => { proxy.once('error', reject); proxy.once('close', resolve); });
  proxy.stdin.end(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-11-25', capabilities: {}, clientInfo: { name: 'meshra-smoke', version: '0.2.0' } } }) + '\n' +
    JSON.stringify({ jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name: 'meshra_task_trace', arguments: { taskId: task.taskId } } }) + '\n');
  let proxyTimer;
  let proxyCode;
  try {
    proxyCode = await Promise.race([proxyDone, new Promise((_, reject) => { proxyTimer = setTimeout(() => { proxy.kill('SIGKILL'); reject(new Error('MCP proxy timeout')); }, 10000); })]);
  } finally { clearTimeout(proxyTimer); }
  assert.equal(proxyCode, 0, proxyDiagnostics);
  const replies = protocol.trim().split('\n').map(line => JSON.parse(line));
  assert.equal(replies.find(reply => reply.id === 1).result.serverInfo.version, '0.2.0');
  const traced = replies.find(reply => reply.id === 2).result.structuredContent;
  assert.equal(traced.task.state, 'cancelled');
  assert.equal(traced.task.correlationId, task.correlationId);
  let providerTurns = 0;
  if (native) {
    const omp = await cli('agent', 'start', 'omp:exampleworker');
    const codex = await cli('agent', 'start', 'codex:reviewer');
    const scoped = await cli('agent', 'token', omp.id);
    const headers = { Authorization: `Bearer ${scoped.token}`, 'Content-Type': 'application/json' };
    const response = await fetch(`http://${address}/v1/request?wait=true`, { method: 'POST', headers, body: JSON.stringify({ to: codex.id, text: 'Reply with exactly MESHRA_LIVE_CORRELATED_OK. Do not use tools.', timeoutSeconds: 90, idempotencyKey: 'native-correlated' }), signal: AbortSignal.timeout(95000) });
    assert.equal(response.status, 200);
    const reply = await response.json();
    assert.equal(reply.state, 'completed', JSON.stringify(reply));
    assert.ok(reply.result.includes('MESHRA_LIVE_CORRELATED_OK'));
    assert.equal(reply.originAgentId, omp.id);
    assert.equal(reply.originSessionId, omp.sessionId);
    assert.equal(reply.destinationSessionId, codex.sessionId);
    const owned = await fetch(`http://${address}/v1/tasks/${reply.taskId}`, { headers });
    assert.equal(owned.status, 200);
    assert.equal((await owned.json()).correlationId, reply.correlationId);
    providerTurns++;
    const reverse = await cli('request', '--from', codex.id, '--to', omp.id, '--message', 'Reply with exactly MESHRA_LIVE_REVERSE_OK. Do not use tools.', '--timeout', '90s', '--wait');
    assert.equal(reverse.state, 'completed', JSON.stringify(reverse));
    assert.ok(reverse.result.includes('MESHRA_LIVE_REVERSE_OK'));
    assert.equal(reverse.originSessionId, codex.sessionId);
    assert.equal(reverse.destinationSessionId, omp.sessionId);
    providerTurns++;
    await cli('agent', 'stop', codex.id);
    const queued = await cli('request', '--from', omp.id, '--to', codex.id, '--message', 'Reply with exactly MESHRA_LIVE_RESTART_OK. Do not use tools.', '--timeout', '90s');
    assert.equal(queued.attempts, 0);
    await stop();
    await start();
    const recovered = await cli('task', 'status', queued.taskId);
    assert.equal(recovered.state, 'queued');
    assert.equal(recovered.attempts, 0);
    const resumed = await cli('agent', 'start', codex.id);
    assert.equal(resumed.sessionId, codex.sessionId);
    const until = Date.now() + 90000;
    let completed = recovered;
    while (!['completed', 'failed', 'cancelled', 'timed_out'].includes(completed.state) && Date.now() < until) {
      await new Promise(resolve => setTimeout(resolve, 100));
      completed = await cli('task', 'status', queued.taskId);
    }
    assert.equal(completed.state, 'completed', JSON.stringify(completed));
    assert.ok(completed.result.includes('MESHRA_LIVE_RESTART_OK'));
    assert.equal(completed.attempts, 1);
    assert.equal(completed.originSessionId, omp.sessionId);
    assert.equal(completed.destinationSessionId, codex.sessionId);
    const stoppedOrigin = await fetch(`http://${address}/v1/tasks/${queued.taskId}`, { headers });
    assert.equal(stoppedOrigin.status, 200);
    assert.equal((await stoppedOrigin.json()).correlationId, queued.correlationId);
    assert.equal((await cli('agent', 'status', omp.id)).state, 'stopped');
    providerTurns++;
  }
  console.log(JSON.stringify({ smoke: 'passed', registeredAgents: agents.length, independentClaudeIdentities: 2, queuedRestartRecovery: true, duplicateAdmissionDeduplicated: true, traceCorrelation: true, boundedSynchronousWait: true, metrics: true, doctor: true, mcpStdioTrace: true, unauthorizedStatus: forbidden.status, invalidCredentialStatus: denied.status, providerTurns, liveCrossHarnessReply: native, liveNativeResumeAfterRestart: native }));
} finally {
  try { await stop(); } finally { await rm(directory, { recursive: true, force: true }); }
}
