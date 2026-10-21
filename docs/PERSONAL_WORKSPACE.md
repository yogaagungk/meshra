# Personal workspace controls

Meshra remains localhost-only. This release adds personal controls without
opening a listener, adding remote authentication, or introducing shared users.

## Personal access

Settings → Personal access saves an operator display name in SQLite and lists
signed-in browsers. Revoke one other browser or sign out all other browsers.
Public session IDs are unrelated to authentication cookies; session listings
never contain cookies, administrator tokens, CSRF tokens, or raw User-Agent data.
The existing local administrator token remains the sign-in credential. Sessions
are bounded to 128, expire after eight hours, and are cleared by a daemon restart.
Revoked sessions also lose event-stream access on the existing stream check.
This is one administrator account, not multi-user workspace authorization.

## Coding workspaces

Workspaces registers up to 32 existing directories. Browse files and subfolders,
preview text, inspect branch status and staged/unstaged diffs, and use
“Chat in this project” to select that directory for a new AI conversation.
Workspace and model bindings are saved per conversation. Each disposable chat
runtime receives that conversation's selected working directory and config;
managed identities remain bounded to one per harness. Existing named-agent chat
configurations retain their native model settings.

File browsing uses Go's `os.Root` to contain traversal and symlinks inside the
registered directory. Symlinks are omitted from listings, `.git` internals are
excluded, and special files cannot block on open. Pages retain at most 101
entries and display at most 100. Previews read at most 64 KiB plus one detection
byte; binary files are refused and recognized credentials are redacted. Git
runs without a shell, with optional locks, external diffs, text conversions and
filesystem-monitor commands disabled. Git output is capped at 256 KiB, with a
five-second timeout. No file writes, terminal, commit or push endpoint is added.

Workspace browsing containment is not an OS sandbox for native AI tooling.
The existing read-oriented harness policies still apply. A project containing
native hooks/MCP/plugin configuration may be rejected by the isolated adapter.
Individual user permissions and stronger native execution isolation are separate
work required before sharing this administrator UI with others.

## Resource management

Settings → Resources persists these limits and applies them without a restart:

- Concurrent turns: 1–32, default 4. Extra work waits in the existing bounded queue.
- Started agent slots: 1–64, default 8. Lowering the limit does not kill active agents.
- Idle shutdown: 0–1440 minutes, default 0 (disabled). Checked every 30 seconds;
  busy/starting agents and agents with pending work are never reaped. Stopped
  agents can be started again in Agents or by the existing AI chat send flow.
- Daily turn budget: 0–100000, default 0 (unlimited). Applies to all dispatched
  attempts, including regeneration and agent-to-agent work. UTC midnight resets
  the allowance. Reservations survive restarts and interrupted runs still count.
  Work already running finishes. Queued work retains its original deadline and
  may time out while waiting for the allowance; raising the limit wakes scheduling.

Current activity reports queue depth, running turns, started slots, Go heap and
Linux daemon resident memory. These memory figures exclude native child processes.
The settings bound concurrency, not a hard aggregate RAM ceiling: a chat may use
a temporary native runtime in addition to its managed readiness slot. Discovery
is serialized and bounded to 25 seconds, with no model response generated.

## Models and usage

The new-chat dialog discovers models for its selected harness and offers
“Native default” if discovery is loading, unavailable, or empty. Settings →
Models & usage also provides discovery refresh and today's usage breakdown.
Only model/provider identifiers and names cross the console boundary; native
provider credentials remain owned by the harness.

Discovery uses Codex `model/list`, OMP `get_available_models`, Claude Agent SDK
`supportedModels()` with no input messages, and OpenCode `/config/providers`.
The OpenCode adapter uses configured providers rather than downloading the full
provider catalog, which can exceed the transport bound. Up to 200 sanitized,
deduplicated options reach the UI. Missing native authentication is reported as
unavailable; no guessed or hardcoded model catalog is displayed.

A schema-7 usage ledger records attempts, chosen harness/model/provider, project,
state, input/output byte counts and elapsed time. Token counts appear only when
reported by Codex, Claude or OpenCode. OMP currently reports no token totals to
this adapter. OpenCode reports tokens for the returned assistant message; its
counts do not represent every intermediate tool/model step. Counts include
cached input where the native summary exposes it. Native-default rows cannot
identify the actual selected model unless the harness reports that separately.
No token estimates, inferred billing, subscription quota or monetary budget is
claimed. The daily turn budget is an execution-count control, not a spending cap.
Tracking begins with this update. Ledger rows are pruned after 30 days, including
when general task retention is disabled. The UI queries bounded aggregate rows
and releases file, diff, model, session and usage caches when they become inactive.

## Verification

Regression coverage checks session revocation and credential non-disclosure,
resource persistence, concurrent dispatch, daily budget across restarts, idle
shutdown, workspace escape attempts and bounded previews, model config delivery,
and deterministic native catalog protocol parsing. The browser suite exercises
project registration, Git diffs, project chat/model selection, profile persistence,
revocation, resource editing, usage, keyboard navigation, dark/light themes,
mobile overflow and accessibility. Native catalog smoke discovery is available
with `MESHRA_NATIVE_MODEL_CATALOG=1 go test ./tests -run '^TestNativeModelCatalog$' -v`;
it generates no paid AI response.

Provider quota and context telemetry are now connected through
[AI Status](AI_STATUS.md), with Codex/OMP live quota reports and explicitly scoped
native context observations. The earlier quota limitation above describes the
initial personal-controls release; use AI Status for current telemetry behavior.
