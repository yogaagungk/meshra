#!/usr/bin/env node
// The worker has no network route. Forward its private loopback proxy only to
// the daemon's purpose-scoped Unix socket; never expose a debugging/listen port.
import net from 'node:net';
import { spawn } from 'node:child_process';
import { dirname } from 'node:path';

const [executable, ...args] = process.argv.slice(2);
if (!executable?.startsWith('/') || executable.includes('\0')) process.exit(2);
const sockets = new Set();
let child;
let stopped = false;
const server = net.createServer((local) => {
  if (sockets.size >= 32) { local.destroy(); return; }
  const remote = net.connect('/run/meshra/egress.sock');
  sockets.add(local); sockets.add(remote);
  const close = () => { local.destroy(); remote.destroy(); sockets.delete(local); sockets.delete(remote); };
  local.on('error', close); remote.on('error', close);
  local.on('close', close); remote.on('close', close);
  local.pipe(remote); remote.pipe(local);
});
function stop(code) {
  if (stopped) return;
  stopped = true;
  server.close();
  for (const socket of sockets) socket.destroy();
  child?.kill('SIGKILL');
  process.stdin.unpipe(); process.stdin.destroy();
  process.exitCode = code;
}
server.on('error', () => stop(1));
server.listen(0, '127.0.0.1', () => {
  const address = server.address();
  const proxy = `http://127.0.0.1:${address.port}`;
  const env = { ...process.env, PATH: `${dirname(process.execPath)}:/usr/bin:/bin`,
    HTTP_PROXY: proxy, HTTPS_PROXY: proxy, ALL_PROXY: proxy,
    http_proxy: proxy, https_proxy: proxy, all_proxy: proxy, NO_PROXY: '', no_proxy: '', NODE_USE_ENV_PROXY: '1' };
  child = spawn(executable, args, { cwd: '/workspace', env, stdio: ['pipe', 'pipe', 'ignore'] });
  process.stdin.pipe(child.stdin); child.stdout.pipe(process.stdout);
  child.stdin.on('error', () => stop(1));
  child.on('error', () => stop(1));
  child.on('close', (code) => stop(code === 0 ? 0 : 1));
});
process.on('SIGTERM', () => stop(1));
process.on('SIGINT', () => stop(1));
process.stdin.on('error', () => stop(1));
