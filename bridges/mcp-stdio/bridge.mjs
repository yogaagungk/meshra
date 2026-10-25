#!/usr/bin/env node
import http from "node:http";
import { once } from "node:events";

const socketPath = process.env.MESHRA_TOOL_SOCKET;
const credential = process.env.MESHRA_TOOL_TOKEN;
// The mounted broker socket is absolute and ends in .sock; the scoped
// credential is bound to one active attempt.
if (!socketPath || !socketPath.startsWith("/") || !socketPath.endsWith(".sock") || !credential || credential.length > 256) {
  process.stderr.write("A mounted active-attempt tool socket and scoped credential are required.\n");
  process.exit(1);
}
const maxFrame = 1024 * 1024;
const maxInflight = 32;
const decoder = new TextDecoder("utf-8", { fatal: true });
let session;
let pending = Buffer.alloc(0);
let output = Promise.resolve();
const requests = new Set();
const replied = new WeakSet();
let inflight = 0;
function emit(value) {
  const line = JSON.stringify(value) + "\n";
  if (Buffer.byteLength(line) > maxFrame) throw new Error("bounded tool frame exceeded");
  output = output.then(async () => { if (!process.stdout.write(line)) await once(process.stdout, "drain"); });
  output.catch(() => process.exit(1));
}
// Exactly one reply per request frame: a transport failure must not produce a
// second error frame for the same identifier.
function fail(frame, message) {
  if (!Object.hasOwn(frame, "id") || replied.has(frame)) return;
  replied.add(frame);
  emit({ jsonrpc: "2.0", id: frame.id, error: { code: -32603, message } });
}
function forward(frame) {
  if (inflight >= maxInflight) {
    fail(frame, "Too many bounded tool requests are in flight.");
    return;
  }
  const body = Buffer.from(JSON.stringify(frame));
  const headers = { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + credential, "Content-Length": body.length };
  if (session) headers["Mcp-Session-Id"] = session;
  inflight++;
  const request = http.request({ socketPath, path: "/mcp", method: "POST", headers }, response => {
    const chunks = [];
    let bytes = 0;
    response.on("data", chunk => {
      bytes += chunk.length;
      if (bytes > maxFrame) { response.destroy(new Error("bounded tool response exceeded")); return; }
      chunks.push(chunk);
    });
    response.on("end", () => {
      if (response.headers["mcp-session-id"]) session = response.headers["mcp-session-id"];
      if (response.statusCode === 202 || response.statusCode === 204) return;
      if (response.statusCode !== 200) { fail(frame, "Active tool endpoint denied or expired; request was not replayed."); return; }
      let text;
      try { text = decoder.decode(Buffer.concat(chunks)); }
      catch { fail(frame, "Invalid bounded tool response; request was not replayed."); return; }
      let parsed;
      try { parsed = JSON.parse(text); }
      catch { fail(frame, "Invalid bounded tool response; request was not replayed."); return; }
      if (replied.has(frame)) return;
      replied.add(frame);
      emit(parsed);
    });
    response.on("error", () => fail(frame, "Tool response interrupted; effects may be uncertain and will not be replayed."));
  });
  requests.add(request);
  request.on("close", () => { requests.delete(request); inflight--; });
  request.on("error", () => fail(frame, "Tool transport interrupted; effects may be uncertain and will not be replayed."));
  request.setTimeout(305000, () => request.destroy());
  request.end(body);
}
process.stdin.on("data", chunk => {
  pending = pending.length ? Buffer.concat([pending, chunk]) : chunk;
  let newline;
  while ((newline = pending.indexOf(10)) !== -1) {
    if (newline > maxFrame) process.exit(1);
    const line = pending.subarray(0, newline);
    pending = pending.subarray(newline + 1);
    if (!line.length) continue;
    let text;
    try { text = decoder.decode(line); }
    catch { emit({ jsonrpc: "2.0", id: null, error: { code: -32700, message: "Invalid bounded JSON-RPC frame" } }); continue; }
    let frame;
    try {
      frame = JSON.parse(text);
      if (!frame || Array.isArray(frame) || typeof frame !== "object" || frame.jsonrpc !== "2.0") throw new Error("invalid frame");
    } catch { emit({ jsonrpc: "2.0", id: null, error: { code: -32700, message: "Invalid bounded JSON-RPC frame" } }); continue; }
    forward(frame);
  }
  if (pending.length > maxFrame) process.exit(1);
});
function close() { for (const request of requests) request.destroy(); process.exit(0); }
process.stdin.on("end", close);
process.on("SIGTERM", close);
process.on("SIGINT", close);
