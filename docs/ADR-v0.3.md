# v0.3 design decisions (M0)

The inspected v0.2 implementation uses a single SQLite connection and relay mutex,
JSON task bodies, transactional events, session-bound bearer credentials, and
repository/session serialization. Preserve the Service interface and every v1
route. Add collaboration as an optional service interface and additive routes.
Reuse durable tasks, correlation, ancestry bounds, offline delivery, cancellation,
and uncertain-execution failures. Never replay interrupted side effects.

Conversation membership does not grant context access. Context ACLs use exact
canonical agent IDs, independent of adapter. Only owners/admins grant or revoke;
bindings never widen permissions. Authorize injected bytes as the recipient at
dispatch, and audit denials. Content is bounded, immutable and untrusted.

Native capability inspection: OMP/Codex support owned exact-ID sessions and
abort/interrupt; Claude SDK supports exact resume and AbortController; OpenCode
supports session IDs and abort. No arbitrary interactive attach is supported.
Installed adapter contracts and v0.2 native evidence are in
V0.2_ADAPTER_VERIFICATION.md. Active cancellation remains best-effort, not rollback.
Real provider tests must establish current availability independently.

M0 baseline: `go test ./...` passed on 2026-10-19 (all packages).
P1 worktrees/write approvals are deferred: current adapters reject write enablement;
existing repository locks serialize shared workspaces.
