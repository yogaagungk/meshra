# One-to-one AI chat

Open **Chat → New conversation**, keep **Chat with an AI** selected, choose one
registered AI, and create the conversation. A stopped AI starts when the first
message is sent. Native harness installation and authentication are still required.
Use **Agent collaboration** for the existing multi-recipient workflow.

Chat supports saved history, contextual follow-ups, Markdown/code rendering, copy,
**Stop response**, and regeneration of the latest completed reply. Regeneration
replaces that reply at its original position, with at most three executions of a
message. Earlier replies cannot be regenerated once a follow-up has been admitted.
Interrupted or failed executions can be inspected and explicitly retried through
the existing execution inspector. A new message can be sent after stopping a reply;
the scheduler waits for the previous native execution to release its resources.

## Conversation context and memory

Each chat turn runs in a fresh native session. Meshra closes the temporary runtime
before releasing the turn slot, and preserves the registered agent's independent
native session. Codex/OMP chat runtimes use separate disposable state directories
to avoid the durable identity lock; those directories are deleted on close.
Conversation context comes from SQLite, scoped to the exact chat
and AI. Chat turns cannot delegate to other agents. Native harness workspace and
provider settings still apply; this does not create a separate filesystem sandbox.

The AI receives up to 12 recent completed exchanges, capped at 32 KiB of encoded
history. Within each exchange, the user text is limited to 4 KiB and the answer to
8 KiB. Oversized history is shortened at UTF-8 boundaries. Failed/cancelled turns
are excluded. Current messages and attached context have separate existing limits;
the assembled prompt is capped at 256 KiB. These are byte limits rather than an
exact model-token calculation. Older details may be forgotten; there is no automatic
summary or semantic retrieval in this version. Attach important reference material
when it needs to be available for a turn.

The console renders one page of up to 20 exchanges. **Load older messages** replaces
the current page; **Back to latest messages** restores the newest page. Timeline
responses also have a 1 MiB payload budget (a single task is never split). Timeline,
conversation, and chat-status query caches are released when the view is unmounted.
A separate small status query keeps stop/regenerate controls current while browsing
older pages. Event subscriptions and refresh timers are closed on navigation.

The existing limits remain: one active execution per agent, at most 32 globally,
bounded durable inboxes, task deadlines, 256 KiB responses, and bounded delta/event
buffers. Chat replies are stored directly with their task instead of also creating
shared result artifacts. SQLite history remains subject to the configured retention
policy (30 days by default). Native harnesses can keep their own session files on
disk; closing a runtime does not delete those files or impose an OS memory limit
on a third-party process.

## Response delivery

Responses appear after completion and credential redaction. Token-by-token text
streaming is intentionally deferred: the existing redaction protects credentials
split across arbitrary native output fragments. Progress, completion, and cancellation
still update through the live event feed. This implementation does not simulate
streaming by animating a completed answer.

## API and persistence

`conversation.create` accepts `mode: "chat"` with exactly one participant and the
administrator principal. Omitting `mode` preserves collaboration behavior. Chat
participants cannot be changed; create another chat to choose a different AI.

Console additions (same cookie/CSRF boundary as existing endpoints):

- `GET /console/api/chat-state?conversationId=…`: latest task ID, state, regeneration availability.
- `POST /console/api/regenerate` with `{ "taskId": "…" }`: requeue the latest completed chat reply.

Admission is idempotent and rejects a second active message in the same chat.
Database schema 6 adds a `chat_conversations` table without changing existing
conversation rows. Opening an existing database applies the migration automatically;
older executables reject schema 6. Back up the data directory before downgrading.

## Verification

Relay tests cover exact conversation context, fresh native sessions, preservation
of the ordinary agent session, cancellation/continuation, regeneration boundaries,
forbidden delegation, context byte limits, timeline byte limits/cursors, and release
of temporary runtimes across 30 consecutive turns. Browser tests cover the default
chat flow, automatic startup, stop/continue/regenerate, reload, accessibility, mobile
overflow, and replacing pages through 43 saved messages. These use deterministic
adapters and do not claim live-provider or OS-level memory-soak validation.

## Harness picker and console refinements

The default chat picker now selects **Codex**, **Claude**, **OpenCode**, or **OMP**,
rather than a registered agent. Meshra reuses at most one managed identity per
harness, with a private workspace under the relay data directory. These managed
identities are omitted from the Agents page and collaboration picker. Existing
named agents and older conversations remain supported. Selecting a harness does
not install it or establish provider authentication.

Task execution views have been removed from the console. Stop and regenerate
remain in chat, and Activity links directly to the associated conversations.
CLI task diagnostics remain available for troubleshooting.

The new-agent workspace field opens an in-app folder explorer for the daemon's
filesystem. It does not upload folders or use a browser file-upload control.
Directories are paginated at 100 entries; the scanner retains at most 101 names.

The Manrope variable font and harness vectors are local assets. Codex/Claude/
OpenCode vectors are distributed under the MIT license in
`web/src/assets/harnesses/LICENSE`; OMP uses a local monogram. Short CSS transitions
and thinking animations respect the operating system's reduced-motion setting.
