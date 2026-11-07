# Harness support and compatibility

Adapter support is intentionally asymmetric. A registered identity is durable even when its harness binary/authentication is unavailable. `start` owns or prepares only that identity's native session. There is no attach-to-arbitrary-terminal API.

| Adapter | Inspected implementation | Prompt | Text stream | Resume | Cancel | Automatic task-bound MCP | Tool/approval policy |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Direct API | OpenAI-compatible `POST /chat/completions` over HTTP, no subprocess | Yes | SSE where supported; buffered and redacted by the relay | None; Meshra sends bounded history each turn | Request cancellation | Broker tools only | Brokered tools only; approved commands run in a private sandbox |
| OMP | CLI 18.12.1, custom RPC v2 over stdio | Yes | Yes | Saved session ID in per-identity session directory | Native abort, forced process cleanup if needed | No | **Full access**: builtin tools with `--approval-mode=yolo`; discovery/project MCP disabled. Tool-free profile drafts keep `--no-tools` |
| Claude | Agent SDK 0.3.303, bundled Claude Code 2.1.303 | Yes | Yes | SDK native session ID | AbortController and owned process-group cleanup | Yes | **Full access**: `bypassPermissions`, every tool allowed. Tool-free profile drafts still deny all tools |

### Capability inventory (backend, enforcement boundary, evidence)

| Runtime | Backend | Trusted mode boundary | Restricted mode boundary | Evidence level |
| --- | --- | --- | --- | --- |
| Direct API | HTTP to the selected endpoint, in-process agent loop | n/a (no native tools) | Broker catalog only; commands in a private bubblewrap sandbox with per-call approval | Deterministic tests; real-daemon smoke of mode gates; live provider not exercised here |
| OMP | `omp` RPC v2 subprocess on the host | None: `--approval-mode=yolo`, full host access | **Conditionally available.** Native `omp` runs in the bubblewrap worker with `--no-tools`; the daemon requires the exact admitted broker roster before any prompt. Unavailable until the operator manifest, a reviewed provider credential and the OS probe pass; production-path proof pending | Protocol fixtures and installed-version checks; no confinement proof |
| Claude | Agent SDK bridge, native Claude Code on the host | None: `bypassPermissions`, full host access | **Conditionally available.** Runs the SDK bridge in the bubblewrap worker (private HOME, no direct network, scoped broker socket, provider-only egress). Available only when the runtime manifest, a daemon-environment credential and the live OS probe all pass; otherwise unavailable with the unmet prerequisite named. No host fallback | Mechanism implemented; containment probe and bridge self-check passed in a manual bubblewrap run; **production-path proof pending a delegated-cgroup host**; no real-credential turn run |

The broker catalog limits what Meshra offers the model; it does not remove native tools in trusted mode. Native-specific conveniences remain native-specific.

“Yes” means implemented capability, not proof that every provider account/configuration has passed a live test. The [v0.2 adapter verification report](V0.2_ADAPTER_VERIFICATION.md) records fresh installed-version, schema, native credential-free protocol evidence, credential prerequisites, and the worktree-write enforcement matrix. Earlier authenticated outcomes remain in [verification.md](verification.md), separate from deterministic protocol contracts. Runtime capability metadata makes these differences discoverable.

## Direct API compatibility formats

Each provider connection has an `apiFormat`: `openai` (default),
`anthropic` or `typesafe`. `openai` posts to `<base>/v1/chat/completions` with a
bearer token. `anthropic` posts to `<base>/v1/messages` with `x-api-key` and
`anthropic-version: 2023-06-01`; system text becomes the `system` parameter, tool
calls become `tool_use` blocks, and consecutive tool results share one user
message. `typesafe` stores a System One connection (`https://api.typesafe.ai`) and
lists its models, but cannot run chat turns because System One answers typed
questions and neither generates text nor calls tools.

Anthropic-format endpoints use prompt caching by default (`promptCaching: "auto"`).
Set `promptCaching` to `off` to disable cache markers, or `1h` to
request the opt-in one-hour tier, which costs more to write. The default
ephemeral tier lasts about five minutes and is refreshed on use. A gateway that
explicitly rejects `cache_control` with HTTP 400 is retried without markers.
Marker eligibility uses a conservative text/schema byte estimate; image bytes
are excluded. Actual tokenization and model-specific minimums are determined by
the provider, so an eligible request may still be too small to cache.
OpenAI-format caching is provider-managed; `promptCacheKey` optionally sends a
stable key derived from the profile and conversation IDs and defaults to false
for compatibility with servers that reject unknown fields.
OpenCode Go requests to `opencode.ai/zen/go` include a stable, hashed
conversation identifier in `x-opencode-session`, and identify Meshra via
`User-Agent`, as required by [OpenCode Go](https://opencode.ai/v2/docs/console/go/#where-can-i-use-it).
OpenCode Go specifies that clients should send typical coding-agent traffic.

Cache reads and writes are token counts, not monetary savings. A long request
with no cache reads can indicate an unsupported endpoint, a cold or expired
cache, or a changed prefix. Compaction and history eviction change the prefix.
Retained Direct API chat exchanges preserve their full text within the 32 KiB
history budget; oversized history is evicted in blocks. Context and attachment
excerpts, intermediate tool calls, and redacted output are not necessarily
replayed byte-for-byte, so the next turn may miss the provider cache.

## Verified interface sources

- [OMP RPC reference](https://github.com/can1357/oh-my-pi/blob/main/docs/rpc.md): `ready`, v2 negotiation/chunk frames, command IDs, `get_state`, `new_session`, `prompt`, `prompt_result`, `abort`, `get_last_assistant_text`, async deltas. This is **not JSON-RPC 2.0**.
- [Claude Agent SDK TypeScript reference](https://code.claude.com/docs/en/agent-sdk/typescript): `query`, options, hooks, session resume, tool permissions and HTTP MCP configuration. This is the Agent SDK running Claude Code, **not** the generic Anthropic model API.

Exact commands, frame fields, version limitations, safety controls and native state placement: [stdio adapter evidence](adapters-stdio.md) and [SDK/HTTP adapter evidence](adapters-sdk.md). MCP manual registration examples: [CLI integration guide](cli.md#manual-harness-mcp-registration). No user harness configuration is changed automatically.

## Installation

OMP must be on the daemon PATH. Install and authenticate it through its native procedure; OMP can itself use OpenCode, OpenRouter and Codex subscriptions. A Direct API connection needs no CLI: save its endpoint and key under Settings → Connections.

Direct API profile turns separate stable instructions from the user task, enforce a
configurable `maxToolCalls` (default 40), detect repeated calls, parallelize
read-only tools and compact older tool context at 75% of the configured context
window. Streaming falls back only when the endpoint explicitly rejects it.
New Direct API tools include turn-local todos, profile-scoped session search,
reviewable skill proposals, sandboxed background processes, and approved
read-only Python with nested calls through the same broker. See
[the execute-code threat model](design/execute-code.md). Commands and code
require human approval; changes to the original workspace still require a
separate changeset approval.

```sh
npm ci --prefix bridges/claude-agent-sdk --ignore-scripts
```

Claude's optional platform package includes its native CLI. Override relocated executable discovery only with `MESHRA_CLAUDE_BRIDGE`; do not add keys to database configuration.

The current [credential-presence inspection](V0.2_ADAPTER_VERIFICATION.md#credential-presence-not-credential-disclosure) found OMP's default native database, but no checked provider API-key environment variables, Claude login, or OpenCode native auth file. Storage presence is not proof of valid credentials or model access. No credential values were printed.

## Real smoke procedures

All commands below may contact model providers and incur charges. Authenticate natively and launch from the intended environment first. See each adapter's detailed procedure for model/provider overrides.

```sh
MESHRA_REAL_OMP=1 go test -v ./internal/adapters/omp -run '^TestRealSmoke$' -count=1
MESHRA_AUTH_SMOKE=1 go test -v ./internal/adapters/claude -run '^TestAuthenticatedSmoke$' -count=1
```

A successful prompt/resume smoke establishes only that exercised native path. It does not prove real destructive-tool cancellation, every approval variant, every provider, or cross-harness MCP. For actual routed flows, run the named-instance commands in the README and inspect immutable origin/session/correlation fields and persisted results.

## Limits

- OMP relay-owned runtimes do not advertise MCP. They can be destinations and CLI-originated sources; use documented manually registered MCP in a separately launched harness when needed. Claude can automatically call the relay during owned turns.
- No adapter silently chooses “the latest session” when resume fails. Replacing a missing native session requires an intentional new identity rather than silently changing reply ownership.
- The MVP has no interactive approval console. Denied tools are visible; they are not pending approvals that can later be resumed.
- Claude starts owned processes per turn to avoid stale credentials/heavy stopped runtimes. Native session history survives outside SQLite; protect those stores too.
- Harnesses are separate memory/CPU/cost consumers. The idle relay measurement excludes them.
- All adapters run harnesses in full-access mode: native file edits, shell commands and network run with the operator's OS permissions and no approval prompt. Nothing is sandboxed or undoable. Only tool-free profile drafting keeps its empty tool roster. The [v0.2 enforcement matrix](V0.2_ADAPTER_VERIFICATION.md#enforcement-matrix-worktreewrites-false) describes the earlier read-only policy.
