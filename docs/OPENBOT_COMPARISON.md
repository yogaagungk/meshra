# Meshra vs OpenBot: verified comparison

Source-verified against OpenBot v0.58.0 (`../openbot`, PolyForm Noncommercial) and
Meshra's current tree. OpenBot is a desktop (Electron) app with optional hosted
accounts; Meshra stays a local web console. Anything marked **out of scope** conflicts
with Meshra's constraints: local only, web UI only, no plugin or marketplace install.

## Routing and coordination

| Capability | OpenBot (source) | Meshra | Status |
|---|---|---|---|
| Send without picking a recipient | Channel composer routes for you (`docs/architecture/channels-and-messaging.md`) | `web/src/routing.ts`: mention, chosen, only member, follow-up; never a silent broadcast | **Done** |
| `@mention`, `@everyone` | `leadingAddress`; picker offers `@everyone` | Composer autocomplete (arrows, Enter/Tab, Esc) | **Done** |
| Visible routing reason | Lead posts a message naming who answers | Composer shows why; lead routing adds a "Routed by" note under the message | **Done** |
| Lead agent routes by role | `ChannelService` runs the lead in a tool-less session with a result schema | `routing.route`/`routing.result` (`internal/relay/routing.go`): disposable verified tool-free turn, strict JSON, intersected with participants | **Done**, see limits below |
| Unreadable routing answer | Pauses the task and offers Reassign | Surfaced as an error; the draft is kept so you can @mention or pick | **Done** |
| Queue with pause, resume, reorder, cancel | FIFO queue per agent | Existed on Automation, now also inside chat (collapsed strip, pause/resume, Run next, Cancel) | **Done** |
| "Steer" a queued message | Steer | "Run next" reorders a queued message to the front | **Done** (reorder, not mid-turn injection) |
| Failover when a provider fails | Automatic move to a backup model | One-click "Try with another agent" on failed or timed-out turns | **Done as a human decision**, see limits below |
| "continue" resumes stopped tasks | Resumes stopped or failed root tasks | Manual retry in the execution inspector | Missing |
| Purpose per channel | Channel settings | "What is this chat for?" next to the lead, saved in this browser | **Done** (browser-local) |
| Archive/Restore channels | Yes | Delete only | Missing |

### Limits of the Meshra versions

- **Lead routing** needs a lead on a harness advertising `toolFreeDraft` (Claude, OMP, OpenAI-compatible).
  Codex and OpenCode cannot prove an empty tool catalog, so they are not offered as leads. The lead
  and purpose are stored in the browser (`localStorage`), not the relay. Messages containing
  detected credentials are never sent to a router.
- **Failover is not automatic.** Meshra's core rule is that a failed turn may already have had external
  effects, and replays are a human decision (`docs/architecture.md`). Automatically re-running the
  same task on another agent would break that rule, so the UI offers it and warns when the first
  attempt was ambiguous.

## Agents

| Capability | OpenBot | Meshra | Notes |
|---|---|---|---|
| Describe an agent, review generated instructions, save | Yes | Existed (`profiles.generate`, tool-free), was buried in a collapsed section | **Surfaced**: open by default for new agents |
| Avatar and identity | Generated avatars | 7 icons and colors | Parity |
| Agent cards | Friendly name, role | Name first, exact ID beneath, plain status text | **Done** |
| Context monitoring and auto-compaction | Per agent | Bounded history window | Missing |
| Routines | Schedule or webhook | Automation page | Partial |
| Workflows graph | Yes | No | Missing |
| Embedded browser | Yes | Browser bridge and panel | Parity |
| Local model detection (Ollama, LM Studio) | Yes | OpenAI-compatible endpoints can be added by hand | Partial |
| Providers | Codex, Claude, Grok, OpenCode, Gemini, Cursor, Cline | Codex, Claude, OMP, OpenCode, Gemini, Cursor, Cline | No Grok |

## Out of scope for Meshra

Electron packaging, iPhone/Android apps and push notifications, hosted accounts and
teams, remote desktop, Computer Use, plugin and marketplace install, OpenBot's public
site. Meshra keeps local sign-in, loopback binding and explicit sharing.

## What Meshra has that OpenBot does not

Immutable pinned context with explicit grants, exact canonical agent identities,
append-only task history with correlation and trace, ambiguous-outcome reporting
(`uncertain_execution`), and a documented localhost security model.

## Next candidates

1. Server-side lead and purpose, so routing settings follow the conversation across browsers.
2. "continue" to resume stopped tasks; archive and restore conversations.
3. Local model discovery for Ollama and LM Studio.
4. Context usage meter and compaction.
