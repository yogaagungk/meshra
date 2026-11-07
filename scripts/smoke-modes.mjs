#!/usr/bin/env node
// Real daemon smoke for P1 execution modes. Zero provider turns, temporary state.
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createServer } from 'node:net';
import assert from 'node:assert/strict';

const binary = resolve(process.env.MESHRA_BIN || join(dirname(fileURLToPath(import.meta.url)), '../meshra'));
const directory = await mkdtemp(join(tmpdir(), 'meshra-modes-'));
const reservation = createServer();
await new Promise(r => reservation.listen(0, '127.0.0.1', r));
const port = reservation.address().port;
await new Promise(r => reservation.close(r));
const address = `127.0.0.1:${port}`;
const daemon = spawn(binary, ['--data-dir', join(directory, 'state'), '--addr', address, 'serve'], { env: { ...process.env, MESHRA_TOKEN: '' }, stdio: 'ignore' });
let cookie = '', csrf = '';
async function call(path, body) {
  const headers = { 'Content-Type': 'application/json', 'X-Meshra-Console': '1', Origin: `http://${address}`, Cookie: cookie, 'X-Meshra-CSRF': csrf };
  const response = await fetch(`http://${address}/console/api/${path}`, { method: 'POST', headers, body: JSON.stringify(body) });
  const text = await response.text();
  let data; try { data = JSON.parse(text); } catch { data = text; }
  if (path === 'login') { cookie = response.headers.get('set-cookie').split(';')[0]; csrf = data.csrf; }
  return { status: response.status, data };
}
const automation = (op, body) => call(`automation/${op}`, body);
try {
  const deadline = Date.now() + 10000;
  let token;
  while (Date.now() < deadline) {
    try { token = (await readFile(join(directory, 'state/admin.token'), 'utf8')).trim(); await fetch(`http://${address}/v1/agents`, { headers: { Authorization: `Bearer ${token}` } }); break; } catch { await new Promise(r => setTimeout(r, 50)); }
  }
  assert.equal((await call('login', { token })).status, 200);
  const ws = await call('workspaces/add', { path: directory, name: 'modes' });
  assert.equal(ws.status, 200, JSON.stringify(ws));
  const base = { name: 'smoke', instructions: 'x', workspaceId: ws.data.id, capabilities: { workspaceRead: false, workspaceWrite: false, command: false, browser: false, computer: false, memoryWrite: false, externalMcp: false, attachments: false }, allowedProfileIds: [], notificationsEnabled: false };
  for (const harness of ['omp', 'claude']) {
    const r = await automation('profiles.save', { ...base, binding: { harness, config: {} } });
    assert.equal(r.status, 503, JSON.stringify(r));
    assert.match(r.data.error, /restricted execution is unavailable/);
  }
  const noAck = await automation('profiles.save', { ...base, binding: { harness: 'omp', config: {} }, executionMode: 'trusted' });
  assert.equal(noAck.status, 400);
  const trusted = await automation('profiles.save', { ...base, binding: { harness: 'omp', config: {} }, executionMode: 'trusted', acknowledgeTrusted: true });
  assert.equal(trusted.status, 201, JSON.stringify(trusted));
  assert.equal(trusted.data.executionMode, 'trusted');
  assert.equal(trusted.data.modeReviewed, true);
  const back = await automation('profiles.setMode', { id: trusted.data.id, expectedRevision: trusted.data.revision, mode: 'restricted' });
  assert.equal(back.status, 503);
  const after = await automation('profiles.get', { id: trusted.data.id });
  assert.equal(after.data.executionMode, 'trusted');
  assert.equal(after.data.revision, trusted.data.revision);
  const sneaky = await automation('profiles.save', { ...base, id: trusted.data.id, expectedRevision: trusted.data.revision, binding: { harness: 'omp', config: {} }, executionMode: 'restricted' });
  assert.equal(sneaky.status, 409);
  console.log('execution-mode smoke passed');
} finally {
  daemon.kill('SIGTERM');
  await new Promise(r => daemon.once('close', r));
  await rm(directory, { recursive: true, force: true });
}
