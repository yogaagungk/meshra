# Harness support and compatibility

Adapter support is intentionally asymmetric. A registered identity is durable even when its harness binary/authentication is unavailable. `start` owns or prepares only that identity's native session. There is no attach-to-arbitrary-terminal API.

| Adapter | Inspected implementation | Prompt | Text stream | Resume | Cancel | Automatic task-bound MCP | Tool/approval policy |
| --- | --- | --- | --- | --- | --- | --- | --- |
| OMP | CLI 18.12.1, custom RPC v2 over stdio | Yes | Yes | Saved session ID in per-identity session directory | Native abort, forced process cleanup if needed | No | Builtins/discovery/MCP disabled; no auto-approval |
| Codex | CLI 0.163.0, App Server, generated installed schemas | Yes | Yes | Saved thread ID | Native turn interrupt, forced cleanup if needed | No | Read-only sandbox; network disabled; approval requests explicitly declined. Profile-managed identities serve the Meshra broker catalog through the installed experimental dynamic tool API with every native tool surface disabled and verified |
| Claude | Agent SDK 0.3.303, bundled Claude Code 2.1.303 | Yes | Yes | SDK native session ID | AbortController and owned process-group cleanup | Yes | Read/search and Meshra tools allowed; unsafe/unknown tools denied visibly |
| OpenCode | CLI 1.19.3, owned localhost HTTP server | Yes | **No**, final text only | Saved HTTP session ID | HTTP abort and owned process-group cleanup | Yes | Read/search and Meshra tools allowed; permission events explicitly rejected |

“Yes” means implemented capability, not proof that every provider account/configuration has passed a live test. The [v0.2 adapter verification report](V0.2_ADAPTER_VERIFICATION.md) records fresh installed-version, schema, native credential-free protocol evidence, credential prerequisites, and the worktree-write enforcement matrix. Earlier authenticated outcomes remain in [verification.md](verification.md), separate from deterministic protocol contracts. Runtime capability metadata makes these differences discoverable.

## Verified interface sources

- [OMP RPC reference](https://github.com/can1357/oh-my-pi/blob/main/docs/rpc.md): `ready`, v2 negotiation/chunk frames, command IDs, `get_state`, `new_session`, `prompt`, `prompt_result`, `abort`, `get_last_assistant_text`, async deltas. This is **not JSON-RPC 2.0**.
- [Codex App Server reference](https://developers.openai.com/codex/app-server/): initialize/initialized, thread/start and resume, turn/start and interrupt, streamed item/turn events and explicit server approval requests. Installed `codex app-server generate-json-schema --out /tmp/meshra-codex-schema` generated the version-matching schemas used during implementation.
- [Claude Agent SDK TypeScript reference](https://code.claude.com/docs/en/agent-sdk/typescript): `query`, options, hooks, session resume, tool permissions and HTTP MCP configuration. This is the Agent SDK running Claude Code, **not** the generic Anthropic model API.
- [OpenCode server](https://opencode.ai/docs/server/), [SDK](https://opencode.ai/docs/sdk/), [v1.19.3 generated types](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/sdk/js/src/v2/gen/types.gen.ts): server health, sessions, message prompt, abort, event stream and permission replies. OpenCode v2 is a separate upstream generation; this adapter deliberately requires the inspected 1.19.3 server.

Exact commands, frame fields, version limitations, safety controls and native state placement: [stdio adapter evidence](adapters-stdio.md) and [SDK/HTTP adapter evidence](adapters-sdk.md). MCP manual registration examples: [CLI integration guide](cli.md#manual-harness-mcp-registration). No user harness configuration is changed automatically.

## Installation

OMP/Codex must be on the daemon PATH. Install/authenticate through their native procedures. The Codex adapter isolates its runtime configuration; custom provider definitions from a user's normal Codex config are not inherited. It shares the native auth file only, not secrets copied into SQLite.

```sh
npm ci --prefix bridges/claude-agent-sdk --ignore-scripts
npm ci --prefix bridges/claude-agent-sdk/opencode
```

Claude's optional platform package includes its native CLI. OpenCode is a separate local installation; review/approve its postinstall with your npm version if required. Override relocated executable discovery only with `MESHRA_CLAUDE_BRIDGE` or `MESHRA_OPENCODE_BIN`; do not add keys to database configuration.

The current [credential-presence inspection](V0.2_ADAPTER_VERIFICATION.md#credential-presence-not-credential-disclosure) found Codex native auth storage and OMP's default native database, but no checked provider API-key environment variables, Claude login, or OpenCode native auth file. Storage presence is not proof of valid credentials or model access. No credential values were printed.

## Real smoke procedures

All commands below may contact model providers and incur charges. Authenticate natively and launch from the intended environment first. See each adapter's detailed procedure for model/provider overrides.

```sh
MESHRA_REAL_OMP=1 go test -v ./internal/adapters/omp -run '^TestRealSmoke$' -count=1
MESHRA_REAL_CODEX=1 go test -v ./internal/adapters/codex -run '^TestRealSmoke$' -count=1
MESHRA_AUTH_SMOKE=1 go test -v ./internal/adapters/claude -run '^TestAuthenticatedSmoke$' -count=1
MESHRA_AUTH_SMOKE=1 go test -v ./internal/adapters/opencode -run '^TestAuthenticatedSmoke$' -count=1
```

A successful prompt/resume smoke establishes only that exercised native path. It does not prove real destructive-tool cancellation, every approval variant, every provider, or cross-harness MCP. For actual routed flows, run the named-instance commands in the README and inspect immutable origin/session/correlation fields and persisted results.

## Limits

- OMP/Codex relay-owned runtimes do not advertise MCP. They can be destinations and CLI-originated sources; use documented manually registered MCP in a separately launched harness when needed. Claude/OpenCode can automatically call the relay during owned turns.
- No adapter silently chooses “the latest session” when resume fails. Replacing a missing native session requires an intentional new identity rather than silently changing reply ownership.
- The MVP has no interactive approval console. Denied tools are visible; they are not pending approvals that can later be resumed.
- Claude/OpenCode start owned processes per turn to avoid stale credentials/heavy stopped runtimes. Native session history survives outside SQLite; protect those stores too.
- Harnesses are separate memory/CPU/cost consumers. The idle relay measurement excludes them.
- Read-oriented tool policy is not a universal OS sandbox: Claude uses SDK hooks, OMP disables tools/discovery, and OpenCode inherits trusted native configuration/plugins. Codex requests the native read-only sandbox without an unrestricted fallback. See the [enforcement matrix](V0.2_ADAPTER_VERIFICATION.md#enforcement-matrix-worktreewrites-false); no adapter enables worktree writes or an approval bypass.
