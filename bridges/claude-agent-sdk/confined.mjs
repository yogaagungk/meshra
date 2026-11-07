#!/usr/bin/env node
// Confined launcher for restricted Claude profiles. It runs inside the
// bubblewrap worker (no network namespace access, private HOME) and does two
// things only: expose the daemon-owned egress socket as a loopback HTTP proxy
// for the native provider connection, then run the reviewed SDK bridge with
// inherited stdio. It holds no policy: the daemon proxy decides every origin.
import net from 'node:net';
import { spawn } from 'node:child_process';
import { statSync } from 'node:fs';

const EGRESS = '/run/meshra/egress.sock';
const args = process.argv.slice(2);
if (args.length !== 2 || args[0] !== '--bridge' || !args[1].startsWith('/') || !args[1].endsWith('/bridge.mjs')) {
  throw new Error('confined launcher requires --bridge <absolute bridge.mjs>');
}
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
  const child = spawn(process.execPath, [args[1]], {
    stdio: 'inherit',
    env: { ...process.env, HTTPS_PROXY: proxy, HTTP_PROXY: proxy, https_proxy: proxy, http_proxy: proxy, NO_PROXY: '', no_proxy: '' },
  });
  const stop = (sig) => () => { child.kill(sig); };
  process.on('SIGTERM', stop('SIGTERM')); process.on('SIGINT', stop('SIGINT'));
  child.on('exit', (code, signal) => { server.close(); process.exit(code ?? (signal ? 1 : 0)); });
});
