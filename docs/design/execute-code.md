# Sandboxed code execution and nested tools

## Contract

`meshra_execute_code` is available only to Direct API profiles that have both workspace read and command capabilities. It accepts a Python 3 script (at most 60 KiB) and a 1–300 second timeout. Python must be explicitly configured as `executables.python3` in the reviewed runtime manifest. There is no host execution fallback. The exact source, interpreter, runtime revision and workspace baseline are covered by the ordinary invocation action hash and approved before startup, like a workspace command. A script runs in a read-only private snapshot with the existing namespace, network, cgroup and output limits. It cannot directly stage workspace writes; nested approved workspace operations can make changes using their existing broker semantics. Script output is untrusted data.

Snapshot initialization is serialized per broker. An admitted code call reserves the broker's mutation slot through approval and startup; immediately before startup it verifies that the pinned snapshot and its approved baseline hash still match. The reservation is released only after the confined process starts, and socket dispatch waits for that release, so subsequently nested mutations can proceed through their own broker checks without a startup race.

The worker receives only a per-invocation Unix socket at `/run/meshra/broker.sock`. Python's standard library can send an HTTP POST to `/call` with a JSON object `{ "name": "meshra_...", "arguments": { ... } }`. The response is `{ "result": <normal tool result> }` or `{ "error": <message> }`. Socket access is the scoped capability; its private host directory and socket are mode 0700/0600 and the endpoint is closed and unlinked when the worker finishes, fails or is cancelled. It is not a relay credential and cannot reach the general MCP endpoint. The worker receives no ambient credentials, inherited environment, host home or network.

For example, a script can call a tool with Python's standard library:

```python
import http.client
import json
import socket

class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect("/run/meshra/broker.sock")

conn = UnixHTTP("localhost", timeout=60)
conn.request("POST", "/call", json.dumps({
    "name": "meshra_workspace_read",
    "arguments": {"path": "README.md", "limit": 4096},
}), {"Content-Type": "application/json"})
response = conn.getresponse()
print(response.status, response.read().decode())
conn.close()
```

The endpoint accepts only one in-flight request, bounds request and response size and invokes the same active `toolBroker.Call` used by the model, with a server-generated unique call ID. Thus the admitted catalog, canonical arguments, action hashes, invocation records, approvals, human session ownership, policy and runtime revision checks, epoch revocation and shared per-turn call budget all remain authoritative. Nested `meshra_execute_code` is rejected to prevent recursive worker/socket exhaustion. A nested call is cancelled with the parent invocation; endpoint teardown cancels and drains its handler before returning. A pending nested approval can consume the script deadline and is invalidated by normal broker cancellation.

## Threat model

The script, its subprocesses, broker arguments and returned tool content are adversarial. A script may try to escape namespaces, write the original workspace, reach the network or host sockets, steal bearer credentials, forge approvals, replay calls, exceed turn/worker/output limits, hold approvals open, reuse the socket after completion, or recursively launch workers. Security relies on the pre-existing fail-closed sandbox (bubblewrap namespaces, private snapshot, no network, born-in-cgroup process and process-tree cleanup), its operator-reviewed runtime manifest, and a local OS user who controls the daemon. The new socket is the only intentional bridge; it is not a generic HTTP proxy and dispatches only through the live broker. Script bytes never become shell text. Script-controlled IDs, endpoint paths, executor identities and approval state are never accepted.

A same-UID hostile host process or compromised kernel/runtime executable is outside this boundary, as for existing sandbox workers. Operators must trust the reviewed interpreter and system runtime mounts. Prompt injection and information disclosed by an authorized nested tool remain risks; the script is allowed to combine data it legitimately receives. Workspace read capability is mandatory because the read-only snapshot is directly visible to code. No safety claim depends on output redaction for authorization. On missing Python, socket setup, namespace or cgroup support, invalid active attempt, revocation, timeout or broker failure, execution fails closed.

Tests should cover interpreter/argument validation, capability/catalog gating, exact approval hash, absence of host/network access, nested authorization and approval denial, catalog/turn limits, recursive rejection, output/request bounds, cancellation and socket cleanup, and rejection of mutation through the read-only mount. Real sandbox tests require Linux bubblewrap, user namespaces and a delegated cgroup-v2 subtree supporting clone3 into cgroup.
