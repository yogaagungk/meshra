Meshra — MVP Implementation Prompt
You are a senior staff-level software engineer and systems architect. Build Meshra, a lightweight, local-first communication relay for multiple instances of AI coding harnesses. The initial target environment is Ubuntu on WSL2 (Windows 11), with an approximately 6 GB WSL memory limit. The implementation must be runnable locally and designed for future portability to macOS/Linux.
Your objective is to deliver a working MVP, not just architecture diagrams or scaffolding. Make sensible engineering decisions, document assumptions, implement incrementally, and verify functionality with automated tests and realistic end-to-end flows.
1. Product vision and boundaries
Meshra is a peer-to-peer logical communication fabric with a centralized local transport/router. It connects named, independently addressable instances of:
OMP (Oh My Pi)
Claude Code
Codex CLI
OpenCode
An agent instance is a named identity backed by a specific harness runtime and session, not a model name or a generic role. Multiple independent instances of the same harness are mandatory in the MVP.
Examples:
`claude:exampleone`
`claude:exampletwo`
`omp:exampleworker`
`codex:reviewer`
`opencode:testengine`
A message addressed to `claude:exampleone` must never be delivered to `claude:exampletwo`. Agent identities must remain stable across process restarts; runtime process IDs and native harness session IDs may change.
Non-goals for MVP: autonomous task planning, model-based destination selection, cloud-hosted multi-node deployment, sophisticated web UI, Kafka/Redis/PostgreSQL, cross-device remote access, arbitrary access to someone else's pre-existing interactive terminal session, or a universal promise of identical capabilities across all four harnesses.
2. MVP deliverables (all required)
2.1 Multi-instance agent registry
Persist agent definitions in SQLite. Each agent must have:
Stable canonical ID: `<harness>:<name>` (case rules and validation documented)
Harness type: `omp | claude | codex | opencode`
Display name and configured working directory
Runtime configuration / optional provider and model settings (without persisting secrets)
Native session mapping (nullable until allocated)
Lifecycle state: `registered | starting | idle | busy | stopped | error`
Last-seen time and useful error information
Capability metadata (`prompt`, `stream`, `resume`, `cancel`, etc.) describing actual adapter support
Authorization policy and concurrency limit (MVP default: one active turn per agent)
Support registering multiple instances of the same harness, listing, inspecting, starting, stopping, removing, and restarting them. Reject duplicate canonical IDs and invalid/nonexistent working directories with understandable errors.
2.2 Cross-agent messaging
Provide reliable local routing by canonical agent ID:
Fire-and-track asynchronous send, yielding `messageId` and `taskId`
Request/reply with correlation IDs, timeouts, status lookup and event streaming
Persistent inbox per destination; busy destinations must queue messages rather than silently dropping them
Delivery acknowledgment/state tracking and recovery following relay restart
Event history, message state transitions, bounded payload sizes, and actionable errors
Replies associated with the originating agent identity and originating session, not merely the last active process
Support for one agent sending multiple requests to different destinations without cross-talk
Delivery policy: at-least-once or explicitly documented retry semantics, with deduplication/idempotency keys; never claim exactly-once execution of a non-idempotent harness turn
For the MVP, use a single-writer/serialized per-agent turn execution model. Avoid deadlocks during agent-to-agent delegation: `relay.request` must not block the destination's own ability to receive and process its replies. Prefer asynchronous tasks plus polling/events for nested delegation.
2.3 Four harness adapters
Build adapters for all four harness types, with a clearly documented support matrix and real integration smoke tests whenever their CLIs/authentication are available. Do not pretend that all adapters support attaching to arbitrary existing interactive sessions.
OMP: investigate and use its documented `omp --mode rpc` JSONL-over-stdio interface and/or supported SDK. It is not JSON-RPC 2.0. Handle ready frames, commands, responses, asynchronous events, cancellation, and protocol version negotiation where applicable.
Codex: use its documented App Server protocol if supported by the installed version. Model its thread/turn lifecycle, streaming events, and approval requests explicitly. Do not assume undocumented remote-control commands are a local adapter API.
OpenCode: use its documented HTTP server/API or official SDK for session creation, prompting, streaming, and abort. Scope host/port and credentials to localhost.
Claude Code: use the documented Claude Agent SDK (not merely the generic Anthropic model API) to manage relay-owned agent sessions and resume them where supported. A small Node/TypeScript adapter sidecar is acceptable if the core is written in Go.
Before implementing an adapter, inspect the locally installed CLI/version and current upstream documentation. Record verified commands, SDK versions, protocol schemas and limitations. Where environment credentials or permissions prevent real integration testing, provide a clearly flagged unverified adapter, contract tests with a mock transport, and a reproducible manual smoke-test procedure. Do not fabricate integration success.
2.4 Agent-facing MCP interface
Expose a local MCP server so supported harnesses can discover and call Meshra tools:
`meshra_agents_list`
`meshra_agent_status`
`meshra_send`
`meshra_request` (or create-task plus status; avoid unsafe indefinite blocking)
`meshra_task_status`
`meshra_task_events`
`meshra_task_cancel`
Validate the caller identity and its permissions; never trust a caller-provided `from` field without authentication. Each agent must only act as itself unless explicitly authorized. Explain how the MCP server is registered in each harness and automate setup only when the harness configuration format has been verified.
2.5 Human CLI and daemon
Provide a single ergonomic executable, `meshra`, with commands equivalent to:
```bash
meshra serve
meshra agent create --harness claude --name exampleone --cwd "$HOME/projects/exampleone"
meshra agent create --harness claude --name exampletwo --cwd "$HOME/projects/exampletwo"
meshra agent create --harness omp --name exampleworker --cwd "$HOME/projects/exampleworker"
meshra agent list
meshra agent start claude:exampleone
meshra agent status claude:exampleone
meshra send --from omp:exampleworker --to claude:exampleone --message "Review the API contract"
meshra task status <task-id>
meshra task events <task-id> --follow
meshra agent stop claude:exampleone
```
Names and paths are examples, not assumptions about the user's filesystem. The CLI should print machine-readable JSON when `--json` is requested. Provide `--help`, meaningful exit codes, and logs that do not leak secrets.
3. Recommended implementation architecture
Use this as a starting point; change it only with a written rationale:
Go: relay daemon, CLI, lifecycle supervisor, queue, routing, API, SQLite persistence.
SQLite with WAL: agent registry, native session mapping, messages, tasks, state transitions and audit events.
Local HTTP/JSON API + SSE: CLI communication, task status, live events; localhost binding by default.
MCP server: separate endpoint/transport where appropriate, sharing application service logic with the CLI/API.
Adapters: Go implementations for process/JSONL and HTTP protocols where practical; isolated Node/TypeScript bridge for harnesses whose supported SDKs demand it.
Process supervisor: one managed runtime or session owner per agent instance when necessary; gracefully terminate children and reap processes.
Minimal dependencies: no external message broker or database for MVP. Do not keep heavy harness processes resident when stopped.
Proposed logical modules (adapt names to the language conventions):
```text
cmd/meshra/
internal/registry/
internal/router/
internal/queue/
internal/tasks/
internal/store/
internal/supervisor/
internal/adapters/{omp,codex,claude,opencode}/
internal/api/
internal/mcp/
internal/security/
internal/events/
bridges/claude-agent-sdk/   # only if needed
migrations/
tests/
docs/
```
Keep interfaces testable and avoid abstract framework-building before a working vertical slice exists.
4. Domain model and protocol
Use stable internal IDs (UUID/ULID acceptable), monotonic timestamps where needed for duration calculations, UTC timestamps for persistence, and `schemaVersion` for externally persisted/exchanged messages.
Example request (illustrative; adapt and formalize in the codebase):
```json
{
  "schemaVersion": 1,
  "messageId": "01EXAMPLEMSG",
  "taskId": "01EXAMPLETASK",
  "correlationId": "01EXAMPLECORR",
  "type": "request",
  "from": { "agentId": "omp:exampleworker", "sessionId": "source-session-id" },
  "to": { "agentId": "claude:exampleone" },
  "payload": { "text": "Review the API contract" },
  "delivery": {
    "idempotencyKey": "review-api-contract-001",
    "timeoutSeconds": 300,
    "maxHops": 4
  }
}
```
Document message/task state machines (for example: `queued → dispatching → running → completed | failed | cancelled | timed_out`), valid transitions, restart recovery, and the difference between transport ACK and agent task completion.
Maintain explicit `parentTaskId`, `originAgentId`, `originSessionId`, `correlationId`, and hop count for delegated work. Define clear failure handling if an origin agent stops before the reply arrives: persist the reply; make it available on resume/status lookup. Never silently inject into an unrelated session.
5. Safety, permissions, and reliability
The MVP must implement:
Bind local services to `127.0.0.1` by default. Never expose them on the public interface without explicit opt-in and authentication.
Authenticate agents and CLI callers. Enforce per-agent allowed destinations and permitted operations; do not treat a JSON `from` field as proof of identity.
Keep credentials in harness-native auth stores/environment or a properly protected local secret provider; do not put API keys in SQLite logs/events.
Apply request size limits, bounded queues, concurrency limits, timeouts, and cancellation.
Prevent routing loops with hop limit, task ancestry validation, and explicit authorization for delegation.
Protect against accidental concurrent writes to one repository: serialize conflicting write tasks or provide a documented repository lock. Distinct worktrees can be a later enhancement.
Keep permission/approval prompts visible and actionable; never silently auto-approve destructive tools, privileged shell commands, or sandbox escapes.
Treat agent-generated output as untrusted data. Do not interpret arbitrary received text as relay commands.
Handle abrupt process exits, WSL shutdown, relay restarts, lost streams, duplicate delivery, and unavailable agents without silently losing task state.
Implement graceful shutdown and document what state can resume versus what must be retried manually.
6. Test scenarios (mandatory)
Write unit, integration, and local end-to-end tests, including these acceptance scenarios:
Identity isolation: register `claude:exampleone` and `claude:exampletwo`, send to one, verify the other receives nothing.
Cross-harness routing: `omp:exampleworker → claude:exampleone`, reply routes to the original OMP session.
Chained communication: `OMP → Claude → Codex → OMP`, with stable correlation, hop counts, and correct reply ownership.
All four adapters: mock/contract coverage and authenticated real smoke tests when available for OMP, Claude, Codex, and OpenCode.
Busy queue: while one agent runs a turn, a second message is queued and processed in order.
Multi-request concurrency: one origin sends to two distinct agents and responses cannot get crossed.
Restart: restart Meshra while messages are queued; verify persistent state, safe recovery, and no unbounded duplicate dispatch.
Failure handling: nonexistent destination, offline destination, auth denied, timeout, failed child process, cancellation, invalid payload, and oversized message.
Delegation safety: reject cycles or hop-limit violations, and prevent unauthorized impersonation.
Resource budget: measure idle daemon memory/CPU and process count inside WSL; note that spawned LLM harnesses have separate resource and cost footprints.
Real end-to-end tests may require valid harness authentication. Separate these explicitly from deterministic tests that can run in CI without credentials.
7. Milestones and working order
Implement in small, demonstrable milestones. Multi-instance must work from the first vertical slice, not be retrofitted after the first adapter.
M0 — Verify environment and interfaces: inspect WSL, tool versions, Git status and SDK documentation; produce a brief compatibility matrix and implementation plan. Do not alter unrelated user configuration.
M1 — Core + multiple named agents: Go CLI/daemon, SQLite migration, registration of two same-harness instances, identity isolation, local API, status and structured logs, tests.
M2 — Messaging engine: durable queue, task/correlation tracking, per-agent serialization, timeout/cancel/retry, event stream, restart tests; fake adapter for deterministic CI.
M3 — Real OMP and Codex: adapters based on verified protocols; demonstrate two separate named instances and OMP ↔ Codex request/reply where credentials permit.
M4 — Claude and OpenCode: add verified session-owning adapters; demonstrate two independent Claude identities (`exampleone`, `exampletwo`) and cross-harness routing with all four types registered.
M5 — MCP interface + hardening: caller identity validation, agent permissions, docs for harness integrations, loop prevention, safe approvals, repo write coordination, CLI polish and local smoke-test scripts.
8. Deliverable quality gate
Do not conclude with 'the architecture is ready' if the software is not runnable. Deliver:
Buildable source tree and dependency manifests, pinned appropriately
Database migrations and configuration example without secrets
`README.md` with prerequisites, install, `meshra serve`, CLI examples, and troubleshooting for Ubuntu WSL2
`docs/architecture.md`, `docs/protocol.md`, `docs/adapters.md`, and `docs/security.md`
Unit/integration tests runnable without paid accounts; optional authenticated smoke tests
Clear commands to build, run, test and lint
Explicit capability/support matrix for each harness, distinguishing verified and unverified behavior
Example showing two Claude instances and one OMP instance communicating without session cross-talk
A concise list of known limitations and next steps
9. Work style and communication
First inspect the actual repository (if present). Preserve existing code, changes, and conventions.
Research or inspect the current official protocol docs and installed CLI versions before choosing exact adapter methods; do not make up SDK method names or command flags.
Present a short execution plan, then implement instead of waiting for approval on routine choices.
Ship vertical slices with tests and concrete examples; prefer boring, reliable code over unnecessary abstraction.
Record meaningful assumptions and compatibility decisions in docs.
Never assert a real harness integration passed unless it actually ran and returned verifiable results.
On partial completion, accurately report what runs, what is mocked, what is blocked, and how to reproduce the results.
10. Starting instructions
Begin now:
Inspect the local environment, existing repository, tool versions and each supported harness interface.
State your chosen concrete tech stack and module boundaries in a short `docs/architecture.md` draft.
Initialize the Meshra repository and implement M1, including SQLite migrations, a runnable CLI/daemon, and tests for multiple named Claude identities.
Continue through the milestones as far as the available tools, credentials and environment allow.
Provide commands demonstrating the end-to-end workflow and a factual completion report.
Primary success criterion: named instances such as `claude:exampleone`, `claude:exampletwo`, and `omp:exampleworker` are distinct, durable identities, and a message intended for one instance is never routed to another. The eventual full MVP additionally supports OMP, Claude, Codex, and OpenCode communicating through the same secure, observable relay.
Official integration references to verify during implementation
OMP RPC: https://github.com/can1357/oh-my-pi/blob/main/docs/rpc.md
Codex App Server: https://developers.openai.com/codex/app-server/
Claude Agent SDK: https://platform.claude.com/docs/en/agent-sdk/overview
OpenCode Server: https://docs.opencode.ai/docs/server/
OpenCode SDK: https://docs.opencode.ai/docs/sdk/
These are starting references, not a replacement for checking current versions and actual behavior.
