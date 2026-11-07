#!/usr/bin/env node
// Confined launcher for native harness binaries (restricted OMP). It runs inside
// the bubblewrap worker: no network namespace access, private HOME. It exposes
// the daemon-owned egress socket as a loopback HTTP proxy and runs ONE reviewed
// executable with inherited stdio. It holds no policy: the daemon proxy decides
// every origin, and the argv is built by the daemon, never by the model.
import net from 'node:net';
import { spawn } from 'node:child_process';
import { statSync } from 'node:fs';

const EGRESS = '/run/meshra/egress.sock';
const argv = process.argv.slice(2);
const sep = argv.indexOf('--');
if (argv[0] !== '--exec' || sep !== 2 || !argv[1].startsWith('/')) {
  throw new Error('confined-exec requires --exec <absolute executable> -- <args...>');
}
const exe = argv[1];
const args = argv.slice(sep + 1);
if (!statSync(EGRESS).isSocket()) throw new Error('mounted daemon egress socket is unavailable');

const connections = new Set();
const server = net.createServer({ allowHalfOpen: false }, (client) => {
  if (connections.size >= 64) { client.destroy(); return; }
  const upstream = net.createConnection({ path: EGRESS });
  connections.add(client); connections.add(upstream);
  let bytes = 0;
  const cleanup = () => { client.destroy(); upstream.destroy(); connections.delete(client); connections.delete(upstream); };
  for (const c of [client, upstream]) {
    c.on('data', (d) => { bytes += d.length; if (bytes > 256 * 1024 * 1024) cleanup(); });
    c.on('error', cleanup); c.on('close', cleanup);
  }
  client.pipe(upstream); upstream.pipe(client);
});
server.on('error', () => process.exit(1));
server.listen({ host: '127.0.0.1', port: 0, exclusive: true }, () => {
  const { port } = server.address();
  const proxy = `http://127.0.0.1:${port}`;
  const child = spawn(exe, args, {
    stdio: 'inherit',
    env: { ...process.env, HTTPS_PROXY: proxy, HTTP_PROXY: proxy, https_proxy: proxy, http_proxy: proxy, NO_PROXY: '', no_proxy: '' },
  });
  const stop = (sig) => () => { child.kill(sig); };
  process.on('SIGTERM', stop('SIGTERM')); process.on('SIGINT', stop('SIGINT'));
  child.on('exit', (code, signal) => { server.close(); process.exit(code ?? (signal ? 1 : 0)); });
});
