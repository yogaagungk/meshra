# Claude Agent SDK and OpenCode adapters

## Compatibility evidence

Implementation references were read before choosing protocol methods:

- [Claude Agent SDK TypeScript reference](https://code.claude.com/docs/en/agent-sdk/typescript): `query({prompt, options})`, `resume`, `SessionStart` / system `init` native session IDs, `canUseTool`, `PreToolUse`, `AbortController`, `includePartialMessages`, and HTTP `mcpServers` with `headers`.
- [OpenCode server](https://opencode.ai/docs/server/), [SDK](https://opencode.ai/docs/sdk/), [permissions](https://opencode.ai/docs/permissions/), [configuration](https://opencode.ai/docs/config/), and [MCP servers](https://opencode.ai/docs/mcp-servers/).
- [OpenCode 1.19.3 generated protocol types](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/sdk/js/src/v2/gen/types.gen.ts), including legacy `/session` APIs, session-level permission rules, `/permission/{requestID}/reply`, and the v2 permission event/reply shape.
- [OpenCode 1.19.3 serve implementation](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/serve.ts), including its readiness line and per-request directory header.

Inspected environment: Node **24.22.0**, npm **11.19.1**, Linux x64/WSL2. Neither `claude` nor `opencode` was initially on PATH. Local dependency installation completed; native `--version` inspection reported:

| Component | Exact version | Evidence |
| --- | --- | --- |
| `@anthropic-ai/claude-agent-sdk` | **0.3.303** | Installed package manifest and npm lock |
| Bundled Claude Code | **2.1.303** | Bundled executable `--version` |
| `opencode-ai` | **1.19.3** | Separate installed manifest, npm lock, executable `--version` |

**Authenticated provider turns remain unverified** for Claude and OpenCode because the inspected environment has no native credentials/API-key environment for them. The capability flag `authenticatedSmokeVerified` remains false. Deterministic contracts passed in the integrated race suite. A real OpenCode 1.19.3 server additionally passed authenticated health, session create/resume, permission PATCH, deletion and rejection of unauthenticated requests, without a provider turn. See [verification.md](verification.md) for actual outcomes. Installation/version inspection alone is not provider authentication or authenticated harness MCP proof.

## Installation and discovery

From the repository root:

```sh
npm ci --prefix bridges/claude-agent-sdk --ignore-scripts
npm ci --prefix bridges/claude-agent-sdk/opencode
```

Both manifests pin exact direct dependency versions and include npm locks. Claude's SDK bundles its native binary as a platform optional dependency; do not omit optional dependencies. OpenCode is deliberately a **separate** dependency installation, not a global CLI change. Recent npm versions may require explicit approval of `opencode-ai`'s postinstall script. Review `node_modules/opencode-ai/postinstall.mjs` and use your npm version's install-script approval mechanism if prompted. The inspected npm install already provided a working local executable; no global installation or user harness configuration was changed.

Keep `bridges/claude-agent-sdk` beside the built Meshra binary when packaging, including `bridge.mjs`, its installed dependencies, and the nested OpenCode installation. Checkout-relative and source-relative discovery also work in development. To relocate these artifacts, set:

- `MESHRA_CLAUDE_BRIDGE=/absolute/path/to/bridge.mjs`
- `MESHRA_OPENCODE_BIN=/absolute/path/to/opencode`

These select executable artifacts, not provider credentials or persisted agent configuration. Claude requires `node` on the daemon PATH. OpenCode also accepts `opencode` on PATH, but checks the owned server's health version and refuses versions other than **1.19.3**. Missing dependencies fail with installation instructions. No adapter starts a paid prompt in its Factory.

Configure provider credentials using each harness's supported native authentication or environment mechanism **before starting the daemon**. Claude accepts the native default model or the agent's `model`; a nonempty `provider` must be `anthropic` (alternate SDK transports remain a native environment concern). OpenCode accepts either both `provider` and `model`, or neither to use its native defaults. Meshra never saves provider API keys or bearer tokens in agent configuration.

## Session and process ownership

### Claude

`internal/adapters/claude.New()` runs the actual Claude Agent SDK in `bridges/claude-agent-sdk/bridge.mjs`, not the generic Anthropic Messages API. A fresh runtime has no fabricated native ID. The first paid `Run` uses SDK session allocation. The sidecar emits the native session ID from `SessionStart` immediately, with system `init` / result as fallbacks. The Go adapter emits `session` synchronously so the relay can durably update its mapping and the task-bound token before MCP calls. Later turns pass only that saved ID as `options.resume`. There is no `continue`, latest-session lookup, interactive-terminal attach, fork, or random UUID passed as a resumed session. A changed native ID on resume fails rather than remapping the identity.

Each turn has one owned bridge process and one SDK query. Text `stream_event` deltas become `delta` events; only `result` supplies the final return value. SDK permission denials become `approval_required`, never raw frame dumps. Errors are bounded/redacted in the sidecar, bearer tokens are redacted again at the Go boundary, and raw SDK stderr is discarded. Cancellation sends a process-group termination signal, allowing the sidecar's AbortController to run, with forced process/pipe cleanup bounded by Go's wait delay and a final process-group kill. Close cancels an active turn and waits for it to be reaped.

### OpenCode

`internal/adapters/opencode.New()` uses Go HTTP, not a JavaScript model client. Every turn starts a new owned `opencode serve --hostname 127.0.0.1 --port 0`; this is never an arbitrary user's existing TUI/server. Its readiness URL must be `http://127.0.0.1:<port>`. Each process gets a fresh random 256-bit `OPENCODE_SERVER_PASSWORD`, explicit username `opencode`, and an HTTP client with proxy/redirect forwarding disabled. Every request uses Basic authentication and `x-opencode-directory`.

A new identity calls `POST /session` and immediately emits its returned native ID. A saved identity calls `GET /session/{savedID}`; an absent/mismatched session fails and is never silently replaced. A returned directory mismatch fails. `PATCH /session/{id}` sets the safe session-level permission rules on every turn, including resumed sessions. `POST /session/{id}/message` returns final text parts. The adapter checks the result session ID and model error envelope. **Text streaming is not implemented**, so `stream=false`; the separate SSE connection exists only for permission supervision. HTTP and SDK methods can expose more capabilities than this adapter claims.

Cancellation calls `POST /session/{id}/abort` with a bounded independent context, then the owned process group is terminated and reaped. All turn exit paths stop that turn's server. OpenCode's own persistent native session store is intentionally retained for resume; stopping Meshra does not erase harness history.

## Permissions and MCP

There is no interactive approval UI. Both adapters deny shell execution, file mutation, subagent execution, and unknown tools by default. A denial produces an observable `approval_required` event; it is **not** a pending request that a human can later approve in this MVP. Do not interpret this event as successful execution.

Claude explicitly uses `permissionMode: default`, never bypass mode, and disables user/project/local SDK settings (`settingSources: []`) and undeclared MCP servers (`strictMcpConfig: true`). A `PreToolUse` hook denies non-allowlisted tools before automatic permission rules can approve them; `canUseTool` is a second fail-closed permission boundary. The allowlist contains `Read`, `Glob`, `Grep`, and the seven known Meshra MCP operations. This intentionally does not provide normal autonomous code editing or a shell. Managed harness policy and native authentication still apply; this is not an OS sandbox.

OpenCode uses session-level catch-all `ask` rules plus a small read/known-Meshra-tool allowlist. Before a prompt it opens `/event`; matching-session `permission.asked` and `permission.v2.asked` requests produce `approval_required` and receive `reply: reject`. Loss or malformed data on the permission supervision stream aborts the turn rather than leaving potentially unobserved work running. Mutating tools are not approved automatically. Native OpenCode configuration can load trusted plugins/instructions and native credentials; Meshra does not rewrite or sandbox these. Treat the configured checkout and the native harness configuration as trusted executable inputs.

The relay supplies a **per-turn identity/session/task-bound** bearer token, not an admin token:

- Claude passes `mcpServers.meshra = {type: "http", url: Turn.MCPURL, headers: {Authorization: "Bearer " + Turn.Token}}` in memory. SDK strict MCP mode excludes other configured servers.
- OpenCode uses documented `OPENCODE_CONFIG_CONTENT` in the owned process environment: `mcp.meshra = {type: "remote", url: Turn.MCPURL, enabled: true, oauth: false, headers: {Authorization: "Bearer " + Turn.Token}}`. There is a new server for every turn, so a previous token cannot remain in a reused MCP client. No HTTP config PATCH, MCP-add route, or config file is used for credentials. `oauth: false` prevents bearer-only MCP from starting OAuth registration or writing MCP OAuth credentials. Automatic sharing and updating are disabled in this process's inline config.

Meshra does not write these credentials to disk or place them in prompts/argv. Process environments are visible to the same OS user and privileged processes; the local-user trust boundary still applies. Native harness session transcripts may contain prompts/results and should be protected as sensitive data.

## Contract tests and opt-in paid smoke

Main integration should run (from the root, after all changes land):

```sh
go test -race ./internal/adapters/claude ./internal/adapters/opencode
```

These default tests require no provider accounts. Claude uses a fake bridge executable over the actual JSONL/process transport; OpenCode uses `httptest` with the same authenticated HTTP/session/permission event sequence. Coverage includes no paid Factory turn, initial native session persistence, resume, independent identities, explicit denial, protocol/upstream failures, no silent session replacement, cancellation, native abort, and owned runtime cleanup. Private constructor injection is available to package tests without adding transport configuration to persisted agent records.

The following commands **can incur provider charges** and are intentionally opt-in. Authenticate natively first, keep credentials out of shell history, and run each independently:

```sh
# Claude: native SDK authentication must already be available.
MESHRA_AUTH_SMOKE=1 go test -v ./internal/adapters/claude -run '^TestAuthenticatedSmoke$' -count=1

# OpenCode: choose an authenticated provider/model supported by your installation.
# If both are omitted, the native default is used instead.
MESHRA_AUTH_SMOKE=1 \
  MESHRA_OPENCODE_PROVIDER=anthropic \
  MESHRA_OPENCODE_MODEL=claude-sonnet-4-5 \
  go test -v ./internal/adapters/opencode -run '^TestAuthenticatedSmoke$' -count=1
```

Each smoke allocates a real native session and performs a second turn through resume. A pass establishes those paid basic prompt/resume paths only; it is not proof of cross-agent routing, MCP, all models, cancellation during a real tool call, or permission isolation under every native customization. No such pass is claimed here.

For authenticated end-to-end MCP/identity validation, start `meshra serve` from the daemon environment with native authentication available. Create **two** Claude identities with different names and working directories and an OpenCode identity, using the CLI examples in the main README. Send a uniquely marked prompt to one Claude identity and verify only its task/session advances. Ask that identity to use `meshra_request` to queue a message to the other, record the returned task ID, and poll `meshra_task_status` / `meshra_task_events` rather than waiting synchronously. Repeat with OpenCode as origin and destination. Inspect task events and native session IDs, restart the daemon, then send another prompt to each and confirm its saved native mapping is resumed. Finally request a shell/file mutation and verify `approval_required` appears without the requested side effect. Those manual procedures remain **unverified** until actually run in an authenticated environment.

Known limitations: no arbitrary interactive attach, no approval/re-approval UI, read-oriented tool policy, OpenCode final-only text, startup cost per turn, native harness storage outside Meshra's database, and no claim of authenticated cross-harness/MCP success from contract tests alone.
