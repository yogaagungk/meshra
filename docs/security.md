# Security model

## Trust boundary

The daemon only accepts loopback addresses (default `127.0.0.1:7331`), including explicit IPv6 loopback and `localhost`. It is a local single-user service, not a hardened multi-tenant remote gateway. All API and MCP requests require a bearer credential. Browser Origin requests are rejected; localhost binding is not authentication. Do not forward its port or publish the admin token.

The data directory is mode 0700. `admin.token`, SQLite, and the daemon lock file are mode 0600. SQLite WAL/SHM files live inside that protected directory. The administrator token is a random 256-bit credential stored outside SQLite. Agent and per-turn token verifiers use SHA-256; raw agent credentials are returned only at issuance. The daemon takes an exclusive OS file lock so two daemons cannot independently schedule the same inbox.

Human CLI commands default to the admin token. `MESHRA_TOKEN` overrides that credential. Do not pass admin credentials into harnesses. `meshra agent token <id>` issues an identity/session-bound credential; treat stdout as secret. Issue it after starting the native session when session-scoped history access is needed. A token issued before any session exists represents the pre-session origin, not a later session. There is no implicit identity migration. Administrator callers may explicitly send as a registered identity; agent callers cannot impersonate another identity using `from`.

Per-turn credentials are scoped to the destination identity, native session and active task. The adapter updates the session scope when a deferred SDK allocation reports its native ID. These credentials expire when the turn finishes, is cancelled, times out or the relay restarts. Nested task creation inherits the authenticated parent task rather than a caller-controlled ancestry assertion.

Outbound agent policies are deny-by-default. `operations` controls `list`, `status`, `events`, `send`, `request`, and `cancel`. `destinations` allows exact canonical identities or an explicitly configured `*`. `delegate` is independently required for nested requests. Registry/lifecycle changes, token issuance and retry require administrator authority. Task access additionally checks the caller's identity and native session against the sender, root origin or destination recorded on that task.

Recipients can additionally restrict `policy.senders` to exact canonical identities or `*`, and `policy.inboundOperations` to `send`, `request`, or `*`. Both the sender's outbound policy and the recipient's inbound policy must allow a message. An omitted or `null` inbound list preserves v0.1 behavior; an explicit `[]` denies everyone or every inbound operation respectively, and stays empty through JSON/SQLite round trips. Administrator sends bypass both policy directions, consistently with existing administrator authority. For example:

```json
{
  "senders": ["omp:exampleworker"],
  "inboundOperations": ["request"]
}
```

Forbidden sends and requests produce metadata-only audit records containing time, authenticated actor, canonical recipient, operation, and category. Malformed identities are normalized rather than copied into the audit. Denial logs contain the same metadata, never message text, bearer credentials, or arbitrary error details. Audit-storage failures are reported without database error contents.

Removed identities are tombstoned and cannot be re-registered under the same canonical ID. This prevents a later, unrelated session owner from inheriting old identity history or permissions by recycling a name.

## Secrets and payloads

Only model/provider identifiers are accepted as runtime configuration; there is no arbitrary environment/credential map. Harness secrets remain in native auth stores or inherited environment. Model/provider identifiers and display names reject control characters and recognized credential forms. Working directories must resolve to existing canonical directories; a startup check rejects removed directories and replacement symlinks that change the canonical path. Never put API keys in model IDs, prompts, display names or messages.

The relay does not log authorization headers or raw protocol frames. Persistence applies best-effort redaction to recognized credentials in task text, output, events, and errors, including common vendor API keys, Bearer values, secret/password/key/token assignments and JSON fields, and URL user information. Native session and correlation identifiers are not blindly redacted. Idempotency fingerprints are computed from original requests so two different secret-bearing requests cannot silently collapse after redaction.

Text credentials can span native delta boundaries. The relay therefore buffers at most 256 KiB of delta text per turn and redacts the successful aggregate, including the current per-turn credential, before emitting persisted chunks. Session/progress/approval events remain immediate; failed/cancelled partial delta text is not persisted. This changes v0.1 text availability timing intentionally, without changing task/result/SSE shapes. Legacy task/event content is redacted during migration, but this is not secure erasure of old SQLite pages, WAL or backups.


Redaction is defense in depth, not a general secret detector or encryption. Unknown formats, unlabelled secrets, repository content, and native harness session files can still be sensitive. Protect the data directory and backups. Meshra does not encrypt SQLite or native harness session files at rest, and redaction does not scrub another process's logs or native authentication stores.

Agent-generated text is not parsed as a relay command. Only authenticated structured API/MCP calls execute routing operations. Input sizes, outstanding inboxes, execution attempts, event payloads and output sizes are bounded; see [protocol.md](protocol.md). Terminal-history retention is configured on the daemon; see [cli.md](cli.md). Active work is not a disposable log.

## Tools, approvals and repository coordination

Adapters always run harnesses in **full-access mode**: Claude uses `bypassPermissions`, Codex uses the `danger-full-access` sandbox with `approvalPolicy: never`, OpenCode allows every permission and OMP uses `--approval-mode=yolo`. Harnesses can read, write and delete anything the Meshra OS user can, run any command and reach the network, without approval. Instructions arriving from other agents, routines or external conversations run with that same authority, so only route trusted senders to these identities and keep the repository under version control. Tool-free profile drafting is the one exception and still denies every tool. `policy.worktreeWrites` is now redundant and remains rejected. The relay's broker tools, private snapshots and changeset approvals still exist, but they no longer bound native harness writes. See [adapters.md](adapters.md).

One active turn per identity, plus repository-root locks, prevents concurrent Meshra turns in the same discovered Git working tree. All turns take this lock because generated instructions cannot reliably predict their effects. Canonical paths are rechecked before runtime startup, but this is not an atomic filesystem sandbox: changes after the check, distinct non-Git roots with overlapping files, external tools, and Git linked-worktree shared metadata remain outside the advisory lock's guarantee. Use separate repositories/worktrees or the same canonical configured root. The relay is not an OS sandbox.

Stopping or cancelling a turn does not undo tools already executed. Restart recovery marks uncertain executions as failed and requires a deliberate manual retry, rather than repeating potentially destructive actions automatically. Queued tasks are durable but cannot execute until their destination is explicitly started again.

## Installation and exposure

No harness user configuration is changed automatically. Use the verified MCP integration examples in the adapter documentation; replace paths and use only the particular agent's credentials. Optional Claude/OpenCode dependencies are installed locally, not globally. Provider calls may incur costs. Authentication-dependent smoke tests are separate from deterministic CI tests and require explicit opt-in.

Known local-user boundary: another process running as the same OS user can read native auth stores and the private relay token files. Meshra does not claim isolation from that user or root. CLI/API history is not a general-purpose redaction service; do not use it to sanitize arbitrary secrets for publication.
