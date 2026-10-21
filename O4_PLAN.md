# Meshra v0.4 — Developer Console & Agent Chat

## Implementation brief
Build a polished, modern, minimal, responsive, production-quality developer console **on top of the existing Meshra v0.3 backend**. This is a real application, not a dashboard mockup. Preserve all established messaging, agent identity, session, authorization, context, reliability, and collaboration semantics.

**Primary product experience: Chat.** Users must be able to create/open conversations, select one or multiple existing named agent instances (e.g. `claude:apmobileprod`, `claude:apmobilewallet`, `omp:adsengine`, `codex:reviewer`), send messages, receive streaming updates where supported, and inspect tasks, context, and execution provenance without leaving the conversation.

**Design bar:** as thoughtful as a well-designed developer tool: modern, restrained, fast, legible, icon-rich but not icon-noisy. No decorative dashboards, fake activity, or misleading typing indicators.

## Non-negotiable constraints
1. Inspect the existing repository and API contracts before changing anything. Reuse real v0.3 domain entities, authorization checks, ids, event sequences, and persisted state. Do not invent backend responses and then make the UI depend on them.
2. Multiple independent agent instances of the same harness must remain isolated by stable identity, permissions, session mapping, working directory, and conversation membership.
3. Keep Go + SQLite as the source of truth; use React + TypeScript + Vite for the frontend, Tailwind CSS and shadcn/ui for accessible primitives, TanStack Query for server data, and React Flow only for views where graphs improve comprehension. Embed built assets in Go for production. Prefer incremental additions over an unrelated new backend.
4. No direct terminal execution through the browser by default. Approval-gated controls must be explicit, permission-checked server-side, and audited.
5. Default bind to localhost. Do not expose endpoints or secrets publicly. Protect any browser auth/session with secure local design, CSRF-safe mutations if cookies are used, and strict CORS/origin policy. Document remote-access options separately.
6. WSL2 6 GB memory budget: avoid continuous high-frequency polling, giant in-memory event arrays, unbounded code editors, heavy charting, and unnecessary background processes.

## Information architecture
- **Chat** (default landing experience): inbox/conversation list, new conversation, rich message timeline, composer, participants, task/context side panel.
- **Agents**: searchable/filterable registry, unique harness badges, availability, workspace, active sessions, capabilities, create/edit permitted agents, view details.
- **Tasks**: status board/list and detailed execution trace, dependencies, approvals, error/retry history.
- **Contexts**: search/filter context references, inspect versions and provenance, attach to conversation/task with clear access indicators.
- **Activity**: traceable message/event history, correlations, retry attempts, agent/harness status.
- **Settings**: user preferences, visual density, keyboard shortcuts, connection/reconnect status, permissions and API health.
- **Overview**: optional compact operational overview, not the default entry point; do not duplicate the Chat UX.

## Core Chat UX
### Layout
Desktop: collapsible icon+label left navigation rail; resizable conversation sidebar; central conversation view; collapsible right contextual inspector. Respect reasonable min widths. Mobile: one panel at a time with back navigation; bottom-safe-area-aware sticky composer. Tablet and handheld: responsive, keyboard/controller-friendly focus navigation; no hover-only critical actions.

### Create/open conversation
- Clear **New conversation** action with accessible label/icon and keyboard shortcut.
- Choose 1+ registered named agents using searchable picker; show **harness, unique instance ID, workspace, status, permissions** before adding; disambiguate same names.
- Select conversation title (suggest editable default), optionally attach existing context references; configure agent selection/participation policies within backend capabilities.
- Conversation list: active/recent, pinned, search, filters, last message excerpt, unread/error badge, timestamp, and stable deep links.
- On revisit/reload, restore persisted timeline, participant set, draft (locally scoped), scroll position when sensible, and event cursor without duplicate messages.

### Composer & send
- Multiline composer; Enter to send, Shift+Enter for newline (user-configurable), Ctrl/Cmd+Enter alternative, composition/IME safe handling.
- Explicit recipients for targeted message vs conversation broadcast; **never implicitly broadcast potentially sensitive content**.
- Show attached context IDs with permission/state before send; attachments limited to backend-supported artifact types and sizes.
- Distinguish queued, accepted, delivered, running, awaiting approval, completed, failed, cancelled, and unknown statuses according to backend truth. Show retry action only if safe/authorized; warn about unknown-outcome duplicate risks.
- Disable send clearly when no valid target, no authorization, session unavailable without queue support, or the conversation cannot accept messages.

### Timeline and streaming
- Visually distinct human/agent/system/event messages; agent avatar/icon + name + harness, timestamps, status, trace/correlation, reply threading or quote references if supported.
- Render Markdown and fenced code safely; syntax highlighting with lazy loading and copy button; collapse long tool outputs, logs, diffs, and structured payloads.
- Stream partial output through SSE where supported, use sequenced event IDs and cursor-based replay/resync on reconnect; **do not synthesize fake streaming** for batch-only adapters.
- Auto-scroll only when user is near the bottom; otherwise show a “New messages” jump control. Support copy message, view raw payload, link to task/context, inspect error and correlation ID.
- Approvals must be unmistakable and show agent identity, action summary, scope, risk, expiration, and explicit allow/deny. Persist actions with audit trail.
- Empty/loading/error/offline/reconnecting states should be designed intentionally; no misleading permanent spinner.

### Multi-agent conversation experience
- Participant strip with exact identifiers and current status; participant add/remove is permission checked and never retroactively grants access to private context.
- Identify which agent authored every event; nested delegations and task handoffs are linked to the owning task rather than flattened into ambiguous chat bubbles.
- Context panel displays references and immutable versions consumed by an agent/task, not invisible shared-memory claims.
- Give per-message provenance: `from`, `to`, conversation ID, task ID (if any), timestamps, delivery status, source harness/session.

## Visual design system
- Direction: developer-focused, high contrast, minimal chrome, strong hierarchy, generous whitespace, precise alignment, subdued surfaces, restrained accent.
- Default dark theme optimized for long coding sessions; equally complete light theme; follow system by default. Persist preference.
- Typography: readable UI sans + monospace for code/IDs, limited type scale. Avoid oversized dashboard headers and tiny low-contrast labels.
- Define tokenized colors for background, foreground, surfaces, borders, focus ring, accent, success, warning, error, muted; accessible contrast, never status-by-color alone.
- Spacing system 4/8px baseline; coherent component dimensions; rounded corners without excessive pills or nested cards. Use skeletons sparingly.
- Icons: choose one consistent set (Lucide), use frequently for navigation, message types, harnesses, context, status, actions, shortcuts. Every icon conveys meaning and critical icon-only buttons have accessible labels/tooltips. Pair icons with text for primary navigation and destructive actions. Use custom harness logos only when brand usage is appropriate; don't invent official logos.
- Subtle transitions only (120–180 ms); honor reduced motion. No gratuitous gradients, glassmorphism, particle backgrounds, or animations that impair performance.
- Dense mode optional for power users, with spacious default. Keyboard focus and command palette/search should feel first-class.
- Design all four states per critical component: default, loading, empty, error; plus disabled and permission-denied where relevant.

## Suggested UI component inventory
AppShell, NavigationRail, ConversationSidebar, NewConversationDialog, AgentPicker, ParticipantChips, ConversationHeader, MessageTimeline, MessageGroup, MessageBubble, EventCard, MarkdownRenderer, CodeBlock, MessageComposer, DeliveryStatus, ApprovalCard, ContextReferenceChip, ContextInspector, TaskInspector, AgentTable, AgentDetail, TaskList, DependencyGraph, ActivityTrace, ConnectionIndicator, SearchCommandPalette, ShortcutHelp, ErrorBoundary, EmptyState, Toasts/Notifications.

## Integration contract and API readiness (M0)
Before implementation, inventory actual APIs and events; produce an `API_UI_CONTRACT.md` mapping existing endpoints/fields to UI features. Fill backend gaps minimally and safely. Required capabilities (names illustrative, match repository conventions):
- list/create/read/update conversation metadata; list participants, add/remove according to permission
- paginated conversation messages and per-message delivery status
- send direct/targeted message and retrieve correlation/task details
- SSE events with stable event IDs and replay cursor, snapshot/resync when history has expired
- list/filter agents and retrieve runtime/session state
- list/read tasks, dependencies, cancellation and approvals
- list/search/read versioned context and attach references with authorization
- health/ready version information
Never assume support for pause/resume, native typing, file upload, or terminal streaming unless verified against adapters.

## Milestones and deliverables
### M0 — Discovery and foundations
Inspect v0.3 code, current frontend (if any), API, event stream, tests, security. Document mismatches, API contracts, navigation map, design tokens, and realistic UI behavior. Include representative wireframes in Markdown or diagrams (not fabricated backend data).

### M1 — Design system and app shell
Initialize/extend React/TypeScript/Vite. Theme tokens, Lucide icons, shared components, responsive rail/panels, keyboard access, command palette foundation, loading/error design states. Connect real health and agent data.

### M2 — Chat MVP (release blocker)
Conversation create/open, named multi-agent participant picker, persistent list, paginated timeline, message composer, delivery states, streaming or batch response, reconnect + resync, cross-session identity correctness, deep links, error and empty states. Test with real named agents. This is the highest priority user-facing release milestone.

### M3 — Collaboration UX
Task inspector, live delegation progress, dependencies, explicit approvals, context ref attach/read, participant and permissions view. Avoid conflating conversations and task runs.

### M4 — Operations UX
Agent registry and details, context browser, activity tracing, filter/search, trace correlation, basic performance and error observability. Graph only when useful.

### M5 — Production hardening
Go static embedding; secure local deployment; origin/auth/authorization validation; SSE replay and pagination; bundle-size/perf/accessibility audits; keyboard and mobile tests; no mock data; migration and rollback instructions; regression tests for v0.1–v0.3.

## Acceptance tests (must automate where feasible)
1. Create a conversation with `claude:apmobileprod` and `omp:adsengine`; send targeted message to one and confirm no leakage to another agent including `claude:apmobilewallet`.
2. Create another conversation with two instances of the same harness; IDs are visually and technically disambiguated.
3. Reload page during active response; replay/resync resolves timeline without duplicates or false delivered/complete claims.
4. Show queued/busy/offline/error and unknown-outcome behavior honestly, including retry guardrails.
5. Attach a permitted immutable context version to a task; reject unauthorized/private context both in UI and backend.
6. View a real delegated task and its origin message, result, and correlation trace.
7. Handle failed/expired approval without silently proceeding.
8. Test keyboard-only navigation, screen-reader labels, light/dark contrast, reduced motion, small viewport and touch input.
9. Ensure agent names, user content, Markdown, and code blocks cannot inject HTML/script; do not expose tokens in logs/UI.
10. Test SSE burst/backpressure, 1,000+ message pagination/virtualization where necessary, and memory use on WSL2 6 GB.
11. Run current backend and frontend unit/integration/E2E suites; document real cross-harness test evidence and untested paths.

## Product quality gates
- The user can start a named multi-agent chat and receive a real answer without reading a CLI manual.
- Every visible action reflects a real backend API and permission; unsupported controls are omitted or disabled with explanation.
- No fake agents/messages/metrics in the shipped experience; fixture data allowed in isolated tests/storybooks only.
- Default interaction path feels fast and calm; navigation and chat composer remain responsive under streaming load.
- Visually consistent icons and spacing; actionable errors; recoverable sessions; strong discoverability and accessibility.
- All prior v0.1–v0.3 functionality continues to work.

## Instructions to the implementation agent
Implement, do not only draft wireframes. Work milestone by milestone, prioritizing **M2 Chat** before charts and secondary dashboards. Adapt all names and schemas to the actual codebase; do not introduce duplicate sources of truth. After each milestone run tests and provide a brief change summary, screenshots or locally runnable UI verification if supported, known limitations, and the next milestone. Never claim unsupported streaming, approvals, or adapter behavior. End with actual demo steps to create a conversation, address a named agent, observe a streamed/replayed reply, attach shared context, and inspect a delegated task.
