# Direct API agent upgrade (Hermes-inspired)

Status: implemented except optional smart approvals and post-turn review (no configured cheap-model connection); delegation remains asynchronous to avoid repository-lock deadlocks. Real sandbox execution requires the confinement prerequisites documented below.

## Goal

Make the **Direct API** (OpenAI-compatible, harness id `openai`) a capable agent
by adding the Hermes Agent features that Claude and OMP already get from their own
harnesses. **Do not change Claude or OMP behavior.** Meshra-level shared features
(approvals, memory review, search) are allowed only when they are opt-in or
harness-neutral, and must leave the verified Claude/OMP paths unchanged by default.

Reference: Hermes Agent docs (hermes-agent.nousresearch.com/docs): toolsets,
smart approvals, bounded memory, skills self-improvement, `todo`, `execute_code`,
`process`, `delegate_task`, `session_search`.

## Current state (verified in code)

- `internal/adapters/openai/adapter.go` `runtime.Run` (line ~380) already has an agent
  loop: chat completion -> tool calls -> `dispatch` -> repeat. Properties:
  - Non-streaming (`Stream: false`), one request per step.
  - `messages` starts as a single user message: `turn.Task.Text`. There is **no
    system prompt**; instructions, memory, skills and bounded history are folded
    into the task text by `profilePrompt` (`internal/relay/profiles_send.go:155`)
    and `chatPrompt` (`internal/relay/chat.go:29`).
  - Tool calls run sequentially, capped at `MaxToolCalls = 40`.
  - No compaction of `messages` inside a turn; each turn is a fresh session with
    <= 12 recent exchanges / 32 KiB of history (`docs/CHAT_MODE.md`).
  - Tools are refused unless the endpoint is declared `SupportsTools`.
  - Arguments are validated against the tool's JSON schema (`validateArguments`).
- Tools come from the relay broker (`internal/relay/tool_broker.go`,
  `tool_actions.go`, `tool_catalog.go`). The catalog is selected per profile by
  `profileToolCatalog` from `Capabilities` (WorkspaceRead/Write/Command, MemoryWrite,
  ExternalMCP, Attachments, `AllowedProfileIDs`). That is already a toolset mechanism.
- Approval is binary per tool: `parseToolAction` sets `approval=true` for
  `meshra_workspace_write`, `meshra_workspace_command`, `meshra_external_mcp`,
  `meshra_attachment_publish`. Humans decide via `tool_approvals`, bound to an
  `ActionHash`. Writes go to a private workspace copy and need a second approval
  to apply (`workspace_changesets`).
- `meshra_history_search` is **already FTS5** (`profile_history` virtual table,
  migration `011.sql`) but scoped to the current conversation.
- `meshra_memory_save` writes directly (`action.mutation=true`, no approval),
  stored in `profile_memories` with revision and pinned flags.
- Skills are hand-written and pinned by version and SHA-256
  (`internal/relay/skill_context.go`, `internal/model/skills.go`).
- Routines: `meshra_routine_propose` already creates inert, disabled drafts for human
  review. This is the pattern to reuse for skills.

## Findings that change the earlier proposal

- FTS5 search already exists; only the **scope** (conversation -> profile-wide) is new.
- Toolset presets need no new catalog logic; they are named bundles of existing
  `Capabilities`.
- The agent loop already exists; the work is a better loop (system prompt,
  streaming, compaction, parallel read-only calls), not a new one.

## Non-goals

- No change to the Claude SDK bridge or the OMP adapter.
- No auto-approval of memory or skill writes by default.
- No Nous Portal gateway, remote sandbox backends (Modal, Daytona, Singularity), voice
  additions, or RL/trajectory tooling.
- Do not weaken: private-copy workspace writes, separate apply approval,
  `ActionHash` binding, pinned skills, deny-by-default for unattended runs.

## Phases

Each phase is independently shippable and ends with tests plus a short entry in
`CHANGELOG.md` and `docs/`. Re-verify the "Open questions" list before starting.

### Phase 0: confirm assumptions (read-only, 1 hour)

Check and record the answers in the PR description:

1. How Claude and OMP reach the broker (`tool_socket.go`, `ScopedMCP`,
   `bridges/mcp-stdio`). Confirm approvals and mutation rules are enforced in the
   broker for them too, so shared changes can be gated on `Binding.Harness == "openai"`.
2. Whether `chat()` (adapter.go ~606) can stream (SSE) against the configured endpoints.
3. Next free migration number in `internal/relay/migrations` (last seen: `020.sql`).
4. Whether `mcp.go` tool listing also reads `profileToolCatalog` (new tools must
   appear for all harnesses or be filtered by harness).

### Phase A: loop quality and quick wins (Direct API only)

A1. **Proper system prompt.** Split `profilePrompt` output into a system message
(instructions, memory, skills index) and user message (task). Keep the byte caps
(256 KiB prompt). Put stable content first for provider prefix caching.
Files: `internal/adapters/openai/adapter.go`, `internal/relay/profiles_send.go`,
`internal/model/model.go` (add `Turn.System string`, other adapters ignore it).

A2. **Tool-loop hardening.** Make `MaxToolCalls` a per-profile setting with the
current 40 as default; add repeated-call detection (same name+args 3 times ->
return a tool error telling the model to change approach); keep the cap error.
Files: `adapter.go`.

A3. **Parallel read-only calls.** Dispatch calls in one assistant message
concurrently only when every call is read-only (history, skill_read, workspace_read,
attachment_read, agents_list, routine_list). Mutations stay sequential, since the
broker allows one outstanding mutation per turn (`tool_broker.go:~215`). Return
results in the original order.
Files: `adapter.go` (needs a read-only flag exposed on `ToolDefinition`, or a
static set in the adapter).

A4. **In-turn compaction.** When reported `prompt_tokens` exceeds 75% of
`ContextWindowTokens` (already tracked for the `context_usage` event), replace the
oldest tool results with one-line stubs (`[tool result truncated: N bytes]`) first;
then summarize older assistant/tool messages with one extra completion. Always keep
the system message and the latest user message. Emit a `context_compacted` event.
Files: `adapter.go`, new `compact.go`, tests with a fake endpoint.

A5. **Streaming.** If Phase 0 confirms support, stream completions and emit text
deltas the way the other adapters do; fall back to the current path when the
endpoint rejects `stream: true`.
Files: `adapter.go`.

A6. **`meshra_todo` tool.** In-turn plan list (`add`/`update`/`list`; statuses
pending, in_progress, done). Stored on the task as an event, not a new table;
rendered in the console's existing tool-call view. Read-only to the host
(no mutation, no approval). Offered to all harnesses only if Phase 0 says it is
harmless; default: `openai` only.
Files: `tool_catalog.go`, `tool_actions.go`, `web/src` tool-call renderer.

A7. **Toolset presets.** Profile create/edit UI gets presets (`Chat only`,
`Research` = read + history + attachments, `Developer` = read + write + command,
`Full`) that just fill `Capabilities`. No new backend.
Files: `web/src` profile form only.

### Phase B: cross-conversation recall (shared, read-only)

B1. Add `meshra_session_search`: FTS5 over `profile_history` across **all
conversations the current profile legitimately has access to** (same
`profileDataAccess` checks as `profile_history.go`). Return conversation id, task
id, snippet, timestamp; `meshra_history_read` stays the way to read an exchange.
Results are quoted data, not instructions (reuse the existing wording).
B2. Add `scope` argument to `meshra_history_search` rather than a second tool
if the schema stays simple; decide in review.
B3. Retention: results must respect the configured retention window and
conversation deletion (`conversation_delete.go` must also delete FTS rows).

Tests: privacy (profile A cannot see profile B), deleted-conversation removal,
operator-only memory untouched, snippet length and result bounds.

### Phase C: approval ergonomics (Direct API profiles first)

Approval policy per profile, stored with the profile tool policy so a change
bumps `ToolPolicyRevision` and invalidates pending approvals (existing trigger in
migration 013 does this).

- `manual`: today's behavior (default for all profiles).
- `allowlist`: a human-created rule skips the prompt for an exact match.
- `smart` (optional, last): a cheap model classifies pending `command` calls into
  allow / deny / ask; only `allow` for read-only-looking commands inside the
  sandbox, never for `meshra_external_mcp` or apply-to-original. Classifier output
  is advisory and logged; any parse error or timeout means **ask a human**.

C1. **Hardline deny list** (do first, benefits every profile): reject before the
prompt commands that are clearly catastrophic (root `rm -rf`, fork bombs, raw disk
writes) and writes to protected paths (SSH keys, cloud credentials, Meshra secret
stores). The sandbox already confines them; this removes noisy prompts and gives a
clear error. Files: `tool_actions.go` (`prepareAction`), new `tool_denylist.go`.
C2. **Allowlist rules.** New table (migration `021.sql`):
`tool_allow_rules(id, profile_id, tool, matcher_kind['argv_exact','argv_prefix','path_glob'], matcher, created_by, created_at, expires_at, revision)`.
A rule match sets `humanApproved`-equivalent state `auto_approved` with the rule id
recorded in `tool_invocations` for audit. Rules never apply to `meshra_external_mcp`,
`attachment_publish`, or changeset apply. Console: "Always allow this" button on an
approval, plus a rules list with revoke.
C3. **Suggestions view.** Read-only list of frequently approved identical actions
(from `tool_invocations` + `tool_approvals`); never auto-applies.
C4. **Smart classifier.** Only after C1-C3 ship. Needs a configured cheap model
connection; off by default; disabled for routines/webhooks/connectors (unattended
runs keep deny-by-default for anything needing approval).

Tests: rule matching edge cases (quoting, argv vs shell text, path traversal),
policy-revision invalidation, rule expiry, audit trail, classifier failure -> ask.

### Phase D: memory review queue and skill proposals

D1. **Memory staging.** Profile setting `memoryWriteApproval: off|on` (default off,
current behavior). When on, `meshra_memory_save` creates a row in a new
`profile_memory_pending` table; console shows Approve / Edit / Reject. Approval
writes through the existing revisioned save path. Add bounds like Hermes: reject
exact duplicates and enforce a per-profile character budget with a clear error so
the model consolidates instead of growing memory without limit.
D2. **`meshra_skill_propose`** (pattern: `meshra_routine_propose`): creates an
inert, unpinned skill draft with name, description, instructions, optional
`basedOn` skill id/version for a patch. Never auto-binds to a profile, never
changes an existing pinned version; a human reviews in Automation > Skills, then
binds as today.
D3. **Optional post-turn review** (off by default): after a successful turn a
tool-free cheap-model call sees a digest of the transcript and may call only
`memory_save` / `skill_propose`. Bounded tokens, same staging/review as D1/D2,
disabled for connector/routine runs unless explicitly enabled.
D4. **Journey view.** Console list of learned memories and proposed skills with
timestamps and source task links.

Tests: staging cannot be bypassed, rejected proposals leave no trace in prompts,
pinned skill hashes unaffected, review call cannot call any other tool.

### Phase E: advanced (decide after A-D)

E1. **`meshra_process`**: start/poll/read-log/kill a background command inside the
sandbox copy, with the same approval as `meshra_workspace_command` at start
and hard limits (count, duration, log bytes). Needs sandbox lifecycle work in
`internal/sandbox` (currently single-shot, <= 300 s).
E2. **`meshra_execute_code`**: run a short script in the sandbox that can call
broker tools through a local socket; each inner call goes through the normal broker
(same approvals, hashes, limits). Highest value and highest risk; requires a design
doc and threat model before code. Do not start without one.
E3. **Delegation polish.** `meshra_profile_request` exists; consider a "wait for
result" helper and result summarization for Direct API profiles. Skip if not needed.

## Suggested order and sizing

| Order | Item | Size | Risk |
|---|---|---|---|
| 1 | Phase 0 | XS | none |
| 2 | A1 system prompt, A2 loop hardening | S | low |
| 3 | C1 hardline deny list | S | low |
| 4 | A7 presets, A6 `todo` | S | low |
| 5 | B session search | M | medium (privacy) |
| 6 | A4 compaction, A3 parallel reads, A5 streaming | M | medium |
| 7 | C2/C3 allowlist and suggestions | M | medium (security) |
| 8 | D1/D2/D4 memory staging, skill proposals, journey | M | low |
| 9 | D3 post-turn review, C4 smart approvals | M | medium |
| 10 | Phase E | L | high |

## Cross-cutting requirements

- Every new tool gets: schema in `tool_catalog.go`, parsing in `parseToolAction`,
  execution in `executeAction`, a `ToolCatalogDigest` consequence (existing tasks keep
  their admitted catalog; do not mutate admitted snapshots), console rendering,
  and tests alongside `tool_broker_test.go` / `tool_events_test.go`.
- Gate new tools by profile capability or harness so existing Claude/OMP profiles'
  catalogs and digests do not change unless the operator opts in.
- All new model-facing text from stored data is quoted as data, not instructions.
- Run `go test ./... -race`, `npm run build --prefix web`, and rebuild embedded `dist`
  per the README before finishing each phase. Add deterministic fake-endpoint tests;
  no live provider needed.
- Update `docs/adapters.md`, `docs/API_UI_CONTRACT.md` for any API change, and
  `CHANGELOG.md`.

## Open questions for the owner

1. Should `meshra_todo`, `meshra_session_search` and the memory review queue be
   offered to Claude/OMP profiles (opt-in per profile) or stay Direct API only?
2. Is a cheap-model connection available to power the smart classifier and
   post-turn review, or should those stay out of scope?
3. Which endpoints must streaming support (local servers, hosted providers)?
4. Acceptable default for `MaxToolCalls` and the compaction threshold per model?
