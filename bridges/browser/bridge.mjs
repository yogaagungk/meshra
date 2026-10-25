#!/usr/bin/env node
// JSONL worker protocol. This entrypoint is mounted and run only by the confined
// worker: no host display, shell invocation, raw debugging port or direct egress.
import { randomBytes, randomUUID } from 'node:crypto';
import { spawn } from 'node:child_process';
import { constants } from 'node:fs';
import * as fs from 'node:fs/promises';
import { createServer, connect } from 'node:net';
import { basename, join, isAbsolute } from 'node:path';
import { once } from 'node:events';

const STATE = '/home/worker/browser-data';
const EGRESS = '/run/meshra/egress.sock';
const WIDTH = 1280;
const HEIGHT = 800;
const MODEL_IMAGE = 512 * 1024;
const VIEWER_IMAGE = 2 * 1024 * 1024;
const MAX_INPUT = 16 * 1024;
const MAX_OUTPUT = 3 * 1024 * 1024;
const MAX_TEXT = 64 * 1024;
const MAX_FILE = 20 * 1024 * 1024;
const MAX_QUARANTINE = 64 * 1024 * 1024;
const TIMEOUT = 45_000;
const DISPLAY = ':99';

function object(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}
function requireThat(condition, message) {
  if (!condition) throw new Error(message);
}
function bounded(value, bytes, name, empty = false) {
  requireThat(typeof value === 'string' && (empty || value.length > 0) && !value.includes('\0') && Buffer.byteLength(value) <= bytes, `invalid ${name}`);
  return value;
}
function integer(value, min, max, name) {
  requireThat(Number.isSafeInteger(value) && value >= min && value <= max, `invalid ${name}`);
  return value;
}
function truncate(value, bytes) {
  const buffer = Buffer.from(value);
  if (buffer.length <= bytes) return value;
  // Avoid splitting a UTF-8 code point, including a four-byte code point.
  let end = bytes;
  while (end > 0 && (buffer[end] & 0xc0) === 0x80) end -= 1;
  return buffer.subarray(0, end).toString('utf8');
}
function parseOptions(argv) {
  const names = new Set(['mode', 'state-dir', 'egress-socket', 'browsers-path', 'executable', 'xvfb', 'xauth', 'openbox', 'xdotool', 'ffmpeg', 'allowlist']);
  const result = { mode: 'browser' };
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index]?.slice(2);
    requireThat(argv[index]?.startsWith('--') && names.has(name) && typeof argv[index + 1] === 'string' && !(name in result && name !== 'mode'), 'unknown, repeated or incomplete bridge option');
    result[name] = bounded(argv[index + 1], 16 * 1024, 'bridge option');
  }
  requireThat(['browser', 'desktop'].includes(result.mode), 'unknown bridge mode');
  requireThat(result['state-dir'] === STATE && result['egress-socket'] === EGRESS, 'private state directory and mounted daemon egress socket are required');
  if (result.allowlist) {
    try { result.allowlist = JSON.parse(result.allowlist); } catch { throw new Error('invalid desktop allowlist JSON'); }
    requireThat(object(result.allowlist) && Object.keys(result.allowlist).every((key) => ['chromium', 'xterm'].includes(key)), 'only reviewed chromium and xterm allowlist entries are supported');
  } else result.allowlist = {};
  return result;
}

let options;
let context;
let cdp;
let proxy;
let proxyPort;
let desktop;
let stopping = false;
let downloadExpected = false;
let activeDownload;
const children = new Set();
const applications = new Set();
const sockets = new Set();
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// Construct the environment from scratch: do not inherit host DISPLAY, Wayland,
// D-Bus, proxies, shell startup hooks, loader flags or Chromium overrides.
function childEnv(withDisplay = false) {
  const env = {
    PATH: '/usr/bin:/bin', HOME: join(STATE, 'desktop-home'),
    TMPDIR: '/tmp', LANG: 'C.UTF-8', LC_ALL: 'C.UTF-8',
    XDG_CONFIG_HOME: join(STATE, 'desktop-home', '.config'),
    XDG_CACHE_HOME: join(STATE, 'desktop-home', '.cache'),
    XDG_DATA_HOME: join(STATE, 'desktop-home', '.local', 'share'),
    XDG_RUNTIME_DIR: join(STATE, 'runtime'),
  };
  if (withDisplay) {
    env.DISPLAY = DISPLAY;
    env.XAUTHORITY = desktop.authority;
  }
  return env;
}
async function privateDirectory(path) {
  await fs.mkdir(path, { recursive: true, mode: 0o700 });
  const stat = await fs.lstat(path);
  requireThat(stat.isDirectory() && !stat.isSymbolicLink() && await fs.realpath(path) === path, 'private directory must not contain symlinks');
  await fs.chmod(path, 0o700);
}
async function executable(path, name) {
  bounded(path, 4096, name);
  requireThat(isAbsolute(path), `${name} must be an explicitly pinned absolute path`);
  await fs.access(path, constants.X_OK);
  requireThat((await fs.stat(path)).isFile(), `${name} is not an executable file`);
  // The Go launcher verifies the complete mounted artifact manifest, including
  // symlink targets and runtime resources, before starting this worker.
}
function startChild(command, argv, { display = false, capture = false } = {}) {
  const child = spawn(command, argv, {
    cwd: '/home/worker', env: childEnv(display), shell: false, detached: true,
    stdio: capture ? ['ignore', 'pipe', 'pipe'] : 'ignore',
  });
  children.add(child);
  child.failure = null;
  child.on('error', (error) => { child.failure = error; });
  child.once('close', () => { children.delete(child); applications.delete(child); });
  return child;
}
function killGroup(child, signal = 'SIGTERM') {
  if (!child.pid) return;
  try { process.kill(-child.pid, signal); } catch { /* already exited */ }
}
async function run(command, argv, { display = false, cap = 8192, timeout = 10_000 } = {}) {
  const child = startChild(command, argv, { display, capture: true });
  const chunks = [];
  let size = 0;
  let failure;
  const timer = setTimeout(() => { failure = new Error('private desktop command timed out'); killGroup(child, 'SIGKILL'); }, timeout);
  child.stdout.on('data', (chunk) => {
    size += chunk.length;
    if (size > cap) {
      failure = new Error('private desktop output exceeds its byte limit');
      killGroup(child, 'SIGKILL');
    } else chunks.push(chunk);
  });
  // Drain stderr without retaining untrusted or unbounded diagnostic output.
  child.stderr.on('data', () => {});
  try {
    const [code] = await once(child, 'close');
    if (failure) throw failure;
    requireThat(code === 0, 'private desktop command failed');
    return Buffer.concat(chunks, size);
  } finally { clearTimeout(timer); }
}
async function started(child) {
  if (!child.pid) await once(child, 'spawn');
  await delay(150);
  requireThat(!child.failure && child.exitCode === null && child.signalCode === null, 'private desktop process failed to start');
}

// Forward the HTTP proxy byte stream without parsing/re-encoding HTTP. The only
// upstream address is the mounted daemon socket; CONNECT responses and buffered
// TLS bytes remain intact. All destination policy is enforced by that daemon.
async function startProxy() {
  const stat = await fs.lstat(EGRESS);
  requireThat(stat.isSocket(), 'mounted daemon egress socket is unavailable');
  proxy = createServer({ allowHalfOpen: false }, (client) => {
    if (stopping || sockets.size >= 128) { client.destroy(); return; }
    const upstream = connect({ path: EGRESS, allowHalfOpen: false });
    for (const socket of [client, upstream]) {
      sockets.add(socket);
      socket.setTimeout(60_000, () => socket.destroy());
      socket.on('error', () => { client.destroy(); upstream.destroy(); });
      socket.on('close', () => { sockets.delete(socket); client.destroy(); upstream.destroy(); });
    }
    client.pipe(upstream);
    upstream.pipe(client);
  });
  await new Promise((resolve, reject) => {
    proxy.once('error', reject);
    proxy.listen({ host: '127.0.0.1', port: 0, backlog: 32 }, resolve);
  });
  proxy.on('error', () => { void shutdown(1); });
  proxyPort = proxy.address().port;
}
function chromiumArgs() {
  return [
    `--proxy-server=http://127.0.0.1:${proxyPort}`, '--proxy-bypass-list=<-loopback>',
    '--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1',
    '--force-webrtc-ip-handling-policy=disable_non_proxied_udp',
    '--no-first-run', '--no-default-browser-check', '--disable-dev-shm-usage',
    '--disable-background-networking', '--disable-component-update', '--disable-sync',
    '--metrics-recording-only', '--no-service-autorun', '--password-store=basic',
    '--use-mock-keychain', '--window-size=1280,800',
  ];
}
async function launchBrowser() {
  if (options['browsers-path']) process.env.PLAYWRIGHT_BROWSERS_PATH = options['browsers-path'];
  const { chromium } = await import('playwright');
  const path = options.executable || chromium.executablePath();
  await executable(path, 'Chromium');
  await privateDirectory(join(STATE, 'profile'));
  context = await chromium.launchPersistentContext(join(STATE, 'profile'), {
    executablePath: path, headless: true, chromiumSandbox: true,
    env: childEnv(), args: chromiumArgs(), viewport: { width: WIDTH, height: HEIGHT },
    acceptDownloads: true, downloadsPath: join(STATE, 'download-staging'),
    serviceWorkers: 'block', timeout: TIMEOUT,
  });
  context.setDefaultTimeout(TIMEOUT);
  context.setDefaultNavigationTimeout(TIMEOUT);
  context.on('page', (page) => {
    if (context.pages().length > 4) void page.close().catch(() => {});
    page.on('download', (download) => {
      if (!downloadExpected) void download.cancel().catch(() => {});
    });
  });
  cdp = await context.browser().newBrowserCDPSession();
  await downloadBehavior(false);
  cdp.on('Browser.downloadProgress', (event) => {
    if (event.state === 'inProgress' && event.receivedBytes > MAX_FILE) {
      void cdp.send('Browser.cancelDownload', { guid: event.guid }).catch(() => {});
      if (activeDownload) activeDownload.exceeded = true;
    }
  });
  for (const page of context.pages()) await page.close();
}
async function downloadBehavior(allow) {
  await cdp.send('Browser.setDownloadBehavior', {
    behavior: allow ? 'allowAndName' : 'deny', eventsEnabled: true,
    ...(allow ? { downloadPath: join(STATE, 'download-staging') } : {}),
  });
}
function currentPage() {
  const page = context.pages().at(-1);
  requireThat(page && !page.isClosed(), 'no tab is open');
  return page;
}
async function browserState(page, includeText = false) {
  const result = { url: truncate(page.url(), 2048), tabs: context.pages().length };
  if (includeText) {
    // Bound extraction inside the renderer as well as the protocol output.
    const snapshot = await page.evaluate((limit) => {
      const walker = document.createTreeWalker(document.body || document.documentElement, NodeFilter.SHOW_TEXT);
      let text = '';
      let nodes = 0;
      let truncated = false;
      while (walker.nextNode()) {
        if (++nodes > 20_000 || text.length >= limit) { truncated = true; break; }
        const node = walker.currentNode;
        if (['SCRIPT', 'STYLE', 'NOSCRIPT'].includes(node.parentElement?.tagName)) continue;
        const remaining = limit - text.length;
        const value = node.nodeValue || '';
        text += value.slice(0, remaining) + '\n';
        if (value.length > remaining) { truncated = true; break; }
      }
      return { text, title: document.title.slice(0, 2048), truncated };
    }, MAX_TEXT);
    result.text = truncate(snapshot.text, MAX_TEXT);
    result.title = truncate(snapshot.title, 2048);
    result.truncated = snapshot.truncated || Buffer.byteLength(snapshot.text) > MAX_TEXT;
  }
  return result;
}
function imageResult(buffer, cap) {
  requireThat(buffer.length <= cap, `PNG screenshot exceeds ${cap} bytes`);
  requireThat(buffer.length >= 24 && buffer.subarray(0, 8).equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10])), 'screenshot did not produce real PNG pixels');
  return { frame: buffer.toString('base64'), frameBytes: buffer.length, mimeType: 'image/png', truncated: false };
}
async function upload(page, args) {
  const root = join(STATE, 'uploads');
  requireThat(args.path.startsWith(root + '/') && await fs.realpath(root) === root, 'only staged uploads are permitted');
  // Resolve the opened descriptor, not a pre-open path: reject symlinks and
  // parent-directory swaps without passing a mutable filename to Chromium.
  const handle = await fs.open(args.path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  try {
    const actual = await fs.realpath(`/proc/self/fd/${handle.fd}`);
    requireThat(actual.startsWith(root + '/'), 'upload escaped the staged directory');
    const stat = await handle.stat();
    requireThat(stat.isFile() && stat.size <= MAX_FILE, 'upload must be a regular file of at most 20 MiB');
    const buffer = Buffer.alloc(stat.size);
    let offset = 0;
    while (offset < buffer.length) {
      const { bytesRead } = await handle.read(buffer, offset, buffer.length - offset, offset);
      requireThat(bytesRead > 0, 'staged upload changed while reading');
      offset += bytesRead;
    }
    const extra = Buffer.alloc(1);
    requireThat((await handle.read(extra, 0, 1, offset)).bytesRead === 0, 'staged upload changed while reading');
    await page.setInputFiles(args.selector, { name: basename(actual), mimeType: 'application/octet-stream', buffer });
  } finally { await handle.close(); }
}
async function quarantineSize() {
  let total = 0;
  for (const name of await fs.readdir(join(STATE, 'downloads'))) {
    const stat = await fs.lstat(join(STATE, 'downloads', name));
    requireThat(stat.isFile() && !stat.isSymbolicLink(), 'invalid quarantined download');
    total += stat.size;
  }
  return total;
}
async function download(page, args) {
  const remaining = MAX_QUARANTINE - await quarantineSize();
  requireThat(remaining > 0, 'download quarantine is full');
  const budget = Math.min(MAX_FILE, remaining);
  const target = join(STATE, 'downloads', `${randomUUID()}.download`);
  let item;
  let output;
  let timer;
  const state = { exceeded: false };
  activeDownload = state;
  downloadExpected = true;
  try {
    await downloadBehavior(true);
    // Download progress events cancel oversized transfers before persistence.
    // Also bound aggregate on-disk staging, including unfinished transfers.
    timer = setInterval(() => {
      if (state.polling) return;
      state.polling = true;
      void (async () => {
        let size = 0;
        for (const name of await fs.readdir(join(STATE, 'download-staging'))) size += (await fs.lstat(join(STATE, 'download-staging', name))).size;
        if (size > budget) {
          state.exceeded = true;
          if (item) await item.cancel();
          await downloadBehavior(false);
        }
      })().catch(() => { state.exceeded = true; if (item) void item.cancel().catch(() => {}); }).finally(() => { state.polling = false; });
    }, 100);
    const pending = page.waitForEvent('download', { timeout: TIMEOUT });
    // Attach rejection handling before click can fail, preventing an orphaned
    // waiter from becoming an unhandled rejection.
    pending.catch(() => {});
    await page.click(args.selector);
    item = await pending;
    requireThat(!state.exceeded, 'download exceeds quarantine byte limit');
    const input = await item.createReadStream();
    requireThat(input, 'download failed');
    output = await fs.open(target, constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY | constants.O_NOFOLLOW, 0o600);
    let size = 0;
    try {
      for await (const chunk of input) {
        size += chunk.length;
        requireThat(size <= budget && !state.exceeded, 'download exceeds quarantine byte limit');
        await output.writeFile(chunk);
      }
    } finally { input.destroy(); }
    requireThat(!state.exceeded, 'download exceeds quarantine byte limit');
    return { download: basename(target), downloadBytes: size, quarantined: true, ...await browserState(page) };
  } catch (error) {
    await fs.rm(target, { force: true });
    throw error;
  } finally {
    clearInterval(timer);
    downloadExpected = false;
    activeDownload = null;
    await output?.close();
    await item?.cancel().catch(() => {});
    await item?.delete().catch(() => {});
    await downloadBehavior(false);
    for (const name of await fs.readdir(join(STATE, 'download-staging'))) await fs.rm(join(STATE, 'download-staging', name), { force: true });
  }
}
async function browserAction(action, args) {
  if (action === 'open') {
    requireThat(context.pages().length < 4, 'the session already has four tabs');
    const page = await context.newPage();
    try { await page.goto(args.url, { waitUntil: 'domcontentloaded' }); }
    catch (error) { await page.close(); throw error; }
    return browserState(page, true);
  }
  if (action === 'close') {
    const page = context.pages().at(-1);
    if (page) await page.close();
    return { tabs: context.pages().length };
  }
  const page = currentPage();
  switch (action) {
    case 'navigate': await page.goto(args.url, { waitUntil: 'domcontentloaded' }); return browserState(page, true);
    case 'inspect': return browserState(page, true);
    case 'click':
      if (args.selector !== undefined) await page.click(args.selector);
      else await page.mouse.click(args.x, args.y);
      return browserState(page, true);
    case 'type': await page.fill(args.selector, args.text); break;
    case 'key': await page.keyboard.press(args.key); break;
    case 'scroll': {
      const amount = args.amount ?? 400;
      await page.mouse.wheel(args.direction === 'left' ? -amount : args.direction === 'right' ? amount : 0, args.direction === 'up' ? -amount : args.direction === 'down' ? amount : 0);
      break;
    }
    case 'screenshot': case 'frame': {
      const cap = action === 'frame' ? VIEWER_IMAGE : MODEL_IMAGE;
      const buffer = await page.screenshot({ type: 'png', fullPage: false, timeout: TIMEOUT });
      return { ...await browserState(page), ...imageResult(buffer, cap) };
    }
    case 'upload': await upload(page, args); break;
    case 'download': return download(page, args);
    default: throw new Error('unknown browser action');
  }
  return browserState(page);
}

async function startDesktop() {
  for (const name of ['xvfb', 'xauth', 'openbox', 'xdotool', 'ffmpeg']) await executable(options[name], name);
  requireThat(Object.keys(options.allowlist).length > 0, 'private desktop requires a reviewed executable allowlist');
  for (const [name, path] of Object.entries(options.allowlist)) await executable(path, name);
  const authDir = await fs.mkdtemp(join(STATE, 'runtime', 'xauth-'));
  await fs.chmod(authDir, 0o700);
  desktop = { authority: join(authDir, 'Xauthority'), authDir, closed: false };
  await run(options.xauth, ['-f', desktop.authority, 'add', DISPLAY, 'MIT-MAGIC-COOKIE-1', randomBytes(16).toString('hex')]);
  await fs.chmod(desktop.authority, 0o600);
  const server = startChild(options.xvfb, [DISPLAY, '-screen', '0', '1280x800x24', '-nolisten', 'tcp', '-auth', desktop.authority, '-noreset']);
  await started(server);
  const deadline = Date.now() + 10_000;
  let ready = false;
  while (Date.now() < deadline) {
    requireThat(server.exitCode === null && !server.failure, 'private Xvfb exited');
    try {
      const geometry = await run(options.xdotool, ['getdisplaygeometry'], { display: true, timeout: 1000 });
      ready = geometry.toString('utf8').trim() === '1280 800';
      if (ready) break;
    } catch { /* Xvfb is still initializing; bounded readiness polling. */ }
    await delay(50);
  }
  requireThat(ready, 'private authenticated Xvfb did not become ready');
  const wm = startChild(options.openbox, ['--sm-disable'], { display: true });
  await started(wm);
  for (const child of [server, wm]) child.once('exit', () => { if (!stopping && !desktop.closed) void shutdown(1); });
}
function desktopKey(key) {
  const aliases = { Control: 'ctrl', Meta: 'super', Alt: 'alt', Shift: 'shift', ArrowUp: 'Up', ArrowDown: 'Down', ArrowLeft: 'Left', ArrowRight: 'Right', Enter: 'Return', Backspace: 'BackSpace', Escape: 'Escape', Space: 'space', PageUp: 'Prior', PageDown: 'Next' };
  return key.split('+').map((part) => aliases[part] || part).join('+');
}
async function desktopAction(action, args) {
  requireThat(!desktop.closed, 'private desktop is closed');
  if (action === 'launch') {
    requireThat(applications.size < 4, 'the private desktop already has four applications');
    const entry = Object.entries(options.allowlist).find(([name, path]) => args.executable === name || args.executable === path);
    requireThat(entry, 'executable is not in the reviewed desktop allowlist');
    const [name, path] = entry;
    let argv;
    if (name === 'chromium') {
      await privateDirectory(join(STATE, 'desktop-chromium'));
      argv = [...chromiumArgs(), `--user-data-dir=${join(STATE, 'desktop-chromium')}`, 'about:blank'];
    } else argv = ['-geometry', '120x40', '-title', 'Meshra private terminal'];
    const child = startChild(path, argv, { display: true });
    applications.add(child);
    await started(child);
    return { launched: name };
  }
  if (action === 'snapshot' || action === 'frame') {
    const cap = action === 'frame' ? VIEWER_IMAGE : MODEL_IMAGE;
    const buffer = await run(options.ffmpeg, [
      '-nostdin', '-hide_banner', '-loglevel', 'error', '-f', 'x11grab',
      '-video_size', '1280x800', '-draw_mouse', '1', '-i', DISPLAY + '.0',
      '-frames:v', '1', '-threads', '1', '-f', 'image2pipe', '-vcodec', 'png',
      '-compression_level', '9', 'pipe:1',
    ], { display: true, cap, timeout: TIMEOUT });
    requireThat(buffer.length >= 24 && buffer.readUInt32BE(16) === WIDTH && buffer.readUInt32BE(20) === HEIGHT, 'private screenshot has invalid dimensions');
    return { ...imageResult(buffer, cap), width: WIDTH, height: HEIGHT };
  }
  if (args.windowId !== undefined) await run(options.xdotool, ['windowactivate', '--sync', String(args.windowId)], { display: true });
  switch (action) {
    case 'click': await run(options.xdotool, ['mousemove', '--sync', String(args.x), String(args.y), 'click', '1'], { display: true }); return { clicked: true };
    case 'type': await run(options.xdotool, ['type', '--clearmodifiers', '--delay', '0', '--', args.text], { display: true }); return { typed: true };
    case 'key': await run(options.xdotool, ['key', '--clearmodifiers', desktopKey(args.key)], { display: true }); return { pressed: true };
    case 'scroll': {
      const button = { up: 4, down: 5, left: 6, right: 7 }[args.direction];
      await run(options.xdotool, ['click', '--clearmodifiers', '--repeat', String(args.amount ?? 3), '--delay', '20', String(button)], { display: true });
      return { scrolled: true };
    }
    case 'close':
      if (args.windowId !== undefined) {
        await run(options.xdotool, ['windowclose', String(args.windowId)], { display: true });
        return { closed: true, windowId: args.windowId };
      }
      desktop.closed = true;
      await stopChildren();
      return { closed: true };
    default: throw new Error('unknown desktop action');
  }
}

const BROWSER_SCHEMAS = {
  open: ['url'], navigate: ['url'], inspect: [], click: ['selector', 'x', 'y'],
  type: ['selector', 'text'], key: ['key'], scroll: ['direction', 'amount'],
  screenshot: [], frame: [], upload: ['selector', 'path'], download: ['selector'], close: [],
};
const DESKTOP_SCHEMAS = {
  launch: ['executable'], snapshot: [], frame: [], click: ['x', 'y', 'windowId'],
  type: ['text', 'windowId'], key: ['key', 'windowId'],
  scroll: ['direction', 'amount', 'windowId'], close: ['windowId'],
};
function validate(frame) {
  requireThat(object(frame) && Object.keys(frame).every((key) => ['id', 'action', 'args'].includes(key)), 'invalid request envelope');
  requireThat((Number.isSafeInteger(frame.id) && frame.id >= 0) || (typeof frame.id === 'string' && frame.id.length > 0 && Buffer.byteLength(frame.id) <= 128), 'invalid request id');
  requireThat(typeof frame.action === 'string' && object(frame.args), 'request requires action and args');
  const { action, args } = frame;
  const schema = (options.mode === 'browser' ? BROWSER_SCHEMAS : DESKTOP_SCHEMAS)[action];
  requireThat(Array.isArray(schema) && Object.keys(args).every((key) => schema.includes(key)), 'unknown action or argument');
  if (action === 'open' || action === 'navigate') {
    bounded(args.url, 2048, 'URL');
    const url = new URL(args.url);
    requireThat(['http:', 'https:'].includes(url.protocol) && !url.username && !url.password, 'only HTTP(S) URLs without credentials are permitted');
  }
  const selectorRequired = ['upload', 'download'].includes(action) || (action === 'type' && options.mode === 'browser');
  if (selectorRequired || args.selector !== undefined) bounded(args.selector, 1024, 'selector');
  if (action === 'click') {
    if (options.mode === 'browser' && args.selector !== undefined) requireThat(args.x === undefined && args.y === undefined, 'click takes a selector or coordinates, not both');
    else { integer(args.x, 0, WIDTH - 1, 'x'); integer(args.y, 0, HEIGHT - 1, 'y'); }
  }
  if (action === 'type') bounded(args.text, 4096, 'text', true);
  if (action === 'key') {
    bounded(args.key, 128, 'key');
    const pieces = args.key.split('+');
    requireThat(pieces.length <= 5 && pieces.slice(0, -1).every((part) => ['Control', 'Alt', 'Shift', 'Meta'].includes(part)), 'invalid key modifiers');
    requireThat(/^(?:[A-Za-z0-9]|F(?:[1-9]|1[0-2])|Enter|Tab|Escape|Backspace|Delete|Insert|Home|End|PageUp|PageDown|ArrowUp|ArrowDown|ArrowLeft|ArrowRight|Space)$/.test(pieces.at(-1)), 'unsupported key');
  }
  if (action === 'scroll') {
    requireThat(['up', 'down', 'left', 'right'].includes(args.direction), 'invalid scroll direction');
    if (args.amount !== undefined) integer(args.amount, 1, options.mode === 'desktop' ? 100 : 2000, 'scroll amount');
  }
  if (action === 'upload') bounded(args.path, 4096, 'staged upload path');
  if (action === 'launch') bounded(args.executable, 4096, 'allowlisted executable');
  if (args.windowId !== undefined) integer(args.windowId, 1, 0xffffffff, 'windowId');
}
async function emit(value) {
  const line = JSON.stringify(value) + '\n';
  requireThat(Buffer.byteLength(line) <= MAX_OUTPUT, 'bounded protocol result exceeded');
  if (!process.stdout.write(line)) await once(process.stdout, 'drain');
}
async function* lines() {
  let pending = Buffer.alloc(0);
  for await (const chunk of process.stdin) {
    let start = 0;
    for (let index = 0; index < chunk.length; index += 1) {
      if (chunk[index] !== 10) continue;
      requireThat(pending.length + index - start <= MAX_INPUT, 'protocol line exceeds 16 KiB');
      const line = pending.length ? Buffer.concat([pending, chunk.subarray(start, index)]) : chunk.subarray(start, index);
      pending = Buffer.alloc(0);
      start = index + 1;
      if (line.length) yield line.toString('utf8');
    }
    requireThat(pending.length + chunk.length - start <= MAX_INPUT, 'protocol line exceeds 16 KiB');
    pending = Buffer.concat([pending, chunk.subarray(start)]);
  }
  requireThat(pending.length === 0, 'unterminated JSONL request');
}
async function stopChildren() {
  const active = [...children];
  for (const child of active) killGroup(child);
  if (active.length) await delay(200);
  // Kill the complete process groups even when the direct parent has exited.
  for (const child of active) killGroup(child, 'SIGKILL');
  children.clear();
  applications.clear();
}
async function shutdown(code = 0) {
  if (stopping) return;
  stopping = true;
  if (desktop) desktop.closed = true;
  const deadline = setTimeout(() => process.exit(code), 3000);
  deadline.unref();
  for (const socket of sockets) socket.destroy();
  proxy?.close();
  await stopChildren();
  if (context) await context.close().catch(() => {});
  if (desktop?.authDir) await fs.rm(desktop.authDir, { recursive: true, force: true }).catch(() => {});
  process.exit(code);
}
async function main() {
  process.umask(0o077);
  options = parseOptions(process.argv.slice(2));
  await privateDirectory(STATE);
  for (const name of ['desktop-home', 'runtime', 'uploads', 'downloads', 'download-staging']) await privateDirectory(join(STATE, name));
  // Staging is disposable, never treated as an authorized upload or attachment.
  for (const name of await fs.readdir(join(STATE, 'download-staging'))) await fs.rm(join(STATE, 'download-staging', name), { recursive: true, force: true });
  await startProxy();
  if (options.mode === 'browser') await launchBrowser();
  else await startDesktop();
  await emit({ type: 'ready', mode: options.mode, ...(options.mode === 'desktop' ? { width: WIDTH, height: HEIGHT } : { tabs: 0 }) });
  // Await each action and each reply before reading another line. Input cannot
  // create an unbounded queue or race browser state, downloads or child cleanup.
  for await (const line of lines()) {
    let frame;
    let id = null;
    try {
      frame = JSON.parse(line);
      validate(frame);
      id = frame.id;
      const result = await (options.mode === 'browser' ? browserAction(frame.action, frame.args) : desktopAction(frame.action, frame.args));
      await emit({ id, ok: true, result });
    } catch (error) {
      if (object(frame) && ((Number.isSafeInteger(frame.id) && frame.id >= 0) || (typeof frame.id === 'string' && Buffer.byteLength(frame.id) <= 128))) id = frame.id;
      await emit({ id, ok: false, error: truncate(String(error?.message || error), 512) });
    }
  }
  await shutdown();
}
process.on('SIGTERM', () => { void shutdown(); });
process.on('SIGINT', () => { void shutdown(); });
process.stdout.on('error', () => { void shutdown(1); });
process.on('uncaughtException', () => { void shutdown(1); });
process.on('unhandledRejection', () => { void shutdown(1); });
main().catch(async (error) => {
  try { await emit({ type: 'error', error: truncate(String(error?.message || error), 512) }); } catch { /* closed protocol */ }
  await shutdown(1);
});
