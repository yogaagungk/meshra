# Meshra v0.2 — Reliable Agent Communication

## Role
You are a principal software engineer implementing **Meshra v0.2** in the **existing repository**. Meshra is a peer-oriented, multi-instance, multi-harness communication layer for **OMP, Claude Code, Codex, and OpenCode**. The v0.1 MVP has been validated by the project owner. **Extend the working implementation; do not rebuild it from scratch.**

## Mission
Make agent-to-agent communication **durable, observable, recoverable, and controlled**, including when a single harness hosts many named agents such as `claude:exampleone`, `claude:exampletwo`, and `omp:exampleworker`.

The primary use case is local Ubuntu on WSL2, with a resource limit of approximately 6 GB RAM. Minimize background processes and dependencies.

## Non-negotiable constraints
1. **Preserve v0.1 behavior and APIs** unless a breaking change is unavoidable. For necessary changes, provide explicit migration and compatibility tests.
2. **Multi-instance isolation is foundational:** never confuse similarly named agents, runtime handles, inboxes, sessions, working directories, or responses.
3. Keep all four adapters working. **Verify actual API capabilities against installed CLI versions and official documentation** before changing adapter protocols. Do not fabricate support for attaching to unmanaged interactive sessions.
4. Do not add Kafka, Redis, RabbitMQ, or PostgreSQL merely for v0.2. Prefer the current Go + SQLite + HTTP/SSE/MCP architecture if it is already implemented; adapt to the repository's actual stack.
5. Avoid arbitrary shell execution or autonomous escalation through relay messages. Preserve user approval boundaries and enforce per-agent permissions.
6. Do not claim exactly-once execution: design for **at-least-once delivery with idempotent processing where feasible**, explicitly documenting limits when downstream harness execution cannot be deduplicated.
7. Never silently resend a potentially executed non-idempotent request after uncertain completion. Surface ambiguous outcomes for reconciliation.
8. Persist essential state, but do not persist secrets or raw credentials in event payloads/logs. Apply redaction and configurable retention.
9. Prioritize actual implementation with tests, not architecture-only documents or mocked happy paths.

## Start here — repository reconnaissance
- Inspect current architecture, migrations, CLI, API, schema, adapters, tests, and documented v0.1 contracts.
- Run the existing test suite and record baseline results.
- Identify what is already implemented versus the requirements below; **do not duplicate existing functionality**.
- Document assumptions and constraints in `docs/V0.2_DESIGN.md` before making material schema/protocol changes, then proceed with implementation in the same task.

## P0 — Required for v0.2 release

### A. Durable message lifecycle and outbox/inbox
Implement a persistent message state machine, consistent with existing APIs:

`queued -> claimed -> delivered -> running -> completed | failed | cancelled | timed_out`

Treat these as logical states; adapt terminology where required. Define allowed transitions and distinguish **transport delivery** from **downstream execution completion**.

- Persist message ID, sender and recipient agent IDs, recipient session selection, conversation/correlation ID, parent request ID, idempotency key, attempt number, creation/update timestamps, deadline, status, error category, and minimal payload or artifact reference.
- Use transactional persistence for enqueue + routing decisions where possible.
- Implement per-agent persistent inbox, safe claim/lease, acknowledgment, retry scheduling, max-attempt policy, and bounded exponential backoff with jitter.
- On restart, recover pending items and reconcile stale leases. Do not duplicate a task merely because the relay restarted while an adapter operation was in flight.
- Ensure messages to `claude:exampleone` never route to `claude:exampletwo`, even during concurrent requests and restarts.
- Bound queue depth / payload size to prevent resource exhaustion; define behavior for overload.
- SQLite migrations must be forward-safe and preserve existing v0.1 data. Use WAL and appropriate busy timeouts if SQLite is in use.

### B. Request/reply correlation and concurrency
- Every request gets a stable correlation ID and every response points to its originating request.
- Correlate events using agent ID **and** session/runtime identity, not solely a harness name.
- Enforce **one active mutating turn per session** by default; queue later turns or reject with a clear conflict response.
- Parallelism across independent agent instances must continue working.
- Implement cancellation and deadlines, including accurate terminal state even if downstream cancellation is best-effort.
- Make duplicate submissions deterministic using an idempotency key scoped to the sender and operation. Specify its retention window.

### C. Restart and reconnect recovery
- Persist agent definitions separately from ephemeral process or connection handles.
- Implement well-defined status values (e.g. `offline`, `starting`, `idle`, `busy`, `unhealthy`) and the distinction between **registered** and **running**.
- On relay restart, restore registry and pending messages; probe runtime availability before claiming an agent has resumed.
- Resume existing harness sessions only when adapter support is verified and the session is still valid; otherwise mark as requiring a fresh session/explicit reattachment.
- Handle process exit, disconnect, crash, timeout, and WSL shutdown cleanly. Provide clear recovery feedback.

### D. Observability and diagnosability
- Structured logs with request ID, correlation ID, sender, recipient, harness, session mapping, state transition, attempt number, latency, and error class; redact credentials and sensitive content.
- Provide a **message trace** command or endpoint to follow a message through its lifecycle.
- Expose metrics or equivalent lightweight counters for queued, in-flight, succeeded, failed, retried, expired, and offline agents; include queue age/latency.
- Provide operator commands consistent with the existing CLI, such as `meshra agent list`, `meshra message inspect <id>`, `meshra message retry <id>`, and `meshra doctor`. These names are examples: preserve the repository's CLI conventions.
- SSE event subscribers should be able to reconnect using persisted event sequence/cursor when feasible. Document retention and gaps.

### E. Permissions and safe defaults
- Define configurable permissions per agent for who can send to it, permissible operations, and whether worktree writes are allowed.
- Default the service to loopback-only binding. Require explicit opt-in and authentication before remote exposure.
- Validate identifiers, project working directories, message payloads, and all user-controllable paths. Avoid command injection.
- Explicitly label and restrict any adapter action that needs user approval. Relaying a request does **not** grant approval.
- Prevent runaway ping-pong delegation using hop count / ancestry checks, deadline propagation, and conservative rate/concurrency limits.

## P1 — Include if P0 is complete and tested
- Lightweight heartbeat/health checks with configurable thresholds and stale agent detection.
- Operator-configurable retries and dead-letter handling with manual redrive; show when outcomes are ambiguous.
- Optional Git worktree isolation for parallel write tasks, without automatically altering existing project directories.
- Configuration diagnostics: malformed settings, unreachable executables, unsupported adapter version, missing credentials (without exposing secrets), and incorrect workspace permissions.

## Explicitly out of scope for v0.2
- Autonomous workflow planning, task decomposition, AI-based agent selection, consensus discussions, and shared group conversations.
- Distributed multi-host orchestration and public SaaS hosting.
- A full graphical dashboard.
- Replacing existing harness CLIs or requiring all agents to run under a new proprietary runtime.

## Contract and API expectations
Extend the existing message contract rather than introducing an incompatible parallel protocol. At minimum support:
- `agent.register`, `agent.list`, `agent.status` (or equivalent existing operations)
- asynchronous `message.send` returning `messageId`
- `message.get` / `message.trace` to inspect state and events
- `message.cancel`
- synchronous request/reply with an explicit deadline and clear timeout semantics
- subscriber event stream with correlation metadata

If the project has MCP tools, expose these capabilities through compatible tools and preserve existing names wherever practical. Document all request, reply, and failure shapes with examples.

## Required end-to-end test scenarios
Use real adapter integrations where credentials/environments are available. For unavailable providers, use protocol-faithful local test servers and identify those results as simulated, **not live integration verified**.

1. Register two Claude instances (`claude:exampleone`, `claude:exampletwo`) and one OMP instance (`omp:exampleworker`). Concurrent messages must route to the exact target.
2. OMP sends to Codex; Codex replies; original OMP requester receives the correct correlated reply.
3. Concurrent requests to **different instances** progress independently. Concurrent mutating requests to **one session** are serialized or safely rejected.
4. Restart Meshra with queued messages; pending work remains visible and is processed or reconciled appropriately.
5. Kill an adapter **after external execution may have begun**; ensure uncertain tasks are not blindly repeated.
6. Deliberately deliver the same message twice; ensure idempotency/reconciliation prevents duplicate side effects where controllable.
7. Simulate an offline destination; observe retries, backoff, deadline, and final failure/dead-letter state.
8. Verify timeout and cancellation behaviors, including downstream operations that cannot be forcibly cancelled.
9. Verify unauthorized sender/operation is rejected and audited.
10. Verify agent separation after a full relay restart and mapping/session recovery.
11. Confirm memory/CPU remain reasonable under the 6 GB WSL limit; use a documented, repeatable lightweight load test, not fabricated benchmarks.
12. Run regression tests for all existing v0.1 APIs, CLI commands, and all four adapters.

## Implementation order
1. **Baseline:** inspect repo, identify existing features, run tests; record baseline and migration concerns.
2. **Design:** message lifecycle, SQLite migrations, retry/idempotency model, session ownership, reconciliation behavior.
3. **P0 core:** durable storage, session-safe routing, claim/lease, correlation, retry and cancellation semantics.
4. **Recovery:** restart reconnection and ambiguous-execution safeguards.
5. **Observability/security:** tracing, metrics, diagnostics, permissions, limits, redaction.
6. **Adapter verification:** check OMP, Claude, Codex, and OpenCode individually; run live E2E where feasible.
7. **Testing and release:** concurrency, crash-injection, migration, regressions, docs, and release notes.

## Deliverables
- Working code committed to the existing project structure (do not initialize a replacement project).
- Tested schema migrations and database rollback/recovery guidance.
- Updated CLI/API/MCP contracts with examples.
- `docs/V0.2_DESIGN.md` (decisions and non-goals).
- `docs/V0.2_OPERATIONS.md` (start, stop, inspect, retry, recovery, logs, WSL-specific constraints).
- `docs/V0.2_TEST_REPORT.md` (exact commands, observed results, distinction between live and simulated integration tests, known limitations).
- `CHANGELOG.md` v0.2 entry, and any updated quickstart instructions.

## Definition of done
v0.2 is complete only when:
- Multiple named instances of the same harness stay isolated under concurrency and restart.
- Persisted pending messages survive relay restarts.
- Retries and uncertain execution are explicitly and safely handled; no undocumented exactly-once claim.
- Request/reply correlation, timeouts, and cancellation have deterministic documented behavior.
- Operators can inspect a failed message and understand what happened.
- Default access is safe, and agent-level permissions are tested.
- All four harness adapters still work at least to the capabilities verified for each one, without regression.
- Test evidence and operational guidance are delivered alongside code.

## Working instructions
Implement the features incrementally and actually run tests after each milestone. Prefer small, readable packages and migrations, dependency injection for adapter tests, and stable interface contracts. When a requirement conflicts with existing behavior, choose the least disruptive path, document the trade-off, and add regression coverage. If a live provider or credential is unavailable, continue with testable code and a clear integration validation checklist—do not invent successful test results.

**Begin by reading the entire repository and reporting the baseline, then proceed directly with the implementation.**
