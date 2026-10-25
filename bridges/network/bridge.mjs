// An opaque private-namespace TCP-to-Unix bridge. The daemon owns all policy;
// this process has no outbound TCP destination and never falls back to one.
import net from 'node:net';

const args = process.argv.slice(2);
let socketPath;
let port = 0;
for (let index = 0; index < args.length; index += 2) {
  const value = args[index + 1];
  if (value === undefined) throw new Error('bridge arguments require values');
  if (args[index] === '--socket') socketPath = value;
  else if (args[index] === '--port' && /^(0|[1-9][0-9]{0,4})$/.test(value)) port = Number(value);
  else throw new Error('unsupported bridge argument');
}
if (!['/run/meshra/egress.sock', '/run/meshra/broker.sock'].includes(socketPath) || port > 65535) {
  throw new Error('a purpose-limited mounted Meshra socket is required');
}

const connections = new Set();
const maximumConnections = 32;
const maximumBytes = 32 * 1024 * 1024;
const maximumLifetimeMs = 5 * 60 * 1000;
let closing = false;
const server = net.createServer({ allowHalfOpen: false }, (client) => {
  if (closing || connections.size >= maximumConnections * 2) {
    client.destroy();
    return;
  }
  const upstream = net.createConnection({ path: socketPath });
  connections.add(client);
  connections.add(upstream);
  let bytes = 0;
  let cleaned = false;
  const cleanup = () => {
    if (cleaned) return;
    cleaned = true;
    clearTimeout(lifetime);
    client.destroy();
    upstream.destroy();
    connections.delete(client);
    connections.delete(upstream);
  };
  const lifetime = setTimeout(cleanup, maximumLifetimeMs);
  lifetime.unref();
  for (const connection of [client, upstream]) {
    connection.on('data', (data) => {
      bytes += data.length;
      if (bytes > maximumBytes) cleanup();
    });
    connection.on('error', cleanup);
    connection.on('close', cleanup);
    connection.setTimeout(30_000, cleanup);
  }
  client.pipe(upstream);
  upstream.pipe(client);
});
server.maxConnections = maximumConnections;
server.on('error', () => {
  process.stderr.write('private socket bridge unavailable\n');
  shutdown(1);
});
server.listen({ host: '127.0.0.1', port, exclusive: true }, () => {
  const address = server.address();
  if (!address || typeof address === 'string' || address.address !== '127.0.0.1') {
    shutdown(1);
    return;
  }
  process.stdout.write(`${JSON.stringify({ address: address.address, port: address.port })}\n`);
});
function shutdown(code = 0) {
  if (closing) return;
  closing = true;
  for (const connection of connections) connection.destroy();
  connections.clear();
  server.close(() => process.exit(code));
  setTimeout(() => process.exit(code), 1000).unref();
}
process.on('SIGTERM', () => shutdown());
process.on('SIGINT', () => shutdown());
