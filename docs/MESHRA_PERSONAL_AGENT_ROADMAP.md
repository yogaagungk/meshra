# Meshra personal agent platform: implementation roadmap

Status: in progress. P0 complete; P1 partial (restricted Direct API enforced; restricted Claude and OMP implemented but unproven on a real worker); P2 partial; P3 partial (compaction only); P4 offline work done; P5 partial (event input, missed-run policy). P6–P10 and all live-account acceptance pending. See section 11.
Prepared: 2026-11-05.
Audience: the next implementation session and subsequent maintainers.

## 1. Product objective and scope

Build on Meshra, not a replacement project. Meshra is an open-source, local-first control plane for personal agents: the owner configures persistent specialists, selects their runtime, and delegates coding or business workflows under explicit authority. Keep one application and repository initially. Separate execution authority; extract repositories only when independent deployment, releases, or reuse justify it.

The requested end state includes all of the following:

- Build and verify code on the owner's machine using Claude Code, OMP, or direct-provider execution.
- Configure agents with durable identity, instructions, approved knowledge, private memory, and scoped conversation history.
- Monitor email, inspect and manage Meta ads, prepare and publish social posts, and handle customer conversations through WhatsApp.
- Run reviewed workflows on schedules and incoming events, with useful event input, predictable knowledge access, durable receipts, and explicit recovery.
- Provide a direct API harness, including DeepSeek, with a common coding/tool capability contract. Do not promise identical native features or equal model quality.
- Publish reusable code, examples, support boundaries, installation instructions, and honest verification evidence without personal data or credentials.
- Compare outcomes against Hermes Agent and OpenClaw rather than copying their entire feature inventories.

“Local-first” means local ownership of orchestration/state; selected content still leaves the machine when cloud inference or external services are used. Make those disclosures visible.

### Scope boundaries

This roadmap covers the objectives and gaps discussed in the review conversation. It does not add a hosted service, multi-user accounts, marketplace, desktop/mobile clients, arbitrary new harness protocols, or a general plugin installer. Browser, desktop, and voice expansion from older plans is not a prerequisite for these API-based workflows. Do not reimplement already-working queues, attachments, skills, templates, profiles, or the scheduler merely because they appear in an older plan.

Use Gmail as the initial concrete email implementation; other mail providers are extensions, not requirements for completion here. Use Meta Marketing API for ads, Facebook Pages and supported Instagram professional accounts for social publishing, and the official WhatsApp Business Cloud API for support. These are recommended implementation targets that make the previously generic examples executable, not claims that the owner has those accounts. Do not add unrelated social networks or personal-account scraping. Account eligibility, scopes, quotas, API versions, and provider policies must be checked against current official documentation during implementation.

## 2. Source of truth and current baseline

Current code wins over historical plans. This roadmap supersedes conflicting implementation directions in `MESHRA_AUTOMATION_PLAN.md`, `DIRECT_API_AGENT_PLAN.md`, and `DIRECT_API_CACHING_PLAN.md` for this scope, but does not declare their implemented features missing. Their status headers and historical sections can disagree. Reconcile them in Phase 0; do not execute those documents blindly.

### Existing foundations to preserve

- Go daemon/CLI, SQLite WAL, React/TypeScript embedded console, loopback single-administrator boundary.
- Canonical agent identities, immutable task/session ownership, durable admission and correlation, repository serialization, asynchronous delegation.
- Portable profiles, conversation-specific bindings, admitted policy/provider snapshots, scoped memory/history, memory review, local skills/templates.
- Broker approvals, private workspace copies/changesets, Linux confinement, exact-origin network policy, encrypted automation secret vault.
- Routines and DAG flows with durable run/step receipts; Slack, Discord, and Telegram connector inbox/outbox handling.
- Direct API agent loop, streamed OpenAI-format replies, bounded tool dispatch, usage/cache accounting, and existing compaction.

### Evidence anchors from the review

Line numbers can drift; locate symbols before editing.

| Current observation | Existing source anchors | Consequence |
|---|---|---|
| Every nondraft OMP launch selects YOLO; Claude allows nondraft tools with permission bypass | `internal/adapters/omp/omp.go`, `bridges/claude-agent-sdk/bridge.mjs`, `docs/security.md` | Managed profiles are not broker-only; capability-off is not native-tool confinement |
| Profile command/write enablement validates broker confinement, despite unrestricted native execution | `internal/model/profiles.go`, `internal/relay/profiles_store.go` | Resolve the authority mismatch, not just labels |
| Broker actions already have approval/context/revision checks | `internal/relay/tool_broker.go`, `internal/relay/mcp_client_broker.go` | Extend this boundary, not a parallel authorization engine |
| Direct API messages/deltas omit DeepSeek reasoning state | `internal/adapters/openai/adapter.go`, `internal/adapters/openai/stream.go` | Thinking-mode tool continuations do not satisfy the documented provider contract |
| Compaction replaces old tool results before requesting a summary | `internal/adapters/openai/compact.go` | Summary cannot preserve evidence already discarded |
| Memory uses pinned/recent order and an 8 KiB injection budget | `internal/relay/profile_memory.go` | Stored facts can be omitted without relevant retrieval |
| Memory budget/dedup checks depend on write-review path | `internal/relay/profile_memory.go`, `internal/relay/profile_memory_pending.go` | Apply intended storage invariants regardless of review mode |
| Routine nodes use new external-classified conversations | `internal/relay/flows.go` | Private operator memory is correctly excluded, but reusable approved knowledge is missing |
| Webhook body is authenticated/hashed, not admitted as task input | `internal/relay/routines.go`, `internal/relay/flows.go` | Events trigger fixed instructions rather than payload processing |
| Final task transition error is ignored | `internal/relay/tasks.go`, `Relay.execute` | Effects can complete without a durable terminal acknowledgment |
| Connector kinds are Slack, Discord, Telegram | `internal/connectors/connectors.go` | Email, ads, social publishing, WhatsApp are not established integrations |
| External MCP calls require exact one-call console approval | `internal/relay/mcp_client_broker.go`, `internal/relay/mcp_client.go` | No safe unattended MCP authorization contract yet |

The review built the source and passed the credential-free `scripts/smoke.mjs` using a temporary executable and state directory. It exercised restart, deduplication, identity/correlation, timeout/cancellation, authentication, context denial, and MCP stdio tracing. Provider turns: zero. It did not verify live coding, DeepSeek, confinement against hostile native tools, business accounts, or visual UI behavior. Those are pending evidence, not failed checks.

## 3. Architecture decisions

### A. One control plane, separate execution domains

Meshra owns profiles, memory authorization, admission, policy, scheduling, approvals, audit, and outcome presentation. Workers execute admitted tasks; they cannot create authority or approve themselves.

- `trusted-coding`: operator-selected project access; native harness tools may run with explicitly acknowledged broad authority. Never presented as sandboxed merely because a broker exists.
- `restricted-automation`: no ambient host shell/filesystem/network authority; only the admitted catalog, approved knowledge, and narrowly scoped integration operations.
- New profiles default to restricted execution with external effects disabled. Trusted mode requires an explicit operator decision; a model, skill, imported template, connector, or routine cannot enable it.
- External-message and unattended business admission must reject trusted-coding destinations. A connector cannot delegate through another profile to escape this restriction.
- Preserve existing profiles/identities/history. Classify existing native behavior truthfully during migration; do not silently reinterpret it as restricted. Require review before enabling old external routes/routines whose destination remains unrestricted. Keep their accepted events visible, with an actionable blocked reason and normal deadline semantics.
- Active work must be drained or explicitly cancelled before a mode change. Queued tasks retain admitted mode/policy; incompatible old admissions are blocked for review, not silently reauthorized or sent to another provider.

Do not turn the relay into a distributed system. Keep worker supervision in the current daemon and launch a distinct process where enforcement requires it. Reuse the sandbox/process/broker machinery. A subprocess with the same host access is not an isolation boundary.

### B. Restricted native execution must be proven, not inferred

Restricted Claude/OMP needs both exact native-tool suppression and confinement of the harness process itself. Isolate HOME/state, mounts, credentials, network, child processes, and permitted broker channels. Verify the effective tool roster and supported protocol/version before a model turn. Do not mount the daemon database, admin token, vault key, unrelated native sessions, host service sockets, or broad home directory.

Prefer a scoped inference/credential broker so model credentials do not become general tool credentials. If a native harness requires an auth artifact, expose only the selected provider credential in the confined harness environment; do not expose it to tool workers. Document that residual provider-credential access accurately. Missing confinement/auth compatibility means restricted mode is unavailable for that harness, never host fallback. The phase is incomplete until restricted Claude and OMP each have actual proof on a supported version.

### C. Shared contracts, not a second platform

Extend the existing task snapshot and broker contracts to carry execution mode/revision, authorized knowledge references, integration connection/operation revision, target restrictions, and effect authority. Derive identity/task/source from trusted state, never model arguments. Reuse `Runtime.Run`, `ToolExecutor`, existing admitted snapshots, action hashes, secret references, and immutable artifacts where practical.

Separate logical identity from runtime: provider switches preserve the profile and authorized knowledge, create fresh execution bindings, disclose the bounded handoff, and never merge private conversations or transfer old task ownership.

### D. Effects need explicit policy

Distinguish reads, drafts, external sends/publication, and financial/account mutations. Default: reads require an explicit bounded grant; external mutations require exact human approval. Optional unattended operation requires an operator-authored, revocable, expiring policy with precise operations, target IDs, limits, and audit. A generic tool's `readOnly` annotation is not proof of safety.

A policy is not a credential: check both at dispatch. Revalidate revocation and applicable revisions immediately before the external call. Approval binds exact content, target, media, account, and action revision; editing invalidates it. Agents cannot widen scope or approve their own proposals.

### E. Integration packaging

Initially implement integrations as ordinary internal modules in this repository using small typed contracts. Do not build a package marketplace or mandatory microservice framework. Prefer official APIs for the selected workflows. External MCP remains supported, but arbitrary MCP tools do not inherit unattended rights: allow that only for a reviewed operation/schema mapped to enforceable constraints; otherwise keep exact manual approvals.

## 4. Coverage checklist

All items below are required within this roadmap, not optional phase suggestions. Start unchecked; mark complete only with acceptance evidence.

| ID | Deliverable or gap | Phase |
|---|---|---|
| R01 | Product definition and truthful support/security documentation | P0, P10 |
| R02 | Explicit trusted/restricted modes, migration, admission/delegation enforcement | P1 |
| R03 | Actual restricted worker isolation and Claude/OMP enforcement | P1 |
| R04 | Durable terminal acknowledgment and safe uncertain-effect handling | P2 |
| R05 | DeepSeek reasoning/tool compatibility and model configuration | P3 |
| R06 | Evidence-preserving compaction and recoverable artifacts | P3 |
| R07 | Common coding capability contract, live coding verification, truthful cache/usage evidence | P3, P10 |
| R08 | Relevant memory retrieval and visible selection/omission | P4 |
| R09 | Approved cross-run knowledge; private/customer isolation; reset/delete semantics | P4 |
| R10 | Consistent memory quotas/deduplication and durable write review | P4 |
| R11 | Immutable usable webhook/event input | P5 |
| R12 | Explicit downtime/catch-up, backlog, startup, and schedule outcomes | P5 |
| R13 | Scoped unattended policy for reviewed integrations/MCP, revocation and limits | P6 |
| R14 | Email monitoring, summaries, checkpoints, and owner delivery | P7 |
| R15 | Meta ads reporting and bounded management | P8 |
| R16 | Social drafting, scheduling, approval, and publishing | P8 |
| R17 | WhatsApp customer conversations, knowledge, escalation, delivery | P9 |
| R18 | Unified console/CLI controls and inspectable task/effect/knowledge history | P1–P10 |
| R19 | Credential lifecycle, prompt-injection containment, duplicate-safe effects | P1, P2, P6–P9 |
| R20 | Open-source packaging, licensing, private-state hygiene, reproducible installation | P10 |
| R21 | Outcome-based Hermes/OpenClaw comparisons and per-runtime support matrix | P10 |

## 5. Phased implementation

Each phase is an end-to-end vertical slice: schema/service, adapter or worker, API/CLI, console where applicable, behavioral tests, actual smoke, docs, changelog. A backend scaffold or UI with no working backing behavior does not complete a phase. Preserve existing patterns and avoid unrelated restyling.

### P0 — Reconcile product and existing plans

Dependencies: none.

Work:
- Recheck the evidence anchors against current code, migrations, and installed versions. Inventory implemented versus unverified behavior; do not rerun user-reported failures just to confirm them.
- Update `PRODUCT.md` to include personal business automation alongside coding, with the two trust domains and local-first/cloud-disclosure distinction.
- Reconcile `README.md`, `docs/security.md`, `docs/adapters.md`, `docs/PERSONAL_WORKSPACE.md`, `docs/API_UI_CONTRACT.md`, and the three prior plans. Mark historical assumptions explicitly; do not rewrite historical evidence as if new verification occurred.
- Identify source-of-truth mode/policy/task-snapshot types and all callers with language-server references before exported-symbol changes. Allocate migrations from the actual current maximum; never reuse historical plan numbers.

Acceptance:
- No current-facing documentation claims all native tools are broker-confined before P1 proves it.
- A capability inventory names actual backend, enforcement boundary, and evidence level for Claude, OMP, and Direct API.
- Existing data/workflows have an explicit migration path; no replacement scheduler or new repository is introduced.

### P1 — Make execution authority consistent and enforceable

Dependencies: P0. Release gate for every business integration.

Primary anchors: `internal/model/profiles.go`, `internal/model/model.go`, `internal/relay/profiles_store.go`, `profiles_bindings.go`, `profiles_send.go`, `tasks.go`, `flows.go`, `connectors.go`, `internal/adapters/omp/omp.go`, `internal/adapters/claude/`, `bridges/claude-agent-sdk/bridge.mjs`, `internal/sandbox/`, `internal/netpolicy/`.

Work:
- Persist explicit mode/revision on profiles and admitted tasks; enforce it in direct sends, backing-agent lifecycle paths, flows, connector routing, and nested delegation. Separate mode selection from ordinary tool capability settings.
- Preserve operator-driven full-access coding with persistent warnings and explicit acknowledgment. Restricted work must not enter that path, including via legacy APIs.
- Supervise confined workers with task/attempt-scoped credentials, private state, narrow channels, output/resource bounds, and process-tree cleanup. Worker completion is a report to the relay, not permission to mutate task ownership or policy.
- Disable native filesystem/shell/subagent/foreign-tool escape paths in restricted Claude/OMP; verify exact catalogs and enforce OS containment. No unrestricted host fallback on protocol, namespace, cgroup, or authentication failure.
- Migrate unsafe existing external routes/routines to blocked-for-review configuration without deleting accepted work. Expose blocked reasons and remediation.
- Extend `web/src/Profiles.tsx`, `ProfileChat.tsx`, `MessagingConnections.tsx`, and approval/runtime surfaces to display mode, effective operations, confinement readiness, and the source of authority. CLI/API must expose equivalent factual status.

Acceptance and smoke:
- Trusted coding can edit an explicitly selected disposable project; the UI reports full-access authority accurately.
- Restricted Claude and restricted OMP each complete an approved broker task on supported installed versions.
- Denied native reads/writes/commands, foreign MCP, delegation to a trusted profile, host-secret access, unauthorized egress, and child-process escape are actually blocked. Include adversarial inbound/tool-output instructions.
- Missing runtime prerequisites and revoked channels fail closed without losing task provenance. Cancellation kills descendant work and never claims rollback.
- Existing task IDs/history remain unchanged; queued old authority is not silently widened; direct API restricted tasks share the same policy enforcement.

### P2 — Close acknowledgment and external-effect recovery gaps

Dependencies: P0; can proceed independently of P1 after shared contracts are fixed.

Primary anchors: `internal/relay/tasks.go`, `reliability.go`, `tool_broker.go`, `connectors.go`, `internal/connectors/`, task/receipt migrations.

Work:
- Handle final transition persistence errors explicitly. Separate “execution returned” from “terminal acknowledgment is durable.” Stop affected dispatch rather than silently returning the identity to healthy idle state; keep memory/resource retention bounded. Retry persistence only where safe, never reexecute the model/tool to repair bookkeeping.
- Reuse/extend invocation and connector receipts for external effects: stable local action identity, target, request digest, policy revision, provider idempotency reference when supported, provider object/message ID, and dispatch/acknowledgment/uncertainty status.
- Persist an effect intent before dispatch. Distinguish definite rejection (not executed) from unknown transport outcome. A local receipt is not provider-side exactly-once execution.
- Reconcile provider state by supported request/object IDs where possible; unresolved outcomes require operator inspection and explicit duplicate-risk confirmation before retry.
- Display effect status separately from overall LLM/task status; a failed continuation must not hide a successful publication or ad change. Do not invent rollback.

Acceptance and smoke:
- Inject a storage failure at terminal persistence after a successful disposable action; no false durable-success claim, unsafe redispatch, or silent healthy-idle transition occurs.
- Kill/restart the daemon after effect intent and after simulated provider acceptance. Known completed effects are not repeated; ambiguous outcomes remain visible and non-replayed.
- Provider-native idempotency is used only when the selected endpoint supports it; the no-idempotency path remains honest.
- Existing connector admission deduplication, uncertain delivery, and explicit retry behavior remain intact.

### P3 — Complete direct-provider coding behavior

Dependencies: P0; P1 for restricted coding proof; P2 for accurate partial-effect outcomes.

Primary anchors: `internal/adapters/openai/adapter.go`, `stream.go`, `anthropic.go`, `compact.go`, `request_stats.go`, `internal/relay/chat.go`, `profiles_send.go`, `connections.go`, `web/src/DirectApiFields.tsx`, `Connections.tsx`, `Stats.tsx`, `AIStatus.tsx`.

Work:
- Implement typed, provider-aware DeepSeek reasoning settings and continuation state for both streamed and nonstreamed replies. Preserve the fields required by the selected API/model without forwarding arbitrary request bodies or exposing raw reasoning in general logs/UI.
- Verify current official model/protocol requirements, including reasoning passback, thinking toggles, tool support, context/output bounds, finish reasons, and usage/cache fields. Do not hard-code model names from this plan. Ordinary OpenAI/Anthropic-format connections must remain correct.
- Decide explicitly how required reasoning/tool state survives or is deliberately omitted at session boundaries. Do not replay an invalid partial transcript. Store only necessary sensitive state with appropriate ownership/retention, or use a verified fresh-turn protocol where supported.
- Fix compaction order: retain original tool evidence until a valid bounded summary exists; summarize before truncation, preserve latest complete tool exchanges and essential source references, and retain exact recoverable artifacts. Failed summaries do not destroy evidence. If context cannot fit, stop visibly rather than corrupting pairing or silently fabricating recall.
- Preserve existing system/history separation, tool limits, validation, repeated-call handling, sequential mutations, and safe parallel reads. Verify cancellation, provider errors, truncated output, malformed tools, unsupported features, and continuation failure after a successful action.
- Define a common capability matrix: inspect/search, edit, command, results, context recovery, memory retrieval, cancellation, policy, and durable outcome evidence. Mark native-specific capabilities separately; do not equate capability support with equal model performance.
- Reuse existing cache support. Verify stable prefixes and provider-reported cache/read/write accounting, record cold compaction boundaries, and keep unknown tokens/costs unknown. Cache hits are an optimization, not a correctness condition.

Acceptance and smoke:
- Against a live DeepSeek account/model, complete a multistep tool loop in each supported thinking configuration; required reasoning survives continuation, including streamed responses.
- An actual disposable coding task reads/searches source, edits it, runs the project check, and reports observed results. Repeat the common scenario with Claude and OMP; record runtime/version/model and permissions, not an unsupported parity claim.
- Under context pressure, an important earlier diagnostic is recovered from the summary/artifact and influences the correct next action; broken/missing summaries fail safely.
- A completed tool action followed by provider failure is still inspectable as an effect, not reported as never executed.
- Live cache measurements are recorded where credentials/provider thresholds permit; absence of a hit is reported, never fabricated. Provider-specific fixtures are protocol coverage, not live-provider proof.

### P4 — Make persistent knowledge useful without weakening privacy

Dependencies: P0; P1 before external use.

Primary anchors: `internal/relay/profile_memory.go`, `profile_memory_pending.go`, `profile_history.go`, `profile_session_search.go`, `profiles_context.go`, `profile_rotation.go`, `profiles_send.go`, `flows.go`, `web/src/Profiles.tsx`, `Journey.tsx`, `ProfileChat.tsx`.

Work:
- Preserve existing operator-private and conversation scopes. Add an explicit operator-reviewed reusable knowledge scope/binding usable by selected routines and external agents. External content cannot promote itself into owner/business policy or cross-run authoritative knowledge.
- Make customer/account/thread ownership part of retrieval authorization. Apply ACLs before ranking; relevance never overrides scope. Provider switching preserves authorized knowledge without merging conversations.
- Add bounded searchable memory retrieval using existing SQLite/FTS patterns before introducing embedding infrastructure. Select by task relevance plus pinned knowledge, with stable tie-breaking, provenance, freshness, and explicit byte/token budgets. Full text remains available only through authorized retrieval.
- Explain what was injected/retrieved/omitted and why. “Saved” must not imply “included in every future prompt.” Pinned overflow must be visible; no secret or irrelevant memory should be injected just to fill a budget.
- Enforce memory byte/count/per-entry quotas and normalized duplicate rules consistently with review on/off and with operator/model writes. Define how existing over-limit data is retained and corrected; never discard it silently.
- Keep staged writes and operator promotion/edit/reject actions transactional and revisioned. Untrusted writes stay proposed or appropriately conversation-scoped; approved knowledge changes do not mutate already-admitted task snapshots unexpectedly.
- Clarify reset versus forget versus archive versus delete. Reset clears future transcript use according to existing epochs; forgetting removes retrieval eligibility. Explain retained audit/backups/native-session limits instead of claiming secure erasure.

Acceptance and smoke:
- An older relevant note is recalled despite newer irrelevant notes exceeding the prompt budget.
- A fresh scheduled run can use specifically approved agent knowledge, but cannot read the owner's private notes or another customer's facts.
- Prompt injection requesting memory promotion/cross-customer recall fails at authorization, not merely through a model refusal.
- Review on/off produces the same storage limits and duplicate behavior. Stale revisions cannot overwrite edits.
- Reset, forget, provider switch, conversation deletion, and archive have distinct, documented, exercised outcomes.

### P5 — Make schedules and event inputs operationally complete

Dependencies: P2, P4; restricted execution gated by P1.

Primary anchors: `internal/relay/routines.go`, `flows.go`, `internal/api/webhooks.go`, attachment/artifact handling, `web/src/Automation.tsx`, `AutomationRuns.tsx`, `AutomationFlows.tsx`.

Work:
- Persist bounded immutable event envelopes atomically with accepted webhook receipts: source/tenant/event ID, verified timestamp, schema/version, payload reference/digest, and data classification. Validate size/content before acceptance; encrypt or protect sensitive payloads according to the established storage model.
- Keep provider signature verification outside the model. Differentiate provider verification from Meshra routine HMAC; an arbitrary provider webhook cannot automatically satisfy the existing routine protocol.
- Admit selected validated fields or authorized artifact references as quoted untrusted task input. Do not interpolate payload into instructions, select authority from it, or let model text alter routing/policy.
- Preserve accepted-event/checkpoint durability, conflicting duplicate-ID rejection, retention, and access controls. Record how a run obtained its input and approved knowledge.
- Add explicit per-routine missed-run policy: default skip, optional bounded catch-up with maximum age/count and overlap/backlog limits. Persist occurrence identities so restart/DST/timezone changes cannot duplicate financial/publication actions.
- Preflight runtime, credentials, target permission, knowledge, and delivery readiness before spending tokens. Expose needs-auth, runtime-unavailable, policy-blocked, skipped, overdue, approval-waiting, and uncertain outcomes. Do not silently auto-start unsafe legacy agents.

Acceptance and smoke:
- Submit a signed event containing a lead/message identifier; the run consumes exactly that input. A changed body under the same event ID is rejected.
- Invalid signatures, oversized payloads, untrusted policy instructions, and unauthorized payload reads are denied.
- Restart through webhook receipt/task admission boundaries; one accepted event does not produce duplicate admissions/effects.
- Exercise skip and bounded catch-up across downtime, overlap, backlog, and a timezone/DST boundary. The console explains each outcome and the next scheduled occurrence.

### P6 — Add bounded unattended integration authority

Dependencies: P1, P2, P5.

Primary anchors: `internal/relay/tool_broker.go`, `tool_actions.go`, `mcp_client.go`, `mcp_client_broker.go`, `secrets.go`, `internal/netpolicy/`, `web/src/ApprovalRules.tsx`, `Approvals.tsx`, `Connections.tsx`.

Work:
- Define a small typed operation contract for integrations: operation/schema version, connection/account, target IDs, read/draft/send/publish/financial effect category, credential requirements, input/output limits, policy evaluator, and receipt/reconciliation behavior.
- Extend existing approval policy rather than adding a second rule engine. Grants specify exact operations/targets, expiry, run/time-window counts, and relevant monetary/content limits. Default denies unattended external mutations; owner reviews and enables each bounded case.
- Reserve count/spend allowance transactionally before dispatch so concurrent runs cannot exceed limits. Settle or retain reservations based on known rejection/success/uncertainty; never free an uncertain financial reservation as if nothing happened. Use currency-aware exact monetary representation.
- Keep unknown MCP operations manually approved. Allow unattended MCP only when a reviewed exact tool/schema is mapped to enforceable typed semantics; schema/server/binding changes invalidate that permission. A server's advisory annotations cannot authorize spending or public posting.
- Revalidate policy, connection, credentials, owner/session rules, and target immediately before dispatch. Distinguish persistent operator grants from a logged-in browser's one-call approval; ordinary logout must not accidentally authorize work or ambiguously redefine durable grants.
- Support least-scope credentials, refresh serialization, expiry/revocation, needs-auth state, and safe error redaction. Workers do not receive unrelated secrets; clear global pause/revoke controls block future effects and explain in-flight uncertainty.

Acceptance and smoke:
- An explicitly granted unattended read succeeds without a live console; an ungranted send/publish/spend is denied.
- An exact mutation approval becomes invalid when content/target changes. Policy revocation blocks pending dispatch; already-dispatched effects remain honestly classified.
- Concurrent actions cannot exceed configured count/spend limits. Unknown results retain appropriate reservations.
- A changed MCP schema or prompt requesting broader permission cannot bypass the grant. Agent/routine/connector credentials cannot create grants or approve calls.

### P7 — Deliver email monitoring end to end

Dependencies: P1–P6. First business vertical slice.

Work:
- Implement Gmail OAuth using minimum read scopes; never request mailbox passwords in chat. Store tokens in the existing vault, support refresh/revoke, and display the authorized account/scopes without leaking tokens.
- Use bounded periodic incremental reads/checkpoints initially so local operation needs no public webhook. Handle initial baseline, pagination, cursor expiry/resync, rate limits, backoff for safe reads, attachments, and cancellation. Advance durable checkpoints only after accepted events are stored.
- Convert new messages into stable, account-scoped event input. Treat sender names, subject/body/HTML, attachments, and links as untrusted; remote images/links do not auto-execute or fetch. Thread and tenant identity must come from verified API data.
- Provide owner-reviewed rules for monitored labels/filters, summary schedule, relevant knowledge, and delivery target. Deliver summaries to Meshra and optionally an already-configured owner connector with a durable receipt.
- Keep outbound email disabled for the monitoring workflow. Do not invent email-send scope simply because other integrations can send.

Acceptance and smoke:
- A real authorized test mailbox receives a harmless test message; Meshra ingests it once, produces a correct summary, and delivers it to the configured owner surface.
- Restart after ingestion, re-fetch the same message, and expire/resync the cursor without duplicating the summary action.
- Malicious email instructions cannot obtain shell access, private memories, credentials, or outbound-send authority.
- Missing/expired/revoked credentials are actionable; other accounts' messages are inaccessible. Record whether summary content was shared with a cloud provider.

### P8 — Deliver Meta ads and social publishing

Dependencies: P1–P6; may proceed alongside P7 after shared operation contracts stabilize. Keep ads and publication operations separately permissioned.

Work — ads:
- Implement official Meta Marketing API connection setup, current version/scope/asset checks, ad-account allowlists, token lifecycle, and bounded paginated insights reads.
- Report spend/performance with source, date range, timezone, attribution setting, currency, and freshness. Do not compare incompatible windows or invent missing metrics.
- Implement concrete management operations: pause/resume a selected campaign/ad set/ad and change a selected budget. Preview exact object/current state/proposed change; enforce per-account/object limits, maximum increases and currency rules outside the model.
- Require human approval by default; unattended changes need a specific P6 policy. Query current provider state where necessary, handle concurrent changes, retain receipts, and reconcile uncertain updates.
- Distinguish a budget-setting limit from an actual-spend guarantee: upstream spend can continue between checks. Describe what each enforced limit controls; do not advertise a perfect real-time spend cap.

Work — social:
- Support Facebook Page and eligible Instagram professional-account publication using current official APIs and approved assets. Implement draft text/media, preview, revisioned approval, scheduled publication, and provider receipt/permalink/status.
- Validate provider media requirements and access before approval/dispatch. If provider fetching requires public media URLs, use an explicitly configured, least-exposure storage/publishing mechanism with bounded expiry; never expose the local data directory or admin service. Show this prerequisite and data disclosure.
- Bind approval to exact content, media hashes, account, and schedule; edits invalidate approval. Revalidate auth/policy at publish time and confirm provider outcome before reporting publication.
- Handle rate limits, rejected/processing media, missed schedules, cancellation, and unknown publish outcomes without blind reposting. Reuse routine/effect receipts rather than a second scheduler.

Acceptance and smoke:
- Against authorized test assets, fetch real insights and verify reported units/time windows against provider responses.
- Pause/resume and change a harmless test budget with explicit approval; reject out-of-scope accounts and over-limit changes even when the model requests them.
- Publish approved harmless content to each supported destination, record its real provider ID/status, and verify actual visibility. Declined/edited/unapproved content never publishes.
- Exercise restart at publication acknowledgment and stale approval boundaries without an automatic duplicate post. An unavailable account type/media prerequisite is a named blocker, not a fake-success fallback.

### P9 — Deliver WhatsApp customer support

Dependencies: P1–P6, P4 knowledge isolation; reuse provider connection primitives from P8 where appropriate.

Work:
- Use official WhatsApp Business Cloud API, not an unofficial personal-account bridge. Implement authorized business/phone-number setup, sender/recipient routing, current message-window/template rules, opt-in requirements, token lifecycle, rate limits, and delivery/read/failure status events.
- Keep Meshra's daemon/admin API loopback-only. Official inbound delivery needs a publicly reachable HTTPS webhook. Supply an optional minimal operator-deployed ingress component in this repository, separate from the daemon: verify Meta signatures, enforce bounds, durably queue encrypted payloads, and expose only authenticated owner-scoped outbound polling/acknowledgment to local Meshra. Never forward admin credentials or give ingress general task/approval access. Review encryption/key ownership and retention before deployment; remote infrastructure is explicitly operator-managed, not a Meshra cloud service.
- Implement provider verification challenge and event validation at ingress; persist locally before advancing/acking the ingress cursor. Deduplicate by provider event/message ID. Inbox messages and status events are different event types.
- Bind conversations to verified business account/phone/customer/thread identity. No cross-customer recall or broad owner-memory access. Supply approved business knowledge and explicit support instructions.
- Add reply modes: draft/approve by default, optional bounded automatic reply policy for selected topics/recipients. Unsupported promises, payments/refunds, sensitive requests, low-confidence answers, or policy exceptions escalate to a human.
- Provide human takeover/pause and re-enable controls; while paused, no agent reply races the operator. Persist ownership/revision with pending sends and revalidate before dispatch.
- Reuse outbox/effect receipts for replies and status transitions; distinguish API acceptance, delivery, and read status. Define uncertain-send handling and explicit duplicate-risk retry.

Acceptance and smoke:
- From a real authorized test number, receive a harmless inbound message, retrieve only its approved knowledge/customer context, and send an authorized reply in the same conversation.
- Verify actual receipt on the test handset and provider delivery status; a model-generated reply or HTTP acceptance alone is not delivery proof.
- Duplicate webhook/status events and daemon/ingress restarts do not cause duplicate replies. Unauthorized sender/account, invalid signatures, expired windows, and missing templates fail visibly.
- Human takeover stops pending automated responses; prompt injection and another customer's question cannot access coding authority or private knowledge.
- Deploy/run the real ingress path and verify its narrow public interface and outbound local poll. Without eligible account/HTTPS infrastructure, record the exact missing prerequisite; do not mark the integration complete.

### P10 — Finish open-source release and outcome benchmarks

Dependencies: P1–P9 for a release claiming the entire roadmap. Can prepare documentation/packaging earlier without claiming readiness.

Work:
- Preserve MIT licensing for original Meshra code. Audit dependency, bundled native binary, browser/model asset, bridge, and copied-example licenses; update existing third-party notices. Do not copy benchmark code/assets merely because their product ideas are useful. Proprietary harness adapters do not grant redistribution rights to the harness.
- Make installation reproducible from a clean machine using actual `go.mod`, Node/build requirements, lockfiles, embedded UI, bridge paths, optional confinement setup, and operator-installed harnesses. Do not rely on the developer's existing home directory or cached artifacts.
- Keep private memories, credentials/tokens, mail/customer content, native sessions, task databases, account IDs, and provider webhooks out of source/example fixtures and release artifacts. Supply sanitized examples that start inert with no effects/credentials.
- Document security boundaries, supported versions/providers/destinations, required paid accounts/infrastructure, scopes, recovery, backup/restore of database plus vault key, revocation, retention, and vulnerability-reporting/contribution paths. Verify encrypted-state restore; a database-only backup is not sufficient for vault recovery.
- Reconcile all current product/security/API/CLI docs and plan statuses. Clearly distinguish implemented, deterministic-tested, actual-runtime-smoked, live-provider-verified, and unsupported behavior. No global “production-ready” claim based on fixture tests.
- Add reusable benchmark scenarios: real code fix with a failing-before/passing-after project check; fresh-session and scheduled-run recall; duplicate event; downtime policy; crash after send; malicious inbound request; ads-limit violation; customer isolation.
- Compare Meshra with Hermes Agent/OpenClaw using pinned versions, matching model/provider where possible, comparable permissions, identical tasks/input, and declared environment differences. Report task completion, human interventions, observed latency, reported usage/cost coverage, and unauthorized effects. Use neutral tasks; do not penalize a product for intentionally absent features without disclosing that scope.
- Maintain per-runtime capability/evidence matrix. Claude/OMP native-only conveniences remain marked native-specific; absence of credentials means “not measured,” not an invented ranking.

Acceptance and smoke:
- A clean installation reaches a useful credential-free demo and diagnostic report; restricted runtime absence is explicit, not host fallback.
- An operator configures a profile/knowledge/routine from sanitized examples without editing core source. Examples cannot silently grant external effects.
- Backup/restore preserves profile/memory/task ownership and usable encrypted credentials under documented key handling.
- All live workflows claimed in the release have evidence; unmeasured benchmark cells remain unmeasured. Packaging/license checks and private-state hygiene are completed before publishing artifacts.

## 6. Dependency order and work ownership

Recommended start: P0, then P1 and P2, then P3/P4, then P5/P6, then P7, then P8/P9, then P10 release closure. P10 preparation can begin earlier. This is not one giant integrated rewrite.

- Shared model/schema/admission changes have one integration owner. Define interfaces before parallel workers touch callers.
- After P0, P1 isolation and P2 persistence work are independent except their shared snapshot contracts.
- P3 provider work and P4 memory can proceed independently once P1 contracts settle.
- P7 email, P8 ads/social, and P9 WhatsApp can be parallel slices after P6 contracts; credentials and ingress are separate real prerequisites.
- Use separate worktrees for concurrent coding against the same repository; repository locks must not be bypassed to make delegation appear parallel. Do not synchronously wait on a child that needs the lock held by its parent.
- Finish and verify each selected phase. Do not narrow the roadmap by silently dropping difficult providers, channels, safety enforcement, or live acceptance gates.

## 7. Verification contract

### Per-phase procedure

1. Read the current implementation and affected callers; recheck protocol/API docs for the installed/selected version.
2. Reproduce the relevant bug before editing when possible. Keep deterministic regression tests for consumer-visible behavior, boundaries, authority, and failure transitions.
3. Implement the complete vertical slice and migration; update callers and broken behavioral contracts. Avoid permanent tests of copied wiring, source text, incidental wording, or mock echoes.
4. Run focused checks and a smoke of the actual changed runtime path. For UI changes, inspect the rendered surface with keyboard/error/blocked states; fixture tests alone are not visual proof.
5. Run the appropriate broader checks once after integration, not repeatedly mid-flight. Update docs/changelog with observed evidence and remaining external prerequisites.
6. Remove throwaway scripts/state and keep secrets out of command output. Never use the owner's production mailbox/campaign/customer as a destructive test target.

### Existing commands to reuse

Run from repository root with prerequisites checked against current manifests:

```sh
# Build web assets when changing the embedded console:
npm ci --prefix web
npm run build --prefix web

# Go behavior and static checks after implementation:
go test -race ./...
go vet ./...
go build -o /tmp/meshra-verification ./cmd/meshra

# Actual isolated CLI/daemon smoke; zero provider turns by default:
MESHRA_BIN=/tmp/meshra-verification node scripts/smoke.mjs
```

Select frontend/browser checks from the actual current `web/package.json` and suite conventions; do not invent script names. Run live/native/provider commands only with explicit cost/account authorization and disposable assets. Existing native smoke flags cover only their exercised paths; extend scenarios where needed rather than treating them as universal harness certification.

### Evidence record for every phase

Record implementation revision, platform, runtime/harness/provider/API versions, configuration/mode, commands/scenarios, exact observed outcome, redacted receipt/artifact references, and any unexercised acceptance criteria. Distinguish protocol fixture, real local process, real confinement, live model, live external API, and human-observed delivery. Report unknown usage/cost as unknown.

## 8. External prerequisites and blocker handling

No credentials or production accounts are required to write or begin this roadmap. Live completion requires operator-provided resources through secure setup, not chat:

- Authorized Claude/OMP installations and native/provider authentication; a DeepSeek account/model supporting the selected tool/thinking contract.
- Linux/WSL namespace, bubblewrap, cgroup delegation, and verified runtime assets for restricted execution. Detect actual availability; do not assume older plan probes still apply.
- Gmail OAuth application/test mailbox and consent/scopes.
- Eligible Meta developer/business/test ad assets, tokens/scopes, Page/Instagram account support, and permission to perform harmless approved test mutations/publication.
- WhatsApp Business test phone/recipient, current required templates/opt-in, and operator-managed HTTPS ingress hosting/domain/TLS plus any explicitly required media hosting.
- Explicit authorization for provider charges and external test sends/posts/account changes.

Missing credentials are not permission to fake an API success. Finish all reachable implementation and deterministic checks, identify the exact missing live gate and setup path, and leave that acceptance criterion pending. Do not globally mark the feature complete or silently substitute another service. Ask the operator only for decisions or secure external setup that repository/tools cannot supply.

## 9. Next-session execution prompt

> Read `docs/MESHRA_PERSONAL_AGENT_ROADMAP.md` as the roadmap for this task and inspect the current source before editing. Continue Meshra in this repository; do not start a replacement project or reimplement completed foundations. Start with P0, then implement P1 as the first behavioral milestone, keeping the rest of the roadmap pending. Reuse existing broker/sandbox/task/profile patterns, update all affected callers/docs, and preserve task identity/data. Do not claim restricted Claude/OMP execution until actual containment and native-tool enforcement have been exercised. Run focused behavioral checks and an actual runtime/UI smoke for changed paths. Record concrete evidence and external blockers. Do not make unapproved paid calls, production sends, public posts, or ad changes. If asked to continue beyond that milestone, follow the dependency order and finish complete vertical slices rather than scaffolds.

This prompt deliberately selects a manageable complete milestone; it does not remove any later requirement from the roadmap. A session explicitly assigned the entire roadmap must continue through every reachable phase and name genuine external blockers.

## 10. Primary references for implementation-time verification

These identify relevant upstream contracts; re-read current versions before implementing. They are not claims that all provider prerequisites have been validated during planning.

- Hermes memory: https://hermes-agent.nousresearch.com/docs/user-guide/features/memory
- Hermes scheduling: https://hermes-agent.nousresearch.com/docs/user-guide/features/cron
- Hermes security: https://hermes-agent.nousresearch.com/docs/user-guide/security
- OpenClaw agent separation: https://docs.openclaw.ai/concepts/multi-agent
- OpenClaw sandbox/tool policy: https://docs.openclaw.ai/tools/multi-agent-sandbox-tools
- DeepSeek tool calls: https://api-docs.deepseek.com/guides/tool_calls
- DeepSeek thinking state: https://api-docs.deepseek.com/guides/thinking_mode
- Gmail API: https://developers.google.com/workspace/gmail/api/guides
- Meta Marketing API: https://developers.facebook.com/docs/marketing-apis/
- Facebook Pages API: https://developers.facebook.com/docs/pages-api/
- Instagram platform: https://developers.facebook.com/docs/instagram-platform/
- WhatsApp Cloud API: https://developers.facebook.com/docs/whatsapp/cloud-api/

## 11. Progress ledger and evidence (2026-11-05)

Platform: Linux 6.18 WSL2, Go 1.27.2, Node 24.22, bubblewrap present, cgroup v2 controllers visible. Installed: Claude Code 2.1.318 (the bridge pins 2.1.303), OMP 18.16.0 (docs pin 18.12.1). Neither installed version was re-verified against the adapter pins. No credentials, provider turns, external sends or paid calls were used.

| ID | Status | Evidence / remaining gate |
|---|---|---|
| R01 | Partial | P0 done: PRODUCT.md, README, security, adapters (capability inventory), API contract, PERSONAL_WORKSPACE and the three prior plans reconciled. Final truthful release docs remain P10. |
| R02 | Partial | Mode on profile and admitted snapshot; admission/delegation/route/routine enforcement; legacy classified as unreviewed trusted; startup review disables unsafe routes/routines keeping history; `profiles.setMode`; deterministic tests in `internal/relay/profile_modes_test.go`; real-daemon smoke `scripts/smoke-modes.mjs`. Not yet: drain-or-cancel flow (mode change rejects while busy), CLI-specific mode display beyond `automation` passthrough, visual/keyboard UI inspection of the new controls, nested-delegation test through a live broker call. |
| R03 | **Partial (Claude and OMP implemented, neither proven on a real worker)** | Claude: `confined_harness.go` launches the pinned SDK bridge through `sandbox.Prepare` (ACP kind: private root/HOME, `--unshare-net`, read-only workspace snapshot, per-agent private state, scoped broker socket mounted at `/run/meshra/broker.sock`, daemon egress proxy narrowed to `https://api.anthropic.com` plus the profile's reviewed network grants). `bridges/claude-agent-sdk/confined.mjs` exposes the egress socket as a loopback proxy inside the worker. The adapter refuses restricted work without the launcher and never falls back to the host (`confined_test.go`). Availability is decided by a live prerequisite and OS probe (`confinedClaudeReadyLocked`), not a flag; each unmet prerequisite is named (`TestRestrictedClaudeNamesEveryUnmetConfinementPrerequisite`). Evidence so far: the containment probe (`confined-probe.mjs`) and the pinned bridge/native 2.1.303 profile-automation check both passed in a manual bubblewrap run using the same namespace and mount arguments but **not** the production `sandbox.Prepare` path (no cgroup). The production-path test `TestRealConfinedClaudeWorkerContainmentProbe` **skips on this host** (WSL: no delegated cgroup v2, nested `/usr/lib/modules` mount), so restricted Claude is reported unavailable here. Not done: a passing production-path run on a supported host, an end-to-end restricted turn with a real credential (needs authorization for a provider call), per-turn tool-roster proof beyond the existing bridge check, a profile-level network-grant preflight, and handling of the installed 2.1.318 vs pinned 2.1.303. OMP: `confined_omp.go` + `bridges/confined-exec/confined-exec.mjs` run the native binary in the same worker via `launchConfinedWorker` (shared with Claude) with `--no-tools`; the adapter (`omp/restricted.go`) refuses restricted work without the native launcher and requires the registered roster to equal the admitted catalog exactly (`restricted_test.go`); readiness names each unmet prerequisite (`TestRestrictedOMPNamesEveryUnmetConfinementPrerequisite`). Evidence on installed OMP 18.16.0 (offline, dummy key, no model call): with `--no-tools` and an isolated HOME the model-facing roster is exactly the host tools, and the binary starts, negotiates v2 and persists its session under a private HOME in a bubblewrap replica (same namespace/mount shape, no cgroup, **not** the production path). `TestRealConfinedOMPWorkerContainmentProbe` skips on this host. Not done for OMP: production-path run on a cgroup host, a real restricted turn (provider charge needs authorization), whether OMP honors the loopback proxy for live provider TLS (untested without a call), a network-grant preflight, and verification beyond 18.16.0. |
| R04 | Partial | Terminal persistence failure stops dispatch for the identity, never reports durable success and never re-executes (`terminal_ack_test.go`). New `effects.list` (read-only, `effects.go`) merges mutating tool invocations and connector deliveries into one effect view with status (intended/dispatched/completed/rejected/cancelled/uncertain), request digest, policy revision, provider reference, duplicate-risk flag, and an explicit "no provider idempotency" statement; it is independent of task state. `effects_test.go` covers restart after dispatch (uncertain, never replayed, task stays completed) and a claimed mutating tool call recovered as uncertain. `effects.list` is operator-only (model and routine credentials get 403, `TestEffectsListIsOperatorOnly`). The console has a read-only Automation → Effects tab (filter by outcome, unknown-outcome notice, never resends) and `meshra effects [--task] [--status] [--limit]` reads the same operation (`TestEffectsCommandIsReadOnlyAndValidated`; run against a real daemon: empty list, invalid status exits 2). Not yet: provider-native idempotency (no current endpoint supports it, so none is claimed), provider-side reconciliation, an explicit duplicate-risk retry confirmation for tool effects, visual/keyboard inspection of the Effects tab, and a kill/restart smoke against a real daemon with seeded effects. |
| R08 | Done (offline) | `selectMemories` ranks only the already-authorized set by task relevance (pinned first, stable tie-break by recency then id), never injects irrelevant unpinned memories to fill the budget, and appends a "Memory selection" line reporting pinned/relevant included and relevant/pinned omitted for budget and not-relevant counts. `profile_memory_select_test.go`: an older relevant note survives 200 newer irrelevant notes; pinned overflow is reported; ordering is stable; secrets stay redacted. `TestProfileMemoryDefaultsRevisionsAndExternalInjection` shows a better-matching memory from another conversation is still excluded (ACL before ranking). Not done: FTS/embedding retrieval (the in-process term-overlap ranker is bounded and sufficient only for small memory sets), per-memory freshness weighting, a console view of the last selection. |
| R09 | Partial | New `reusable` flag (migration 028) on operator-scope memories: only operator authority can set it; scheduled and external runs retrieve reusable knowledge but never owner-private notes or other conversations (`profile_knowledge_test.go`, including a model-authority promotion attempt that fails with 403). Reset keeps memories; forget (`memory.remove`) removes retrieval eligibility and leaves the `memory.removed` audit event; conversation delete removes scoped memories. Not done: a separate archive operation (none exists; documented as such), per-customer knowledge binding beyond conversation scope, a live scheduled-run smoke. |
| R10 | Done (offline) | Count, byte-budget and duplicate rules now apply identically with review on or off and for operator and model writers; duplicates ignore case and whitespace; over-limit existing data is retained and can be corrected by shrinking (`TestMemoryBoundsAreIdenticalForOperatorWritesAndShrinkingCorrectsOverLimit`). Existing staged/revisioned review behavior is unchanged. |
| R11 | Partial | A signed routine webhook now persists the exact event (id, sha256 digest, redacted payload) on the run before admission and admits it to entry steps as quoted untrusted data after the unchanged instruction (`TestRoutineWebhookRunConsumesExactEventAsQuotedData`). Non-UTF-8 and oversized payloads are refused with no receipt; invalid signature, stale timestamp and a changed body under the same event ID were already rejected (`TestRoutineWebhookSignatureReplayAndConflict`). Not done: provider-specific signature verification separate from the routine HMAC, payload encryption at rest, a data-classification field, a restart-at-admission-boundary smoke, an authorized-artifact payload reference. |
| R12 | Partial | New per-routine `missedRun` policy: `skip` (default, unchanged) or `latest` with `missedMaxAgeSeconds` (60..604800): after downtime the older missed occurrences are recorded as backlog and only the most recent one stays due if recent enough, then claimed through the normal path (unique occurrence key, overlap and autoStart gates). A repeated startup scan creates no duplicate. Console control added. Tests: `TestRoutineLatestMissedPolicyCatchesUpOnceWithinMaxAge`, `TestRoutineLatestMissedPolicyDoesNotRunStaleOccurrencesAndValidates`. DST: a repeated local hour (fall back) previously fired a cron job twice; each local wall-clock occurrence now fires once, a nonexistent local time (spring forward) has no occurrence that day, and downtime across the fold counts each occurrence once (`routines_dst_test.go`: US and UK rules, daily and half-hourly jobs). Preflight before admission (`routinePreflightLocked`): unattended work on an unrestricted or missing profile (`policy_blocked`), a restricted harness without proven confinement (`runtime_unavailable`) and a missing/disabled Direct API connection or unreadable credential (`needs_auth`) are recorded as skipped occurrences with that reason and no model call (`TestRoutinePreflightSkipsWithActionableReasonBeforeAnyModelCall`); the console explains each reason. Not done: multi-occurrence catch-up (deliberately limited to one), preflight of knowledge and delivery-target readiness, native-harness login checks (OMP/Claude logins are not inspectable without a call), the approval-waiting and overdue outcome labels, console display of the next occurrence per outcome. |
| R06 | Partial | Fixed an evidence-destroying order: compaction used to truncate older tool results in place and then summarize the truncated stubs. It now summarizes the original transcript, replaces older exchanges only after a valid bounded summary exists (with tool-call source references), and on a failed, empty or oversized summary leaves every message intact, emits `context_compaction_failed`, backs off, and stops visibly with an error once the prompt reaches 95% of the context window (`TestFailedOrEmptySummaryRetainsEveryToolResult`, `TestNearlyFullWindowWithFailedCompactionStopsVisibly`, updated `TestCompactionPreservesLatestExchange`). Not done: a recoverable artifact store for full tool results beyond the durable task record, a live context-pressure recovery run, summary-before-truncation for the Anthropic-format path if it differs, cold-boundary cache records. Fixture tests are protocol coverage, not live-provider proof. |
| R05, R07, R13–R21 | Pending | Not started. Live gates: DeepSeek account (R05, R07), Gmail OAuth test mailbox (R14), Meta test assets and permissions (R15, R16), WhatsApp test number plus public HTTPS ingress (R17), provider charges authorization. |

Full verification run: `go vet ./...` clean; `go test ./...` passes; `go test -race ./...` passes except `TestStatsMissingSourceIsReportedNotFatal` and `TestStatsImportClaudeLogs`, which report a data race on an unmodified checkout as well (pre-existing, unrelated to this work). `npm run build --prefix web` succeeds. `scripts/smoke.mjs` and `scripts/smoke-modes.mjs` pass (zero provider turns). Web unit tests (vitest, 17) pass. Not exercised: Playwright e2e and visual inspection of the rendered UI.
