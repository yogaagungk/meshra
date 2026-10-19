# Security model

## Trust boundary

The daemon only accepts loopback addresses (default `127.0.0.1:7331`), including explicit IPv6 loopback and `localhost`. It is a local single-user service, not a hardened multi-tenant remote gateway. All API and MCP requests require a bearer credential. Browser Origin requests are rejected; localhost binding is not authentication. Do not forward its port or publish the admin token.

The data directory is mode 0700. `admin.token`, SQLite, and the daemon lock file are mode 0600. SQLite WAL/SHM files live inside that protected directory. The administrator token is a random 256-bit credential stored outside SQLite. Agent and per-turn token verifiers use SHA-256; raw agent credentials are returned only at issuance. The daemon takes an exclusive OS file lock so two daemons cannot independently schedule the same inbox.

Human CLI commands default to the admin token. `MESHRA_TOKEN` overrides that credential. Do not pass admin credentials into harnesses. `meshra agent token <id>` issues an identity/session-bound credential; treat stdout as secret. Issue it after starting the native session when session-scoped history access is needed. A token issued before any session exists represents the pre-session origin, not a later session. There is no implicit identity migration. Administrator callers may explicitly send as a registered identity; agent callers cannot impersonate another identity using `from`.

Per-turn credentials are scoped to the destination identity, native session and active task. The adapter updates the session scope when a deferred SDK allocation reports its native ID. These credentials expire when the turn finishes, is cancelled, times out or the relay restarts. Nested task creation inherits the authenticated parent task rather than a caller-controlled ancestry assertion.

Agent policies are deny-by-default. `operations` controls `list`, `status`, `events`, `send`, `request`, and `cancel`. `destinations` allows exact canonical identities or an explicitly configured `*`. `delegate` is independently required for nested requests. Registry/lifecycle changes, token issuance and retry require administrator authority. Task access additionally checks the caller's identity and native session against the sender, root origin or destination recorded on that task.

Removed identities are tombstoned and cannot be re-registered under the same canonical ID. This prevents a later, unrelated session owner from inheriting old identity history or permissions by recycling a name.

## Secrets and payloads

Only model/provider identifiers are accepted as runtime configuration; there is no arbitrary environment/credential map. Harness secrets remain in native auth stores or inherited environment. Never put API keys in model IDs, prompts, display names or messages. The relay does not log authorization headers or raw protocol frames. Task text, model output and event content are intentionally persisted and may themselves contain sensitive repository data: protect the data directory and backups accordingly. This MVP does not encrypt SQLite or native harness session files at rest.

Agent-generated text is not parsed as a relay command. Only authenticated structured API/MCP calls execute routing operations. Input sizes, outstanding inboxes, execution attempts, event payloads and output sizes are bounded; see [protocol.md](protocol.md). Persisted history has no automatic retention policy yet: monitor disk space and retire old data directories when no longer needed.

## Tools, approvals and repository coordination

Adapters use conservative permissions and never enable bypass/auto-approve modes. Their exact tool/approval limitations are in [adapters.md](adapters.md). An approval denial is an event and/or explicit failed task, not a hidden approval. The MVP is not an interactive approval console. Resolve native credentials or permissions intentionally, inspect any external effects, and retry explicitly. Do not weaken harness sandbox settings to make a smoke test pass.

One active turn per identity, plus repository-root locks, prevents concurrent Meshra turns from writing the same Git working tree. All turns take this lock because generated instructions cannot reliably predict their effects. Distinct non-Git roots with overlapping files, external tools, Git linked-worktree shared metadata and symlink changes after registration are outside this advisory lock's guarantee. Use separate repositories/worktrees or the same canonical configured root. The relay is not an OS sandbox.

Stopping or cancelling a turn does not undo tools already executed. Restart recovery marks uncertain executions as failed and requires a deliberate manual retry, rather than repeating potentially destructive actions automatically. Queued tasks are durable but cannot execute until their destination is explicitly started again.

## Installation and exposure

No harness user configuration is changed automatically. Use the verified MCP integration examples in the adapter documentation; replace paths and use only the particular agent's credentials. Optional Claude/OpenCode dependencies are installed locally, not globally. Provider calls may incur costs. Authentication-dependent smoke tests are separate from deterministic CI tests and require explicit opt-in.

Known local-user boundary: another process running as the same OS user can read native auth stores and the private relay token files. Meshra does not claim isolation from that user or root. CLI/API event access is not a redaction service.
