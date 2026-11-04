# Changelog

## Unreleased

### Console motion, Settings and code view

- Every tab menu (Settings, Automation, Agents, Workspaces Files/Changes, Stats,
  profile chat tools) now uses one tab bar with a sliding active marker, arrow
  key navigation and content that arrives from the side the tab moved toward.
  Pages fade in, the first blocks of a panel settle in a short sequence, and
  `prefers-reduced-motion` reduces it to a short fade.
- Settings is a two-pane layout grouped as General, AI, Connections and System
  (Appearance, Personal access, Providers, Models & usage, AI status, Network
  access, MCP servers, Messaging, Resources, Runtime), one section at a time,
  deep-linkable as `#settings/<section>`. On narrow screens the navigation
  becomes a scrolling tab strip. Connection rows group name, tags and actions;
  adding a network origin opens inline with a connection picker.
- Workspace file previews and Git diffs use a read-only code view with syntax
  colors, line numbers, wrap and copy, in light and dark themes.

### Usage stats

- New **Stats** page with Overview, Models, Providers and Requests tabs: cost,
  requests, conversation tokens, cache rate, error rate, latency, TTFT,
  tokens/s, token mix, per-model and per-provider breakdowns, peak burn hours
  and a filterable request log, for 24 hours, 7 days or 30 days.
- Direct API calls are recorded per request (duration, time to first token,
  stop reason, usage, failures) in the new `model_requests` table.
- OMP and Claude are imported read-only: `~/.omp/stats.db` (per-request latency
  and API-equivalent cost) and `~/.claude/projects/**/*.jsonl` (token usage per
  message only; no latency or price). Only numbers, model, project path and
  timestamps are stored, never message text. This reads files outside the data
  directory, so it covers all usage on this machine, not just Meshra-run work;
  the Project filter narrows it. Set `MESHRA_STATS_IMPORT=off` to disable, or
  `MESHRA_OMP_STATS_DB` / `MESHRA_CLAUDE_PROJECTS_DIR` to relocate the sources.
- Operations `stats.summary`, `stats.requests` and `stats.refresh`; stats are
  kept 30 days (migration 026).

### Removed

- Remove the Local voice feature: speech-to-text and read-aloud jobs, the
  `voice.*` automation operations, the console audio route, the Voice draft tab
  and the Local voice readiness card. Migration 025 drops the `voice_settings`
  and `voice_jobs` tables, and the sandbox no longer has a `voice` worker kind.

### Provider API compatibility

- Provider connections now choose an **API compatibility** format: `openai`
  (chat completions, the default and the format of every existing endpoint),
  `anthropic` (Messages API with `x-api-key`, tool use, images, usage and
  cache accounting) or `typesafe` (System One).
- Anthropic endpoints use one complete reply per step; Messages API streaming is
  not implemented. `max_tokens` is fixed at 8192.
- TypeSafe System One returns structured judgments (choice, score, noul), not
  text or tool calls. It can be saved and its models listed, but a profile or
  turn bound to it is refused with a clear error.

### Direct API agent tools

- Separate Direct API system instructions from tasks, stream compatible
  completions, bound repeated calls, parallelize read-only calls and compact
  older in-turn context. Profiles can configure `maxToolCalls` (default 40).
- Add turn-local todos, profile-scoped session search, capability presets,
  reviewable skill proposals and opt-in memory staging.
- Add opt-in exact approval rules with audit records, expiry and revocation;
  unattended actions continue to require human approval.
- Add confined background process control and reviewed Python execution with
  nested calls through the same broker; original-workspace application remains
  a separate approval.

### Three ways to run an AI

- **Breaking:** remove the Codex, OpenCode and ACP harnesses, their adapters, bridges, connection screens and quota/status code. Only Claude, OMP and Direct API remain. OMP can itself use OpenCode, OpenRouter and Codex subscriptions.
- Migration 20 deletes agents, profiles, queued tasks, chats and saved ACP configurations that belonged to the removed harnesses, and drops the `acp_agents` table.
- Add **Direct API** as a first-class option for agents, profiles and one-to-one chats: pick a saved OpenAI-compatible endpoint and a model; no CLI is started.
- Show tool calls in chats and agent replies. A running turn animates the tool in flight; the summary expands to each call's input and output. Claude, OMP and Direct API report them as `tool_use` and `tool_result` events, bounded to 4,000 characters and redacted.

### Automation revamp

- Skills can now be written in the console and pinned to profiles from the skill editor or the profile editor; updating a skill shows which profiles still pin an older version.
- Flows have a list, names (`flows.list`, optional `name`), a plain-language step editor with "Starts after" choices that refuse loops, and a live diagram.
- Add a Runs tab: a receipt for every routine occurrence, skip reasons in plain language, and a per-step status diagram with Cancel run.
- Queue shows only agents with work, applies reordering immediately, and offers Send next, Later and Cancel with real labels, ages and conversation links.
- Routine cards show profile and flow names, the last run and links; "Schedule this" in a profile chat pre-fills a new routine.
- Collapse the approvals block to one line when nothing is waiting, add count badges to tabs and deep-linkable tabs (`#automation/flows`).

### Routing and coordination

- Address agents with `@name` or `@everyone`, with keyboard autocomplete; a lone member or the agents last addressed receive plain messages, and the composer shows why. Nothing is ever broadcast implicitly.
- Add optional lead routing (`routing.route`, `routing.result`): a lead on a verified tool-free harness picks recipients in a disposable turn; answers are strictly decoded and narrowed to current participants.
- Show queued messages in chat with pause, resume, run next and cancel; offer "Try with another agent" on failed turns instead of replaying automatically.
- Make agent cards lead with the friendly name and keep the exact ID visible; open the describe-an-agent draft by default for new profiles.
- Refactor profile generation onto a shared disposable tool-free turn helper.

### Console revamp

- Refresh the light and dark glass surfaces with flat backgrounds and colored navigation.
- Render Mermaid diagrams and tables in chat, and add seven selectable animated agent icons with custom colors.
- Show reported usage and cost coverage without inventing prices or unreported totals.
- Create general agents in private working directories or assign them to registered workspaces by ID; reject arbitrary directory paths for new public agent registrations. Existing agents keep their working directories.

### Console management

- Add confirmed conversation and agent deletion, with active-work safeguards and explicit retention of shared or audit data.
- Add confirmed context deletion with owner/admin authorization; immutable artifacts and audit history remain retained.
- Exclude chat-origin tasks from the Activity view without removing the chat execution records needed for history and recovery.
- Add semantic positive, warning, error and danger button styles.

### Full-access harnesses

- Run every harness in full-access mode: Claude `bypassPermissions` with all tools, Codex `danger-full-access` with `approvalPolicy: never` and accepted command/file approvals, OpenCode `*: allow` with `once` replies, and OMP builtin tools with `--approval-mode=yolo`. Profile-managed Claude/OMP sessions keep native builtins beside the verified broker catalog. Tool-free profile drafting is unchanged.

### Optional automation boundary

- Report `201 Created` for automation saves without an `expectedRevision` (including client-selected IDs) and `200 OK` for revision-checked updates.
- Add an optional context-aware automation service over authenticated bearer and console POST routes without changing the legacy service or runtime contracts. Unknown operations remain unavailable until their backing behavior is implemented.
- Bind console-only authority to a digest of a validated live session, revalidate against the real session store, and notify relay revocations outside API session locks.
- Factor task admission into one transaction-aware core so feature receipts fail atomically with task/event insertion; retain ordinary authorization, ordering and idempotency.
- Add typed automation clients and authenticated byte-preserving upload/download transport without mutation retries.
- Add a CLI `meshra automation OP JSON_OBJECT` route over the same bearer automation service, with `meshra automation runtime-status` as a confined-runtime readiness shortcut. Approval decisions, human takeover and microphone capture stay console-only.
- Stream automation resource-change metadata over the console event feed with cursor replay, resync and heartbeat rules; events never carry task text, tool arguments or secrets.
- Add portable profiles with separate conversation-specific execution identities, immutable admission snapshots, exact-version context grants, bounded history disclosure and revision-checked provider switches. Canonical named identities and legacy disposable chat remain separate.
- Add asynchronous, metered profile drafting with disposable empty workspaces and reviewed name/instructions-only output. OMP verifies an empty native tool roster; Claude enforces an empty SDK catalog. Installed Codex 0.163.0 and OpenCode 1.19.3 refuse drafting where a complete native-tool/discovery denial cannot be enforced.
- Add literal, bounded profile history search, scoped revisioned memories, persistent queue pause/reorder, safe native-context rotation and explicit reset cutoffs without changing transcript sequence.
- Add byte-exact authorized attachments with integrity checks, quotas, draft expiry and provenance-preserving deletion; reject unsupported native image transports rather than silently omitting images.
- Attribute run usage to admitted profile/binding revisions, preserve partial native reports and nullable costs, and distinguish configured from reported model/provider data.
- Add active-attempt tool invocations, expiring exact-action approvals, session-owned clarification and uncertainty recovery. Bearer and agent credentials cannot decide human attention.
- Add Linux bubblewrap workers with private snapshots, delegated cgroup-v2 limits, manifest checks and separately reviewed host changesets. Missing confinement prerequisites refuse execution; no host fallback is provided.
- Add exact-origin, DNS-pinned egress and revocable scoped sockets. Connector ingress carries its own narrow route identity rather than administrator authority.
- Add immutable pinned skills and scoped, bounded asset reads. Review ZIP/TAR/TAR.GZ skill archives without executing their contents; reject unsafe paths, links, devices, duplicate entries and size overruns. Template export/import strips connection credentials, native sessions and execution grants; digest-pinned single-use previews create inert drafts.
- Add a confined ACP launcher with a read-only private workspace, isolated native state, selected authentication files and exact-origin egress. Capability/session checks make no model request; missing runtime manifests, bubblewrap or delegated cgroups refuse startup.
- Pin endpoint/ACP configuration and encrypted credential revisions in admitted task snapshots; later configuration edits cannot redirect admitted inference. Credential replacement blocks subsequent calls, and disabling a connection cancels its discovery probes.
- Support default text-only endpoint profiles without advertising broker tools; reject explicit tool-dependent grants before admission. A real HTTP fixture reproduced the prior no-request failure and completed after the fix with no tools offered.
- Grant only the reviewed public endpoint origin on explicit enable, preserve revocation across metadata edits, and require separate confirmation for private targets. Scan canonical installed CLI paths without executing them and accept explicitly configured custom ACP v1 servers under the same confinement gate.
- Reconcile flow step receipts and reservation indexes atomically. Deferred nodes block provider/workspace/policy widening until settled or canceled; routine tasks carry a narrow routine source and real same-conversation dependencies.
- Give routine execution tokens a separate non-serializable principal field; deny legacy REST/MCP and every automation operation except granted `routines.run`.
- Complete exact-loopback OAuth callback routing before browser metadata/cookie gates. State is claimed once before exchange; encrypted token persistence is atomic with its flow, logout cancels owned jobs and removes pending PKCE material, and callback success redirects locally without retaining codes in task events.
- Integrate external MCP calls with the live active-attempt broker, immutable server/binding/tool/credential pins and exact one-call console approval. Revalidate before dispatch, cancel on configuration/auth revocation and preserve uncertain effects. A real loopback HTTP fixture exercised model-adapter → approval → MCP → result completion without exposing administrator or inference credentials upstream; no paid/native model turn was used.
- Implement Codex dynamic-tool protocol framing and strict thread/turn/catalog checks against the installed app-server schema. Profile automation remains disabled: omission of native apply-patch and rooted-only native reads are not verified. Protocol fixtures are not evidence of native confinement; legacy Codex behavior is unchanged.
- Gate OMP profile startup on installed `omp/18.12.1` protocol v2 and an exact host-tool roster, preserving native tool denial. Register and clear tools per turn, dispatch host calls asynchronously, and honor cancellation. The real no-model startup/set/clear preflight passes; authenticated model/tool turns are not claimed.
- Gate Claude profiles on SDK `0.3.303`, native `2.1.303`, managed-settings exclusion and exact scoped MCP tools, with persisted native-session acknowledgement before calls. The installed no-model check passes. Keep OpenCode `1.19.3` profile automation disabled because ambient configuration/plugin installation precedes enforceable restrictions; preserve its legacy behavior.
- Keep operator-only memories out of routine prompts and connector conversations, including local follow-ups in existing connector threads.
- Route ACP filesystem requests through relative rooted broker paths and terminal requests through the approved confined command broker, preserving bounded output, cancellation and nonzero exit status.
- Add console surfaces for the new automation capabilities: an Automation destination with queue, routine, flow, skill and template views, profile management with an explicit capability set, console-only approval and question decisions with the exact reviewed action, and operator controls for exact network origins, provider connections and confined-runtime readiness. Legacy views keep their behaviour and every call keeps the session cookie and CSRF handling.

### Developer Console (v0.4)

- Add **New folder** to the workspace folder chooser: create a single child directory, open it for explicit selection, and show invalid-name, conflict and filesystem errors without overwriting existing entries.
- Add a production-embedded React/TypeScript/Vite console with Tailwind tokens, locally owned shadcn/ui Radix primitives, Lucide icons and TanStack Query.
- Make Chat the default: named-agent selection, exact recipients, durable task timelines, SSE replay/resync, saved drafts/send keys, bounded history, context attachments and execution provenance.
- Add a localhost-only human-operator session with HttpOnly/SameSite cookies, absolute expiry, CSRF protection, strict Host/Origin/Fetch Metadata checks and CSP; retain legacy bearer API browser restrictions.
- Route human messages through existing admission, idempotency, scheduler and context ACLs without registering a fake harness. Report partial multi-recipient admission explicitly.
- Add searchable Agents/Tasks/Contexts/Activity, real lifecycle controls, context publication/grants, task deep links, denial-only approval evidence and explicit retry confirmation.
- Add owner-checked conversation rename/participant removal and schema-5 query indexes; preserve v0.1–v0.3 messaging/session/recovery contracts.
- Validate desktop/mobile/light/dark accessibility, keyboard/IME behavior, 1,005-message pagination, authentication boundaries, and real OMP/Codex/OpenCode browser chat; publish measured WSL2 evidence and screenshots.

### Agent collaboration (v0.3)

- Add v0.3 persistent conversations, exact-agent membership, filtered context bindings and channel lifecycle events.
- Add transactional schema migrations 003/004, immutable SHA-256 SQLite artifacts, private context versions, explicit grants/revocation, metadata search and append-only access audit.
- Extend durable tasks with pinned context refs, immutable dependencies, ready/blocked inspection and immutable result refs; retain v0.1/v0.2 routes and task states.
- Reauthorize context as the recipient before adapter delivery, enforce configurable byte/fan-out limits, and inject bounded untrusted JSON envelopes across all adapters.
- Add collaboration HTTP/CLI/MCP operations and authorized immutable MCP resources; preserve repository/session serialization and uncertain-execution manual reconciliation.
- Add native three-harness validation, transport authorization/restart/migration regressions, WSL2 resource sampling, design/runbook and evidence report. Claude provider verification is waived; no four-harness native claim.

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
