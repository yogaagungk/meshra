# v0.4 API/UI contract (M0)

Audited v0.3: Go/SQLite only; no frontend. `/v1` rejects every Origin header;
bearer identity/session checks and immutable context ACLs are authoritative.
Conversation operations are POST `/v1/collaboration/OP`; full task/status/events
are `/v1/tasks/ID`. Events have stable increasing numeric IDs and 256-item pages.
Text deltas are deliberately buffered/redacted until completion. The UI must show
running progress, never simulated token streaming or typing.

Add `/console/api` for authenticated local **human operator** sessions. Exchange
an admin token once for a random HttpOnly SameSite=Strict cookie (8-hour absolute
expiry) and CSRF token held in browser memory. No bearer token is persisted by the
frontend. Validate loopback Host, exact Origin and Fetch Metadata; require CSRF on
all mutations. Legacy bearer routes keep their browser-Origin rejection. No CORS.
The browser console is an admin operator surface, not a replacement agent token.

| UI capability | Source |
|---|---|
| Session/health | GET `/console/api/session`, POST `/console/api/login`, POST `/console/api/logout` |
| Agent registry/lifecycle | existing `Agents/Create/Action` behind console routes |
| Conversation CRUD/membership | existing collaboration operations plus owner-checked rename/remove |
| Timeline | bounded SQLite task pages ordered by durable task seq; full tasks, no duplicated message store |
| Human send | existing admission/scheduler with explicit `human:operator` origin, exact selected recipients; partial failures reported per recipient |
| Reconnect | single selected-conversation SSE invalidation feed using global event sequence, cursor replay and heartbeat; snapshot refetch recovers expired history |
| Task inspector/cancel/retry | existing task/event APIs; uncertain retry requires explicit warning/confirmation |
| Context search/get/attach | existing version-pinned context ACLs; recipient preflight remains server-side |
| Approvals | existing approval_required events denote denied actions; no allow endpoint exists, so UI never offers allow |

Tasks are the source of truth for human requests and native results. No synthetic
agent is registered for a human. Multi-target sends are individually admitted;
responses include admitted tasks and per-recipient errors. Native session isolation,
repository locks, cancellation and uncertain-execution semantics remain intact.

Navigation: Chat (default), Agents, Tasks, Contexts, Activity, Settings. Desktop:
rail | resizable inbox | conversation/composer | toggle inspector. Mobile: one
panel at a time, explicit back action, sticky composer with safe-area padding.

```
┌────────┬──────────────────┬───────────────────────────┬───────────────┐
│ Meshra │ Conversations  + │ Title / exact participants│ Task/context  │
│ Chat   │ Search           │                           │ provenance    │
│ Agents │ durable channels │ Human → selected agent    │               │
│ Tasks  │                  │ Native result / real state│               │
│ Context│                  │                           │               │
│ Activity                  │ To: exact ID · pinned refs│               │
│ Settings                  │ Message…              Send│               │
└────────┴──────────────────┴───────────────────────────┴───────────────┘
```

Tokens: 4/8px spacing; 14px UI sans, 13px mono IDs; 12/14/16/20/28px scale;
neutral slate surfaces, restrained teal accent, semantic success/warning/error;
full light/dark/system themes, visible keyboard focus, reduced-motion support.
No terminal, fake agents, charts or unsupported approval controls.
