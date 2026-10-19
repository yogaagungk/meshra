# Meshra v0.3 — Agent Collaboration & Shared Context

> Implementation plan for the existing Meshra repository. Preserve all working v0.1 and v0.2 behaviors. This document is a plan, not a claim that these capabilities exist today.

## 1. Objective

Enable multiple named agent instances across OMP, Claude Code, Codex, and OpenCode to collaborate on related work through durable conversations, explicit task delegation, dependency-aware execution, and shared context **by reference**.

Meshra remains communication infrastructure, not a central AI planner. Agents choose when to delegate; Meshra manages identity, delivery, authorization, lifecycle, and evidence.

### Non-negotiable invariants

1. Agent identity is stable and unique, e.g. `claude:apmobileprod`, `claude:apmobilewallet`, `omp:adsengine`.
2. Sessions and permissions remain isolated; same-harness agents must not share history by accident.
3. v0.2 durable messaging, deduplication, retry, and recovery contracts remain supported.
4. No full chat-history synchronization or access to model-internal context windows.
5. Artifact content and cross-agent messages are **untrusted data**, not instructions overriding policy.
6. Existing interactive sessions must not be assumed attachable; verify support per adapter.

## 2. MVP-to-v0.3 deliverables

**P0 (release gate)**
- Persistent multi-participant conversations with explicit membership and ACLs.
- Task delegation with owner, requester, result, correlation, deadline, bounded recursion, cancellation, and state transitions.
- Acyclic task dependencies, with ready/blocked semantics and parallel scheduling where safe.
- Shared context registry and immutable, content-addressed artifacts.
- Reference-based context attach, lookup, publish, fetch, and audit trail.
- Safe context injection where adapters cannot fetch directly; bounded token/byte budget, explicit provenance, no silent history dump.
- Crash recovery and versioned database migrations.
- End-to-end validation across real adapters (at minimum OMP, Codex, Claude; include OpenCode before calling four-harness support complete).

**P1 (same release if feasible)**
- Git worktree allocation/cleanup for concurrent write tasks.
- Conflict detection, least-privilege write scopes, and approval gates.
- Lightweight CLI traces and conversation/task inspection.

**Deferred**
- AI-generated task plans, vector search, automatic semantic memory, distributed message brokers, a web dashboard, and autonomous auto-merge.

## 3. Logical architecture

```text
CLI / MCP tools / HTTP API
            |
   Authorization Boundary
            |
+--------------------------------+
| Conversation Service           |
| Task / Dependency Scheduler    |
| Shared Context Service         |
| Artifact Store + Audit         |
+--------------------------------+
            |
  Meshra v0.2 Durable Messaging
            |
 Harness Adapters + Agent Registry
  OMP | Claude | Codex | OpenCode
```

Reuse the existing Go/SQLite/HTTP/MCP architecture where already implemented. Add services behind interfaces; do not introduce an external database or queue for v0.3.

## 4. Resource model

- **Agent**: stable `harness:name` identity with allowed scopes/capabilities.
- **Session**: adapter-specific runtime session, explicitly mapped to Agent; may be offline.
- **Conversation**: durable collaboration channel with participants and policy.
- **Task**: delegated unit of work, linked to a conversation and target agent; has zero or more dependencies.
- **Context**: metadata describing a versioned immutable content reference.
- **Artifact**: content-addressed bytes/UTF-8 text stored under a hash and size.
- **Context binding**: explicit task/conversation association to a particular immutable context version.

Example lifecycle:

```text
conversation.create → participant.add → context.publish
  → task.create(to=codex:implementer, contextRefs=[...])
  → task.claim/run → context.get → task.complete(resultRef=...)
  → task dependent becomes ready → reviewer consumes refs
```

### Recommended task states

`created → blocked | queued → running → succeeded | failed | cancelled | timed_out`

- Validate transitions transactionally.
- Treat `running` after restart as `reconciling` (internal status) until the adapter confirms the actual state. Never blindly reissue a side-effecting turn.
- Cancellation is best-effort: record a requested cancellation even if an adapter cannot abort immediately.
- A task with failed dependencies must follow a documented policy (`blocked` until retry, or `skipped`), not run silently.

## 5. Data design (adapt to existing schema)

Use SQLite migrations (idempotent, rollback-tested where possible). Suggested tables:

| Table | Purpose | Key constraints |
|---|---|---|
| `conversations` | id, title, policy, created_by, state | unique id |
| `conversation_participants` | conversation_id, agent_id, role | unique pair, FK |
| `tasks` | id, conversation_id, requested_by, assigned_to, state, deadline, result_ref, idempotency_key | unique idempotency scope |
| `task_dependencies` | task_id, depends_on_id | unique pair, prevent cycles |
| `contexts` | id, logical key, owner, scope, visibility, type, metadata | unique id |
| `context_versions` | context_id, version, digest, size, media_type, created_by, created_at | unique context/version; immutable |
| `context_bindings` | task_id or conversation_id, context_id, version | validate exactly one binding target |
| `context_acl` | context_id, agent_id or role, permission | deny by default |
| `context_access_log` | caller, operation, context_id/version, timestamp, outcome | append-only |
| `artifacts` | sha256, storage_path, size, media_type | hash integrity and unique digest |

Reuse existing audit/event tables if they already satisfy the need. Do not duplicate v0.2 message IDs, retry queues, or registry state.

## 6. API and MCP contract (illustrative; reconcile with v0.2 conventions)

### Conversations
- `conversation.create(title, participants, policy)` → `conversationId`
- `conversation.get(conversationId)`
- `conversation.addParticipant(conversationId, agentId)`
- `conversation.list(cursor, limit)`
- `conversation.events(conversationId, cursor)`

### Tasks
- `task.delegate(conversationId, fromAgent, toAgent, instruction, contextRefs, dependsOn, idempotencyKey, deadline)` → `taskId`
- `task.get(taskId)`
- `task.list(conversationId, cursor, status)`
- `task.cancel(taskId, reason)`
- `task.events(taskId, cursor)`

### Shared context
- `context.publish(type, title, scope, visibility, content or artifact, idempotencyKey)` → `{contextId, version, sha256}`
- `context.get(contextId, version)` → metadata + authorized content or artifact handle
- `context.search(scope, type, tags, keyword, cursor)` → metadata only
- `context.attach(targetTaskId or conversationId, contextId, version)`
- `context.revoke(contextId, principal)` → revokes access, does not erase previously transmitted tokens

For large artifacts, separate `artifact.put` and `artifact.get` from metadata APIs; use streaming or bounded downloads. Prefer `meshra://contexts/{id}/versions/{version}` and `meshra://artifacts/sha256/{digest}` as resource identifiers when exposing MCP Resources.

### Example delegation

```json
{
  "conversationId": "conv_payment_refactor",
  "fromAgent": "claude:apmobileprod",
  "toAgent": "codex:implementer",
  "instruction": "Implement the approved API-compatible change.",
  "contextRefs": [
    {"contextId": "ctx_api_contract", "version": 2},
    {"contextId": "ctx_arch_decision", "version": 1}
  ],
  "dependsOn": [],
  "idempotencyKey": "payment-refactor-impl-v1",
  "deadline": "2026-10-21T10:00:00Z"
}
```

## 7. Context resolution and injection

1. Publishing agent creates a **small, typed context unit** (decision, contract, findings, test report, patch reference).
2. Meshra validates size, scope, ACL, optional secret scanning, and stores the bytes immutably.
3. Delegation message contains exact immutable `{contextId, version}` references.
4. Target adapter authorizes access **as the receiving agent** (not the sending agent), then uses either:
   - **Explicit fetch:** expose `context.get` via MCP/tool invocation; preferred.
   - **Bounded injection:** resolve allowlisted refs at turn start, annotate source/version and untrusted-data boundaries.
5. Record access, including denied attempts. Do not automatically push updates into active model contexts.
6. New information produces a new context version; existing tasks remain pinned to the original version unless explicitly rebound.

Default to a metadata preview first. Enforce configured maximum artifact bytes and cumulative injected context budget. Never persist credentials, tokens, `.env` secrets, or private keys as shared context. Redact where appropriate and make context visibility narrow by default.

## 8. Scheduler and concurrency safety

- Construct and validate a DAG before task activation; reject cycles.
- Serialize turns that target the **same live session** unless the adapter explicitly supports concurrency.
- Parallelize independent tasks only when write scopes do not overlap or isolated worktrees are assigned.
- Keep deadlines, maximum delegation depth, maximum fan-out, and task budgets configurable.
- Ensure `messageId`, `taskId`, and `contextVersion` are independently traceable.
- Handle relay restart, offline agent, slow response, duplicate event, failed artifact upload, missing context, and permission revocation explicitly.

Delivery guarantees are **at-least-once where technically achievable**, not exactly-once execution. For a side-effecting task with uncertain completion, require reconciliation or human confirmation rather than automatic replay.

## 9. Milestones

### M0 — Repository audit / design freeze
- Inspect actual v0.2 APIs, adapters, SQLite schema, tests, and permission model.
- Map what can be reused; record compatibility decisions in `docs/ADR-v0.3.md`.
- Confirm real session/cancellation capabilities per harness before coding.

### M1 — Conversation foundation
- SQLite migrations, participants/ACL, REST/CLI and MCP conversation operations.
- Unit and integration tests for isolation, membership, pagination, restart.

### M2 — Shared Context foundation
- Immutable artifact storage, hash verification, context versions, ACL, publish/get/search/attach.
- Tests for unauthorized fetch, immutable version pinning, content corruption, oversized payloads.

### M3 — Delegation and dependencies
- Task state machine, DAG validation, scheduler, result artifact refs, cancellation, correlation, reconciliation.
- Tests for cycles, duplicate requests, queue order, offline recipients, timeouts and restart recovery.

### M4 — Harness delivery integration
- Expose MCP tools and adapt native OMP, Claude, Codex, OpenCode integration to pass refs and lifecycle events.
- Use per-adapter fallbacks only when necessary; document unsupported functionality honestly.

### M5 — End-to-end validation and hardening
- Real 3-agent scenario: `claude:apmobileprod` publishes contract → `codex:implementer` executes dependent task → `omp:reviewer` validates using pinned refs → original conversation receives results.
- Real same-harness isolation scenario: `claude:apmobilewallet` must not observe context restricted to `claude:apmobileprod`.
- Verify OpenCode connectivity separately before claiming all four integrations pass.
- Test restart during work, duplicate delivery, denial, cancellation, and busy-session serialization.
- Publish migration notes, changelog, CLI examples, resource usage, and known limits.

## 10. Acceptance criteria (v0.3 release gate)

- [ ] At least three named agents can participate in one persistent conversation.
- [ ] Two Claude instances remain isolated across messages, tasks, context, and permission checks.
- [ ] Explicit shared context refs can be published, attached, fetched, and audited.
- [ ] A pinned context version yields identical bytes regardless of later updates.
- [ ] Context visibility is enforced server-side and cannot be bypassed by switching adapters.
- [ ] Agent A can delegate to B, and B's response is correlated to the correct task and conversation.
- [ ] Task dependencies block execution correctly and cyclic graphs are rejected.
- [ ] Independent tasks may run concurrently without unsafe shared-workspace writes.
- [ ] Restart preserves all durable state, and uncertain side-effect execution is not blindly duplicated.
- [ ] Loop/fan-out/deadline protection works and cancellations are auditable.
- [ ] A real end-to-end test exercises at least three different harnesses; all four have documented test results.
- [ ] Existing v0.1/v0.2 compatibility and regression suites pass.
- [ ] Development mode works on WSL2 within the existing 6 GB memory cap for a small representative scenario (measure, do not guarantee heavy parallel agent loads).

## 11. Deliverables for the coding agent

- Working implementation with migrations and no hidden mocked success paths.
- Automated unit, integration, and representative E2E tests.
- `docs/ADR-v0.3.md` and `docs/V0.3_DESIGN.md` containing schema and behavioral contracts.
- `docs/V0.3_RUNBOOK.md` with startup, backup, restore, troubleshooting, and safe rollback steps.
- `docs/V0.3_TEST_REPORT.md` with exact commands, passed/failed tests, limitations, and evidence.
- CLI/MCP usage examples for real named agents.

## 12. Execution instruction (paste into Codex / Claude / OMP)

> Read `MESHRA_V0.3_IMPLEMENTATION_PLAN.md` and inspect the existing Meshra repository before editing. Implement milestones M0–M5 incrementally, preserving all v0.1/v0.2 APIs and behavior. Treat multi-instance isolation and context authorization as release blockers. Follow the repository's existing architecture where sensible; justify deviations in an ADR. Run automated tests after each milestone and perform real cross-harness E2E validation where credentials and adapters are available. Do not claim integration success based only on mocks. Do not silently retry tasks with uncertain side effects. Deliver code, migrations, tests, documentation, and an evidence-based test report.
