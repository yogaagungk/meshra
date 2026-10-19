# Architecture

Meshra uses Go for the daemon, CLI, authenticated HTTP/JSON API, task SSE, and MCP. SQLite WAL stores agents, messages/tasks and append-only events. A single daemon owns the database using an OS lock. No broker or network database is required. The Claude Agent SDK runs in a small pinned Node sidecar; other adapters use owned stdio processes or a localhost HTTP server.

The scheduler is event-driven: durable admission, lifecycle changes, completion and cancellation wake it; timers handle persisted task deadlines and bounded offline retry schedules. It does not poll between those events. Partial SQLite indexes exclude terminal history from scheduling and inbox counts. Versioned transactional migrations preserve v0.1 IDs, event sequence and idempotency while adding operation scoping; newer unsupported schemas are refused. Minimum Go version is 1.25, required by pinned pure-Go SQLite 1.46.2.

## Boundaries

- `internal/model`: domain records, adapter and application service contracts.
- `internal/relay`: SQLite migrations, registry, authorization, durable inbox, scheduler, lifecycle supervision, task ownership and recovery.
- `internal/api`: HTTP, SSE and stateless HTTP MCP, sharing the authenticated service boundary.
- `internal/adapters`: protocol-specific session owners. An adapter never routes messages itself.
- `cmd/meshra`: human CLI, daemon startup and MCP stdio proxy.
- `bridges/claude-agent-sdk`: Claude SDK JSONL bridge.

Canonical identities are lowercase `<harness>:<name>`; names match `[a-z][a-z0-9_-]{0,62}`. Sessions are relay-owned, never guessed from the latest interactive terminal. One active turn per identity and at most 32 globally. Canonical repository roots are locked for all turns, conservatively serializing possible writes. Separate identities may share a harness but never a native session. Registered definitions persist independently of ephemeral runtime handles; restart reports previously running agents as stopped until explicit native startup/resume.

Queued work and retry eligibility survive restart. A task whose execution was interrupted is failed with an explicit `uncertain_execution` category and ambiguity flag, not blindly replayed. Retrying is a human decision and can repeat external effects. Idempotency keys deduplicate admission within sender/session/operation and retained history, not harness execution. Replies are persisted against immutable origin/session and correlation metadata; they are never injected into whichever session happens to be active. Text deltas are accumulated in bounded memory and redacted before successful-output persistence; session/progress/approval events remain immediate.

Nested delegation is asynchronous: `meshra_request` returns a task immediately. Polling/status and events do not acquire a turn slot. Ancestry, hop limits and destination/operation permissions are checked by the relay. Agent credentials are not accepted as a `from` assertion.

## Initial compatibility evidence

Environment inspected: Go 1.27.2, Node 24.22.0, npm 11.19.1, OMP 18.12.1, Codex CLI 0.163.0 on Linux/WSL2. Claude and OpenCode were not initially on PATH. OMP documents a custom JSONL RPC with ready/response/events (not JSON-RPC). Installed Codex exposes App Server and version-specific JSON schema generation. Official documentation was consulted before adapter implementation. Detailed protocol evidence, limitations and smoke outcomes belong in [adapters.md](adapters.md).

No user harness configuration is modified. The local service binds only to loopback (default 127.0.0.1) and uses bearer credentials protected by filesystem permissions. Model/provider configuration contains no secrets; adapters inherit harness-native authentication.

The relay exposes payload-free structured lifecycle logs, retained-history metrics, bounded traces, executable/workspace diagnostics, persisted SSE cursors, outbound and optional inbound permissions, and configurable terminal-history pruning. The shared service remains the authorization boundary for HTTP and MCP, including audited impersonation rejection. See [v0.2 design](V0.2_DESIGN.md), [operations](V0.2_OPERATIONS.md), and [test evidence](V0.2_TEST_REPORT.md).
