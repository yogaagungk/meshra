# Meshra

A local-first relay for independently named AI coding harness instances. `claude:exampleone` and `claude:exampletwo` are separate durable identities, not interchangeable aliases for a model. Routing uses exact canonical IDs and persists replies against their original identity/session.

The daemon includes a SQLite WAL registry and inbox, serialized agent/repository execution, authenticated HTTP/JSON and task SSE, seven MCP tools, lifecycle supervision, and adapters for OMP, Codex App Server, Claude Agent SDK, and OpenCode HTTP. No Redis, cloud service, or paid account is required for deterministic tests.

## Prerequisites and build

- Ubuntu/WSL2 or Linux, **Go 1.25+**. The Go core uses pure-Go SQLite; a C compiler is not required for a normal build.
- **Node 20+** and npm for the optional Claude SDK bridge. Native harness requirements still apply.
- OMP **18.12.1** and Codex CLI **0.163.0** are the inspected stdio versions. OpenCode adapter pins **1.19.3**. Claude SDK pins **0.3.303**, bundling Claude Code **2.1.303**.
- Native harness authentication or provider environment credentials for real model turns. Do not put API keys in relay configuration.

```sh
go mod download
go build -o meshra ./cmd/meshra
./meshra --help

# Optional SDK/native dependencies, local to this checkout:
npm ci --prefix bridges/claude-agent-sdk --ignore-scripts
npm ci --prefix bridges/claude-agent-sdk/opencode
```

Keep the bridge/dependencies with the executable when packaging; relocation environment variables and exact native compatibility evidence are in [adapters.md](docs/adapters.md). Installation does not establish provider authentication.

## Run

```sh
./meshra serve
# A separate terminal:
./meshra agent list --json
```

Default listener: `127.0.0.1:7331`. Default state: `$XDG_CONFIG_HOME/meshra` or `~/.config/meshra`. The daemon creates a private admin token and takes an exclusive data-directory lock. An explicit configuration example is [config.example.json](config.example.json). No harness configuration is modified automatically.

For an isolated run, pass matching options to server and client:

```sh
./meshra --data-dir /tmp/meshra-demo --addr 127.0.0.1:7441 serve
./meshra --data-dir /tmp/meshra-demo --addr 127.0.0.1:7441 agent list --json
```

## Two Claude identities and one OMP identity

Create actual directories first; these example paths are not assumed to exist:

```sh
mkdir -p /tmp/meshra-work/prod /tmp/meshra-work/wallet /tmp/meshra-work/ads
./meshra agent create --harness claude --name exampleone --cwd /tmp/meshra-work/prod \
  --allow-to omp:exampleworker --allow-operation list,status,send,request,events,cancel
./meshra agent create --harness claude --name exampletwo --cwd /tmp/meshra-work/wallet
./meshra agent create --harness omp --name exampleworker --cwd /tmp/meshra-work/ads \
  --allow-to claude:exampleone,claude:exampletwo \
  --allow-operation list,status,send,request,events,cancel
./meshra agent start omp:exampleworker
./meshra agent start claude:exampleone
./meshra agent start claude:exampletwo
./meshra agent list --json

./meshra request --from omp:exampleworker --to claude:exampleone \
  --message 'Reply with PROD_REVIEW_OK. Do not use tools.' \
  --idempotency-key prod-review-001 --json
# Copy taskId from the admission response:
./meshra task events TASK_ID --follow --json
./meshra task status TASK_ID --json
./meshra agent status claude:exampletwo --json
```

The task result belongs to the recorded OMP native session. Wallet receives no turn. Claude allocates its native session on its first SDK turn; an identity that has not received a turn can therefore have no native session yet. Request completion is observed by status/events, not inferred from send success. Independent requests have separate task/correlation IDs.

A reverse request is explicit and separate:

```sh
./meshra request --from claude:exampleone --to omp:exampleworker \
  --message 'Reply with OMP_REPLY_OK. Do not use tools.' --json
./meshra agent stop claude:exampleone
./meshra agent restart omp:exampleworker
```

The same workflow can use Codex/OpenCode destinations after registration:

```sh
mkdir -p /tmp/meshra-work/reviewer /tmp/meshra-work/testengine
./meshra agent create --harness codex --name reviewer --cwd /tmp/meshra-work/reviewer
./meshra agent create --harness opencode --name testengine --cwd /tmp/meshra-work/testengine
./meshra agent start codex:reviewer
./meshra agent start opencode:testengine
```

These are real-harness commands and may incur provider costs. Credential-free identity/routing scenarios are exercised by the automated local end-to-end tests.

## Agent-facing tools

The authenticated MCP endpoint is `http://127.0.0.1:7331/mcp`. Tools:

- `meshra_agents_list`, `meshra_agent_status`
- `meshra_send`, `meshra_request` (asynchronous, returns a task)
- `meshra_task_status`, `meshra_task_events`, `meshra_task_cancel`

Policies are deny-by-default. Agents need permitted operations and exact allowed destinations; delegation additionally requires `--delegate`. A JSON `from` field is never authentication. Claude/OpenCode adapters provide per-turn identity/session/task-bound MCP credentials. OMP/Codex automatic MCP wiring is not claimed; documented manual registration is available in [cli.md](docs/cli.md#manual-harness-mcp-registration). The optional `meshra mcp stdio` proxy refuses admin credentials.

## Reliability and recovery

- One active turn per identity. Busy/offline destinations retain persistent inboxes until start or deadline.
- All Meshra turns sharing a Git repository root serialize. Use distinct repositories/worktrees for independent work; external programs are not covered by this advisory lock.
- Idempotency keys deduplicate admission. **No exactly-once execution claim.**
- Shutdown/restart preserves queued work. Interrupted executions become explicit uncertain failures, never automatic replays. Start agents again, inspect external effects, and deliberately retry when safe.
- Task deadlines include queue time. `meshra task cancel TASK_ID` requests termination, not rollback.
- Harness failure or interruption puts that agent in `error`, preserving other queued tasks. Inspect the error, `agent restart`, then explicitly retry the failed/interrupted task if appropriate. This also avoids reusing stdio runtimes closed during cancellation.
- Replies remain persisted if their origin stops. They are never silently injected into another active session.

See [protocol.md](docs/protocol.md) for state transitions, queue limits, events, correlation, ancestry, session ownership and retry semantics; [security.md](docs/security.md) for authentication and safety.

## Tests and checks

```sh
# Deterministic unit, protocol-contract, API/MCP and local end-to-end tests:
go test -race ./...
# Static checks:
go vet ./...
# Formatting check (no output means formatted):
gofmt -l cmd internal tests
# Build:
go build -o meshra ./cmd/meshra
# Credential-free smoke of the real CLI/daemon (requires Node):
node scripts/smoke.mjs
```

Acceptance coverage includes two same-harness identities, cross-harness request/reply ownership, nested delegation/correlation, queue order, conflicting repository locks, concurrent requests, queued restart, abruptly killed daemon recovery, idempotent admission, bounded inboxes, unavailable agents, authentication/impersonation failures, timeouts/cancellation, and persistent SSE replay. Protocol contracts use fake processes/HTTP transports, not fabricated provider success.

The local smoke starts a private temporary daemon, registers all four harness types with two Claude identities, queues/deduplicates a message, restarts the daemon, verifies durable state and authentication, and cancels the queued task. It never starts a provider turn and removes its temporary state.

Optional **paid/native** prompt-and-resume checks:

```sh
MESHRA_REAL_OMP=1 go test -v ./internal/adapters/omp -run '^TestRealSmoke$' -count=1
MESHRA_REAL_CODEX=1 go test -v ./internal/adapters/codex -run '^TestRealSmoke$' -count=1
MESHRA_AUTH_SMOKE=1 go test -v ./internal/adapters/claude -run '^TestAuthenticatedSmoke$' -count=1
MESHRA_AUTH_SMOKE=1 go test -v ./internal/adapters/opencode -run '^TestAuthenticatedSmoke$' -count=1
```

Optional model/provider overrides are documented in [adapters.md](docs/adapters.md). These checks validate only the exercised native prompt/session paths, not every possible tool permission or native MCP path. Actual executed results are recorded in [verification.md](docs/verification.md).

## Troubleshooting on WSL2

- **Connection refused:** run `meshra serve`, and use the same `--addr` for the client. Both processes need access to the same private state directory unless the client uses its own `MESHRA_TOKEN`.
- **401/403:** remove an unintended `MESHRA_TOKEN` from the human shell, or grant only the intended agent operations/destinations. Session-bound credentials cannot read unrelated session history.
- **Already owned:** another daemon holds that state directory; use it or stop it cleanly. A dead process releases the OS lock; do not delete SQLite/WAL files to force recovery.
- **Harness not found / model unavailable:** install the documented version, authenticate through native tooling, and launch the daemon from that environment. Custom Codex provider definitions are not inherited from user config by the isolated adapter.
- **Queued forever:** inspect agent state and deadline. Start/restart the exact destination. A running conflicting repository turn keeps the shared lock; nested requests must not wait indefinitely while holding it.
- **Approval required:** the MVP rejects unsafe tool actions visibly. There is no approval console and no automatic sandbox escape. Do not enable bypass approval to fix a smoke test.
- **After WSL shutdown:** restart the daemon and agents; queued tasks remain. Inspect uncertain failed turns before retrying.
- Keep state on the WSL Linux filesystem, not a shared Windows mount with different locking/permission semantics. Idle relay and spawned harness footprints are separate; see measured resource evidence.

## Boundaries and next steps

No arbitrary interactive-session attach, autonomous planning, cloud/remote relay, model-based routing, web UI, or identical tool capabilities across harnesses. Current adapters are read-oriented; OpenCode text is final-only. Native harness sessions live outside SQLite. No interactive approval UI, automatic history retention, remote security boundary, or coordination with external writers. MCP delegation is automatic only where verified adapter configuration supports it. Future work: authenticated integration coverage on more provider accounts, an explicit approval workflow, and worktree/retention management—not undocumented remote-control shortcuts.

Design and evidence: [architecture](docs/architecture.md), [protocol](docs/protocol.md), [adapters](docs/adapters.md), [security](docs/security.md), [CLI/API/MCP](docs/cli.md).
