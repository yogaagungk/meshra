# Architecture

Meshra uses Go for the daemon, CLI, authenticated HTTP/JSON API, task SSE, and MCP. SQLite WAL stores agents, messages/tasks and append-only events. A single daemon owns the database using an OS lock. No broker or network database is required. The Claude Agent SDK runs in a small pinned Node sidecar; other adapters use owned stdio processes or a localhost HTTP server.

The scheduler is event-driven: durable admission, lifecycle changes, completion and cancellation wake it; a timer handles the next persisted task deadline. It does not repeatedly scan offline inboxes. A partial SQLite index excludes terminal history from scheduling queries. Minimum Go version is 1.25, required by pinned pure-Go SQLite 1.46.2.

## Boundaries

- `internal/model`: domain records, adapter and application service contracts.
- `internal/relay`: SQLite migrations, registry, authorization, durable inbox, scheduler, lifecycle supervision, task ownership and recovery.
- `internal/api`: HTTP, SSE and stateless HTTP MCP, sharing the authenticated service boundary.
- `internal/adapters`: protocol-specific session owners. An adapter never routes messages itself.
- `cmd/meshra`: human CLI, daemon startup and MCP stdio proxy.
- `bridges/claude-agent-sdk`: Claude SDK JSONL bridge.

Canonical identities are lowercase `<harness>:<name>`; names match `[a-z][a-z0-9_-]{0,62}`. Sessions are relay-owned, never guessed from the latest interactive terminal. The MVP supports one active turn per identity. Canonical repository roots are locked for all turns, conservatively serializing possible writes. Separate identities may share a harness but never a native session.

Queued work survives restart. A task whose execution was interrupted is failed with an explicit uncertain-execution error, not blindly replayed. Retrying is a human decision and can repeat external effects. Idempotency keys deduplicate admission, not harness execution. Replies are persisted against immutable origin/session and correlation metadata; they are never injected into whichever session happens to be active.

Nested delegation is asynchronous: `meshra_request` returns a task immediately. Polling/status and events do not acquire a turn slot. Ancestry, hop limits and destination/operation permissions are checked by the relay. Agent credentials are not accepted as a `from` assertion.

## Initial compatibility evidence

Environment inspected: Go 1.27.2, Node 24.22.0, npm 11.19.1, OMP 18.12.1, Codex CLI 0.163.0 on Linux/WSL2. Claude and OpenCode were not initially on PATH. OMP documents a custom JSONL RPC with ready/response/events (not JSON-RPC). Installed Codex exposes App Server and version-specific JSON schema generation. Official documentation was consulted before adapter implementation. Detailed protocol evidence, limitations and smoke outcomes belong in [adapters.md](adapters.md).

No user harness configuration is modified. The local service binds only to 127.0.0.1 and uses bearer credentials protected by filesystem permissions. Model/provider configuration contains no secrets; adapters inherit harness-native authentication.
