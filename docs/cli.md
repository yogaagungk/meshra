# CLI, HTTP API, and MCP

## Build and daemon

Go 1.25 or newer is required by the SQLite dependency. Build with `go build -o meshra ./cmd/meshra`, then put the resulting executable on PATH or use `./meshra` in the examples below.

```sh
meshra --help
meshra serve
# Separate terminal:
meshra agent list --json
```

The daemon defaults to `127.0.0.1:7331`. `--addr` accepts only literal loopback IPs or `localhost`, never wildcard/non-loopback addresses; IPv6 loopback must be bracketed. The actual listening socket is checked too. SIGINT/SIGTERM triggers HTTP shutdown and relay Close. Request/header deadlines protect incoming requests; SSE streams have no blanket response deadline. The core owns the daemon lock, SQLite store, and administrator credential.

Global options work before or after commands: `--data-dir DIR`, `--addr HOST:PORT`, `--config FILE`, `--json`, `--help`/`-h`. The data directory defaults to `os.UserConfigDir()/meshra` (`$XDG_CONFIG_HOME/meshra` or `~/.config/meshra` on Linux). It must match between server and human client unless `MESHRA_TOKEN` supplies the credential. The CLI does not discover nondefault listening ports from the data directory.

`--config` reads an explicit JSON file with only optional `addr` and `dataDir` properties. No file is read implicitly; explicit CLI flags override file values, which override defaults. Relative data directories resolve against the current directory; shell-style `~` and environment expansion are not performed inside JSON. [config.example.json](../config.example.json) is usable as-is:

```sh
meshra --config config.example.json serve
meshra --config config.example.json agent list
meshra --data-dir /tmp/meshra-demo --addr 127.0.0.1:7441 serve
```

Credentials do not belong in this file. Harness provider/model are per-agent values, supplied at creation; no API keys are accepted in the relay configuration. Existing user harness configuration is never rewritten automatically.

Daemon-only reliability flags are `serve --retention DURATION`, `--max-delivery-attempts N`, `--retry-base DURATION`, and `--retry-cap DURATION`. Durations use Go syntax, for example `720h` (not `30d`). Defaults are 720 hours of terminal history, 16 offline delivery checks, a 1-second retry base, and a 30-second retry cap. Zero duration selects the default; negative retention disables pruning. The effective retry base must be at least 1ms, the cap must be at least the base and at most 1h, and delivery attempts must be 1..1000. For example:

```sh
meshra serve --retention 168h --max-delivery-attempts 12 --retry-base 1s --retry-cap 20s
```

Retention removes terminal tasks and their events together, never active work or ancestors needed by retained work. Pruned task IDs return 404 and their idempotency keys no longer deduplicate submissions. Backoff applies only before invocation when a destination is offline; it does not restart native runtimes or replay a turn after invocation. Structured lifecycle logs go to stderr without message payloads or tokens.

## Named agents and policy

Names are lowercase `[a-z][a-z0-9_-]{0,62}`. Canonical IDs are `<harness>:<name>`, with harness `omp`, `claude`, `codex`, or `opencode`. Each identity owns a separate native session. Cwd must already exist; use independent repositories/worktrees when parallel writes are needed.

```sh
meshra agent create --harness claude --name exampleone --cwd "$HOME/projects/exampleone" \
  --allow-to codex:reviewer --allow-operation list,status,request,events,cancel --delegate
meshra agent create --harness claude --name exampletwo --cwd "$HOME/projects/exampletwo"
meshra agent create --harness omp --name exampleworker --cwd "$HOME/projects/exampleworker" \
  --allow-to claude:exampleone,claude:exampletwo \
  --allow-operation list,status,send,request,events,cancel
meshra agent create --harness codex --name reviewer --cwd "$HOME/projects/review" \
  --model MODEL_ID --provider PROVIDER_ID
meshra agent list
meshra agent status claude:exampleone
meshra agent start claude:exampleone
meshra agent restart claude:exampleone
meshra agent stop claude:exampleone
meshra agent remove claude:exampletwo
```

`--display-name` is optional. `--allow-to` and `--allow-operation` accept repeated flags and comma-separated values. Operations are exactly `list`, `status`, `send`, `request`, `events`, `cancel`; destinations may include `*`. Empty outbound policy grants no agent operations/destinations. `--delegate` authorizes nested delegation subject to ancestry/hop checks; it does not bypass destination/operation permissions. Creation/lifecycle/removal/token issuance are administrator operations. Start reports native harness setup failures; installing the binary alone does not authenticate the harness. Removed identity names cannot be reused.

Recipient-side `--allow-from` and `--allow-inbound-operation` also accept repeated/comma-separated values. Omit a flag to preserve legacy inbound access; explicitly supply `--allow-from=` or `--allow-inbound-operation=` to deny all senders or inbound operations respectively. For example, `--allow-from omp:exampleworker --allow-inbound-operation request` allows only requests from that sender, still subject to its outbound policy. In CreateAgent JSON these are `policy.senders` and `policy.inboundOperations`: omitted/null means legacy behavior, `[]` means deny. `--worktree-writes` sends `policy.worktreeWrites:true`; the server rejects this request when the adapter cannot enforce approvals. Relaying never grants native tool approval or enables worktree writes.

The human CLI uses `MESHRA_TOKEN` when set, otherwise reads `DATA_DIR/admin.token`. Running `meshra agent token ID` prints a sensitive agent bearer credential (a JSON object under `--json`). Do not paste it into arguments, logs, checked-in files, or another identity's environment. To launch an independent manual harness as a particular identity, first obtain that agent's token in an administrator terminal, then give only that token to that harness's environment. Clear any inherited administrator `MESHRA_TOKEN` before selecting an agent token.

## Messaging and task results

```sh
meshra agent start omp:exampleworker
meshra send --from omp:exampleworker --to claude:exampleone --message 'Review the API contract' \
  --idempotency-key review-api-001 --timeout 5m --json
meshra request --from omp:exampleworker --to claude:exampleone --message 'Return a brief review' --timeout 300
meshra request --from omp:exampleworker --to claude:exampleone --message 'Return a brief review' \
  --timeout 5m --wait --json
meshra task status TASK_ID --json
meshra task trace TASK_ID --json
meshra task events TASK_ID --follow
meshra task events TASK_ID --after 42 --follow --json
meshra task cancel TASK_ID
meshra task retry TASK_ID
```

By default both send and request return the complete persistent task immediately, including `taskId`, `messageId`, `correlationId`, origin/session metadata, destination, and state. Neither waits for a harness turn unless request explicitly opts into `--wait`. `--timeout` accepts positive seconds or Go durations (duration form capped at 24h); `--parent-task` and `--max-hops` express delegation. An administrator supplies `--from`; an agent credential derives its identity from authentication and cannot impersonate another agent. A per-turn credential carries its ancestry in the principal even if no parent flag is sent.

`request --wait` requires explicit `--timeout` between 1 and 86400 seconds (durations are rounded up to seconds). The request is durably admitted first, then the HTTP transport polls the task until terminal or its persisted deadline. A parent deadline or existing idempotent task can make that bound earlier than the newly supplied timeout. Terminal output is the same Task object: successful replies use `state:"completed"` and `result`; unsuccessful replies use `state:"failed"`, `"cancelled"` or `"timed_out"`, with `error`/`errorCategory` when available. Terminal failure is still a successful HTTP exchange and CLI exit 0: callers must inspect `state`. If the scheduler has not persisted terminal state by the deadline, output is the current nonterminal Task, not a fabricated timeout. The CLI bounds the HTTP context to the requested timeout plus 5 seconds of transport allowance; disconnects/signals stop waiting, not the durable task. Inspect or explicitly cancel by task ID afterward. Agent callers need `status` as well as `request` permission to wait.

States are `queued → dispatching → running → completed | failed | cancelled | timed_out`. Admission is not completion. Offline agents retain queued work until started, delivery checks are exhausted, or the deadline expires. Administrator-only retry of uncertain/failed work can duplicate external side effects; inspect `ambiguous` and `errorCategory` first. Idempotency keys deduplicate matching admission within the retained sender/session/operation scope, not exactly-once harness execution; conflicting reuse returns 409. Consult [protocol.md](protocol.md) for ownership and recovery rules.

Default output is readable indented JSON, except token output and followed events. `--json` emits complete compact JSON; followed events are one complete event per line (JSONL). Exit codes: 0 success, 1 operation/HTTP failure, 2 invalid usage/configuration. A successful asynchronous send does not imply its task will succeed: inspect terminal state/result/error.

### Operator inspection

```sh
meshra task trace TASK_ID --json
meshra metrics --json
meshra doctor --json
```

Trace returns `{"task":Task,"events":[Event,...],"eventsTruncated":false|true}`, with the latest 256 retained events and persisted lifecycle metadata, subject to task ownership and both status/events permissions. Each trace data field is limited to 2048 bytes with an explicit marker; use cursor-paginated events for complete retained data. Metrics and doctor require administrator credentials. Metrics return `queued`, `inFlight`, `succeeded`, `failed`, `retried`, `expired`, `offlineAgents`, `oldestQueueSeconds`, and `averageLatencySeconds`; these describe retained history/current state, not lifetime totals. `retried` counts retained messages with an execution retry or any scheduled offline retry. Doctor returns `{"healthy":true|false,"checks":[{"name":"...","status":"...","detail":"..."}]}` without credential values. CLI exit status indicates transport success; inspect `healthy`. Native credential validity is reported as unknown rather than inferred from executable presence.

## Authenticated HTTP API

Every endpoint, including MCP and identity, requires `Authorization: Bearer TOKEN`. No credentials are accepted in URLs. Every request with an `Origin` header is rejected with 403 (including `null`); browser callers are deliberately unsupported. HTTP clients must not add Origin. Responses disable caching. Requests with JSON bodies require `Content-Type: application/json`; unknown fields, malformed/trailing JSON values, and bodies over 128 KiB are rejected. Task text itself is bounded to 64 KiB by the application service.

| Method | Path | Body/result |
| --- | --- | --- |
| GET | `/v1/identity` | Authenticated `admin`, `agentId`, `sessionId`, `taskId` (no token) |
| GET | `/v1/agents` | Agent array |
| POST | `/v1/agents` | CreateAgent object → complete Agent |
| GET | `/v1/agents/{id}` | Complete Agent |
| DELETE | `/v1/agents/{id}` | `{"removed":true}` |
| POST | `/v1/agents/{id}/start`, `/stop`, `/restart` | Complete Agent |
| POST | `/v1/agents/{id}/token` | `{"token":"…"}`; sensitive |
| POST | `/v1/send`, `/v1/request` | SendRequest → complete queued Task |
| GET | `/v1/tasks/{taskId}` | Complete Task |
| GET | `/v1/tasks/{taskId}/trace` | `{"task":Task,"events":[Event,…],"eventsTruncated":bool}`; latest 256 events, task status/events authorization |
| GET | `/v1/tasks/{taskId}/events?after=N` | Up to 256 Events with IDs strictly greater than N; continue using the last returned ID |
| GET | `/v1/tasks/{taskId}/events?after=N&follow=true` | Persistent replay plus live SSE |
| POST | `/v1/tasks/{taskId}/cancel`, `/retry` | Complete Task |
| GET | `/v1/metrics` | Metrics object; administrator only |
| GET | `/v1/doctor` | Diagnostics object; administrator only |
| POST | `/mcp` | Stateless MCP JSON-RPC |

CreateAgent example (provider/model may be omitted):

```json
{"harness":"omp","name":"source","cwd":"/absolute/existing/repository","config":{"model":"MODEL_ID","provider":"PROVIDER_ID"},"policy":{"destinations":["claude:target"],"operations":["list","status","send","request","events","cancel"],"delegate":true}}
```

SendRequest properties: `from` (administrator-selected or matching authenticated agent), required `to`, required `text`, optional `idempotencyKey`, `timeoutSeconds`, `parentTaskId`, `maxHops`. The endpoint determines `type` (`send` or `request`). The service authenticates ownership/ancestry, never a caller-provided origin record. Responses are JSON: 201 for agent creation, 202 for admitted asynchronous send/request tasks, otherwise 200 except the opt-in waiting semantics below. Failures are `{"error":"actionable explanation"}` with the model service's status (400/401/403/404/409/413/429/503 as applicable); internal errors return a generic 500. Invalid methods use 405 and Allow; unsupported JSON media types use 415.

`POST /v1/request?wait=true` is the only synchronous opt-in. It requires an explicit integer `timeoutSeconds` in 1..86400, otherwise returns 400 before admission. Omitted `wait` or `wait=false` preserves immediate 202; other nonempty values return 400. Waiting uses the persisted task deadline and request context, without cancelling on disconnect. A terminal task returns HTTP 200 and `X-Meshra-Wait-Status: terminal`, including failed/cancelled/timed-out tasks; a deadline reached before a terminal transition is persisted returns HTTP 202, the latest Task, and `X-Meshra-Wait-Status: deadline`. A 202 is not a completed reply: continue status/events inspection. No wrapper or parallel reply protocol is introduced.

For example, a successful terminal reply includes `{"taskId":"...","state":"completed","result":"review text",...}`; a failed terminal reply includes `{"taskId":"...","state":"failed","error":"...","errorCategory":"...",...}`. These illustrative field subsets are not complete Task objects. Admission/authorization errors retain the top-level `{"error":"..."}` shape and non-2xx status.

### SSE replay

SSE uses `Content-Type: text/event-stream`. Each event has `id: <persistent numeric ID>`, `event: message`, and `data: <complete Event JSON>`. Event properties are `id`, `taskId`, `type`, `data` (string), `time`, and additive correlation/session/sender/recipient/attempt metadata (`messageId`, `correlationId`, `from`, `to`, `harness`, `sessionId`, `attempt`) when available. JSON encoding safely escapes embedded newlines. Event IDs are durable global cursors, not array indices; gaps from other tasks or retention are valid. A reconnect's `Last-Event-ID` header overrides `after`; omit both to replay from zero. Persist the last *fully processed* ID and reconnect with it. Delivery can repeat on client crashes; deduplicate by ID. No in-memory subscription is the source of truth: polling reads the persistent event store, preventing disconnect/reconnect gaps within retained history.

Follow replays all persisted pages before streaming new events, emits `: heartbeat` comments every 15 seconds during quiet periods, closes after the terminal state and final persisted events are drained, and stops on request context cancellation. Non-follow returns one page of up to 256 events and closes immediately; repeat with the last returned ID until empty. The MCP events tool has the same page size. A network/error disconnect is not proof of completion: reconnect with the last cursor and inspect task status. The CLI exposes the reconnect cursor as `--after` (it does not silently retry).

For credential-safe persistence, native text deltas are accumulated up to 256 KiB, redacted together, and emitted in bounded chunks only on successful completion. Progress/session/approval events remain immediate; failed/cancelled partial text is discarded. This necessary v0.1 timing change prevents reconstructing credentials split across native chunks. Chunk count/timing is not a stable contract; concatenate successful `delta` data and inspect terminal state/result.


Reading event pages requires the agent's `events` operation. Follow also checks `status` to determine when the task is terminal; grant both for streaming clients.

## MCP contract

`POST /mcp` implements stateless [Streamable HTTP](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports). Send `Accept: application/json, text/event-stream`, JSON Content-Type, and the bearer token on **every** request. Initialize negotiates `2025-03-26`, `2025-06-18`, or `2025-11-25`; unsupported requested versions receive the latest supported version. Subsequent requests can send `MCP-Protocol-Version`; unsupported headers receive 400. There is no MCP session cookie/ID, OAuth endpoint, server-initiated request, or MCP GET event stream (GET returns 405). Accepted notifications return an empty 202. JSON-RPC methods: `initialize`, `ping`, `tools/list`, `tools/call`. Requests return JSON, not MCP SSE; task SSE is the separate authenticated REST endpoint above.

| Tool | Arguments | Result |
| --- | --- | --- |
| `meshra_agents_list` | `{}` | `{agents:[Agent,…]}` |
| `meshra_agent_status` | `{id}` | Agent |
| `meshra_agent_register` | CreateAgent object (`harness,name,cwd` required) | Agent; administrator only, does not start runtime |
| `meshra_send` | `{to,text,idempotencyKey?,timeoutSeconds?,parentTaskId?,maxHops?}` | Task, immediately |
| `meshra_request` | Same as send | Task, immediately; poll for reply |
| `meshra_task_status` | `{taskId}` | Task |
| `meshra_task_events` | `{taskId,after?}` | `{events:[Event,…]}`; no blocking/follow |
| `meshra_task_cancel` | `{taskId}` | Task |
| `meshra_task_trace` | `{taskId}` | `{task:Task,events:[Event,…],eventsTruncated:boolean}`; latest 256 events |
| `meshra_task_retry` | `{taskId}` | Task; administrator only, repeated effects possible |
| `meshra_metrics` | `{}` | Metrics; administrator only |
| `meshra_doctor` | `{}` | Diagnostics; administrator only |

The server advertises version `0.2.0`; all seven v0.1 tool names remain available. Tools advertise object JSON Schemas with explicit properties, required fields and `additionalProperties:false`. Message tools deliberately omit and reject `from`: only the authenticated principal identifies the caller. Registration accepts the same nested config/policy as CreateAgent, including explicit-empty inbound lists. Successful calls include JSON text content plus `structuredContent`; tool validation/service errors return text content with `isError:true`. MCP requests stay asynchronous so delegation never holds an HTTP request awaiting a nested turn; HTTP/CLI waiting is separately opt-in. Administrator tools are available through authenticated HTTP MCP only, not the agent-only stdio proxy.

### Optional stdio proxy

```sh
# In an environment containing only this agent's credential:
meshra mcp stdio --addr 127.0.0.1:7331
```

The proxy requires `MESHRA_TOKEN`, never reads `admin.token`, checks `/v1/identity`, and refuses administrator credentials. Stdin/stdout contain newline-delimited JSON-RPC only; diagnostics go to stderr. Notifications produce no stdout. No token is placed in command arguments or emitted as a protocol frame.

The proxy accepts responses up to 32 MiB because JSON-escaped Task/Trace can appear in both text and structured content. Requests remain bounded to 128 KiB. Oversized/invalid responses fail explicitly; this is not an unbounded buffering proxy.


## Manual harness MCP registration

The following configuration **formats** were inspected; no authenticated external harness integration is implied. Configure only the intended per-instance profile/project, not a globally shared token. Meshra adapters may provide per-turn MCP wiring themselves; do not replace that with a permanent identity token for nested delegated runs. A manually connected interactive harness has the permissions of its chosen agent token, not arbitrary access to relay-owned sessions.

First, in a human administrator shell, capture a token without putting it in process arguments:

```sh
export MESHRA_TOKEN="$(meshra agent token omp:exampleworker)"
# Launch only the intended source harness from this environment.
```

**Codex:** installed `codex mcp add --help` confirms URL plus bearer-token environment support:

```sh
codex mcp add meshra --url http://127.0.0.1:7331/mcp --bearer-token-env-var MESHRA_TOKEN
```

This registration changes Codex configuration only when you run it yourself. Ensure that configuration is scoped to the intended Codex instance and that each launched process has its own agent token. A stdio alternative supported by the same help is `codex mcp add meshra -- meshra mcp stdio`; the child must inherit the agent-token environment.

**Claude Code:** the [official MCP reference](https://code.claude.com/docs/en/mcp) documents stdio command registration. To avoid putting a bearer token in `--header` arguments, use the proxy and an inherited agent-token environment:

```sh
claude mcp add --transport stdio meshra -- meshra mcp stdio
```

This is documentation-verified syntax, not a locally authenticated Claude test. Select the appropriate Claude profile/project scope yourself; inspect `claude mcp get meshra` and `/mcp` after launch.

**OMP:** the installed package's `src/config/mcp-schema.json` explicitly supports project `.omp/mcp.json`/`.mcp.json` and `mcpServers` with stdio `command`/`args`. Manually merge into the intended project's `.omp/mcp.json`, leaving the token only in its launch environment:

```json
{"mcpServers":{"meshra":{"type":"stdio","command":"meshra","args":["mcp","stdio"]}}}
```

Schema reference: [upstream OMP MCP schema](https://github.com/can1357/oh-my-pi/blob/main/packages/coding-agent/src/config/mcp-schema.json). `omp mcp --help` did not expose a dedicated registration subcommand in the inspected installation; do not assume `omp mcp add` exists.

**OpenCode:** the [official MCP configuration reference](https://opencode.ai/docs/mcp-servers/) documents `mcp.<name>.type=remote`, `url`, `headers`, `oauth:false`, and `{env:VAR}` expansion. Manually merge into the intended OpenCode configuration:

```json
{"mcp":{"meshra":{"type":"remote","url":"http://127.0.0.1:7331/mcp","oauth":false,"headers":{"Authorization":"Bearer {env:MESHRA_TOKEN}"}}}}
```

OpenCode also documents local `{"type":"local","command":["meshra","mcp","stdio"]}` under the same `mcp.meshra` key. These are documentation-verified formats, not a locally authenticated OpenCode test. Do not commit credentials or overwrite unrelated settings when merging.

## Checks

API/CLI tests exercise bounded JSON, bearer/Origin isolation, administrator access, trace authorization, explicit-bound waiting and disconnect/deadline behavior, SSE cursors/terminal replay, loopback/configuration validation, explicit-empty inbound policy, full JSON output, and agent-only stdio protocol handling. A real-relay proxy regression covers large JSON-escaped traces. Relay and HTTP end-to-end tests cover actual ownership, audited impersonation rejection, exact instance routing, retained pagination and cancellation. Run `go test -race ./...`; the actual executable/MCP smoke and native/provider results are in [V0.2_TEST_REPORT.md](V0.2_TEST_REPORT.md).
