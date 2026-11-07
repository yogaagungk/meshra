#!/usr/bin/env node
// Containment self-test. Run it inside the confined Claude worker (same mounts,
// namespaces and environment as a restricted profile turn). It prints one JSON
// object; every check must be true. It never contacts the model provider.
//   node confined-probe.mjs [--deny <host path>]... [--expect-broker]
import net from 'node:net';
import { accessSync, constants, existsSync, readdirSync, statSync, writeFileSync } from 'node:fs';

const deny = [];
let expectBroker = false;
const argv = process.argv.slice(2);
for (let i = 0; i < argv.length; i++) {
  if (argv[i] === '--deny') deny.push(argv[++i]);
  else if (argv[i] === '--expect-broker') expectBroker = true;
}
const checks = {};
const reaches = (p) => { try { accessSync(p, constants.F_OK); return true; } catch { return false; } };

checks.homeIsPrivate = process.env.HOME === '/home/worker';
checks.noDaemonEnvironment = !Object.keys(process.env).some((k) => k.startsWith('MESHRA_'));
checks.noAmbientDisplayOrBus = !['DISPLAY', 'WAYLAND_DISPLAY', 'DBUS_SESSION_BUS_ADDRESS', 'SSH_AUTH_SOCK'].some((k) => k in process.env);
checks.hostPathsAbsent = deny.every((p) => !reaches(p));
checks.noHostServiceSockets = !['/run/user', '/var/run/docker.sock', '/run/docker.sock', '/mnt/c', '/etc/passwd', '/sys/fs/cgroup'].some(reaches);
checks.workspaceReadOnly = (() => { try { writeFileSync('/workspace/.meshra-probe', 'x'); return false; } catch { return true; } })();
checks.runDirHoldsOnlyMeshraSockets = (() => {
  try {
    const names = readdirSync('/run/meshra').sort();
    const allowed = expectBroker ? ['broker.sock', 'egress.sock'] : ['egress.sock'];
    return JSON.stringify(names) === JSON.stringify(allowed) && names.every((n) => statSync(`/run/meshra/${n}`).isSocket());
  } catch { return false; }
})();
checks.runtimeMountsReadOnly = (() => { try { writeFileSync('/usr/meshra-probe', 'x'); return false; } catch { return true; } })();

const tcp = (host, port) => new Promise((resolve) => {
  const s = net.createConnection({ host, port, timeout: 2000 });
  s.on('connect', () => { s.destroy(); resolve(true); });
  s.on('error', () => resolve(false));
  s.on('timeout', () => { s.destroy(); resolve(false); });
});
// Through the daemon proxy, an origin without a reviewed grant must be refused.
const viaProxy = (target) => new Promise((resolve) => {
  const s = net.createConnection({ path: '/run/meshra/egress.sock', timeout: 3000 });
  let data = '';
  s.on('connect', () => s.write(`CONNECT ${target} HTTP/1.1\r\nHost: ${target}\r\n\r\n`));
  s.on('data', (d) => { data += d.toString(); if (data.includes('\r\n')) { s.destroy(); resolve(data.split('\r\n')[0]); } });
  s.on('error', () => resolve('error'));
  s.on('timeout', () => { s.destroy(); resolve('timeout'); });
  s.on('close', () => resolve(data.split('\r\n')[0] || 'closed'));
});
checks.directInternetUnreachable = !(await tcp('1.1.1.1', 443)) && !(await tcp('8.8.8.8', 53));
checks.unreviewedOriginRefused = !/^HTTP\/1\.[01] 2\d\d/.test(await viaProxy('example.com:443'));
checks.loopbackBridgeOnly = !(await tcp('127.0.0.1', 22));
const ok = Object.values(checks).every(Boolean);
process.stdout.write(JSON.stringify({ ok, checks }) + '\n');
process.exit(ok ? 0 : 1);
