# Changelog

## Unreleased

- Use neutral example agent identifiers consistently across plans, documentation, routing regressions, and executable smoke scenarios.
- Split synthetic secret-shaped test literals without changing the redaction cases or bypassing repository push protection.

## 0.2.0 — 2026-10-19

### Reliable communication

- Extend the existing Go/SQLite relay rather than replacing v0.1. Add transactional schema-2 migration preserving agent/task/message/correlation IDs, task order, event sequences, and v0.1 foreign-key relationships; refuse unsupported future schemas and roll back failed upgrades.
- Persist claim owner/lease, destination session, offline delivery checks, jittered retry eligibility, maximum dispatch attempts, error category and ambiguous execution. Keep `dispatching` as the compatible logical claimed state; record delivery separately from completion.
- Recover queued work and retry schedules after restart. Never automatically replay potentially invoked work; interrupted operations require explicit reconciliation and manual retry.
- Scope admission keys to sender/session/operation and retain original-request fingerprints before redaction. Propagate parent deadlines; preserve one active turn per identity/repository and cap active turns at 32 globally.
- Keep cancellation/deadline terminal state authoritative over late results, including downstream operations that ignore cancellation. Schedule against absolute persisted deadlines so time spent persisting/logging transitions does not extend the next timer interval.

### Operator and access controls

- Add task trace, retained-history metrics and installation/workspace doctor through the existing CLI/API; preserve all seven v0.1 MCP names and add registration, trace, manual retry, metrics and doctor tools.
- Add opt-in synchronous HTTP/CLI request waiting with an explicit deadline; asynchronous behavior remains the default and disconnect does not cancel admitted work.
- Add optional recipient sender/operation allowlists, metadata-only denial audits, canonical-workspace startup checks, recognized-credential redaction, payload-free structured lifecycle logs, and configurable terminal/audit retention (default 30 days).
- Keep loopback-only authenticated service and conservative harness approval boundaries. Reject worktree-write enablement unsupported by current adapters; do not claim universal OS filesystem isolation.
- Bound traces to the latest 256 events and 2048 bytes per data field, signal truncation explicitly, and retain full bounded cursor-paginated events. Increase bounded MCP stdio response capacity to 32 MiB for JSON-escaped Task/Trace text plus structured content.

### Adapters and verification

- Reverify installed OMP18.12.1, Codex0.163.0, Claude SDK0.3.303/CLI2.1.303, and OpenCode1.19.3 against installed capabilities and official sources.
- Fix OpenCode1.19.3 permission PATCH append growth: append the safe policy only when its exact tail is missing. Add the authorized trace tool to Claude/OpenCode read-oriented MCP allowlists without enabling administrator or write tools.
- Add migration/rollback, retry/recovery, permissions/redaction, large MCP trace, deadline/cancellation, real CLI/MCP/native routing smokes, and repeatable `/proc` relay-load measurement. Current commands/results and live-versus-simulated limits are in `docs/V0.2_TEST_REPORT.md`.

### Upgrade and intentional compatibility changes

- Back up the stopped relay directory **and native harness state** before upgrading. API/domain `schemaVersion: 1`, existing endpoints/CLI commands and MCP names remain. Rollback requires the untouched pre-upgrade database/native backup; never downgrade a schema-2 file in place.
- **Native text timing changes:** raw deltas can split a credential across events. Buffer at most 256 KiB, redact the complete successful output, then persist bounded delta chunks before completion. Progress/session/approval events remain immediate. Failed/cancelled partial text is discarded. Consumers must concatenate text and inspect terminal state, not pin native delta counts/timing.
- Offline work can now fail after delivery-check exhaustion before its deadline; it still never auto-starts a runtime. Configure the policy on `serve`.
- Terminal history and its admission keys expire by retention rather than lasting forever; active work and required ancestry remain protected. Pruning is logical deletion, not secure erasure of old pages/WAL/backups.
- Claude/OpenCode authenticated provider/MCP turns are not live-verified without their native credentials. No exactly-once execution, arbitrary interactive attachment, autonomous escalation, or remote-hosting claim is introduced.
