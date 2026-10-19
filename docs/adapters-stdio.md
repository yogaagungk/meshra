# OMP and Codex stdio adapters

## Compatibility evidence and verification status

Implementation inspection used installed `omp/18.12.1` and `codex-cli 0.163.0`. Commands inspected: `omp --version`, `omp --help`, `codex --version`, and `codex mcp add --help`. Both opt-in authenticated smoke tests passed. Live CLI OMP ↔ Codex routing and Codex resume/queued restart also passed after fixing stale-turn event correlation. Exact outcomes and verification boundaries are recorded in [verification.md](verification.md); destructive-tool cancellation and native MCP are not claimed.

References consulted before choosing protocol methods:

- [OMP official RPC reference](https://raw.githubusercontent.com/can1357/oh-my-pi/main/docs/rpc.md).
- [OMP settings and discovery-source controls](https://raw.githubusercontent.com/can1357/oh-my-pi/main/docs/settings.md).
- [OMP approval semantics](https://raw.githubusercontent.com/can1357/oh-my-pi/main/docs/approval-mode.md).
- [Codex App Server](https://developers.openai.com/codex/app-server/).
- [Codex MCP configuration](https://developers.openai.com/codex/mcp/).
- Installed-version Codex generated schemas in `/tmp/meshra-codex-schema`, including `v1/InitializeParams.json`, `v2/{ThreadStartParams,ThreadResumeParams,TurnStartParams,ConfigReadParams,ConfigReadResponse}.json` and approval response schemas. This directory is inspection evidence, not a runtime dependency. Regenerate with `codex app-server generate-json-schema --out /tmp/meshra-codex-schema`.

The installed OMP bundled source was also inspected: `--no-tools` sets the builtin tool list to empty, but MCP discovery/refresh is a separate path. Consequently the adapter additionally disables discovery sources with its own process-local settings overlay; it does not rely on `--no-tools` alone.

| Feature | OMP | Codex |
| --- | --- | --- |
| Native transport | custom JSONL RPC, **not** JSON-RPC | App Server JSONL, JSON-RPC-shaped without `jsonrpc` header |
| Prompt / text stream | yes | yes |
| Owned native session resume | exact persisted session ID | exact persisted thread ID |
| Cancel | `abort`, then owned process teardown | `turn/interrupt`, then owned process teardown |
| Workspace writes | builtin and discovered tools disabled | read-only native sandbox, network disabled |
| Interactive approvals | no | no; explicit denial events and responses |
| Automatic Meshra MCP | **no** | **no** |
| Attach arbitrary interactive session | no | no |

## Ownership, bounds, shutdown

Both adapters implement `New() model.Adapter`. Factories use the daemon-lifetime context; each `Run` uses its task context. Startup is bounded to 20 seconds plus bounded teardown. A native ID is emitted as `session` as soon as the selected session is verified; text uses `delta`. No raw protocol frames, model thinking, tool arguments, native stderr, or relay credentials are emitted. Provider-supplied error messages remain untrusted user-visible data.

Native state lives below `os.UserConfigDir()/meshra/runtimes/<harness>/<sha256(canonical-agent-ID)>/`, directories created with mode 0700. It is deliberately not a guessed latest-session directory. An exclusive Unix flock prevents two live owners of the same identity's native state, including daemons with different relay data directories. The shared Agent contract has no data-directory parameter, so changing `meshra`'s relay data directory does not relocate this native state. Preserve native state alongside relay backups. Different canonical identities get different state directories, subprocesses, and native sessions.

Stdout physical JSONL frames and stdin commands are bounded to 1 MiB. The transport buffers at most 64 physical frames. OMP logical reassembly is capped at 4 MiB, below OMP's advertised 64 MiB maximum: larger frames fail explicitly, not silently truncate. Meshra text results are bounded to 64 KiB. Invalid JSON, oversized frames, malformed/interleaved chunks, process exit, and EOF fail the active operation.

Each child has its own Unix process group. Shutdown closes stdin while continuing to drain stdout, gives 500 ms for native cleanup, kills the remaining group, and waits/reaps the direct process. `Close` is idempotent. Cancellation attempts the native abort/interrupt with a 200 ms bound and then closes the entire runtime, so no background task can continue into a subsequent Meshra turn. After cancellation or a protocol/turn error, explicitly restart the agent before further execution; restarting resumes its stored native session, not a different session. This process helper targets Linux/macOS Unix process groups; Windows native support is not implemented.

## OMP protocol

Launch shape:

```text
omp --mode=rpc --no-ui --no-tools --no-extensions --no-skills --no-rules \
    --no-lsp --no-pty --no-title --approval-mode=always-ask \
    --config=<owned-directory>/safety.yml --session-dir=<owned-directory>/sessions
```

Provider/model, when supplied, use installed `--provider=` and `--model=` flags. Authentication and model defaults otherwise use native OMP configuration/environment. User configuration is not edited. The owned `safety.yml` disables discovery IDs `native`, `claude`, `claude-plugins`, `codex`, `gemini`, `github`, `opencode`, `cursor`, `windsurf`, and `agents-md`, and sets `mcp.enableProjectConfig: false`. The native `always-ask` mode is defense in depth, not an auto-approval grant. No API key appears on a command line.

The factory requires the initial `ready` frame to advertise protocol 2 and sends correlated `negotiate_protocol`. It intentionally does **not** fall back to v1: v1 may elide oversized event data without a lossless client-side reconstruction. Every response is correlated by command ID and checked against the command name. New sessions explicitly issue `new_session`; resume launches with only `--resume=<persisted-native-ID>`. `get_state.sessionId` must match the requested ID on resume, and `sessionFile` must be contained in the identity-owned session directory.

`set_event_filter` selects `message_update` with `messageUpdates: "delta"`. Only `assistantMessageEvent.type == "text_delta"` becomes visible text. Prompt admission (`response.success`) is not completion: the adapter waits for its correlated `prompt_result`, checks `status` (`completed`, `aborted`, `error`), and additionally waits for quiescence if `sessionSettled` is false. An `agent_end` by itself cannot complete a task. Provider failures may arrive after successful admission and are errors, not fabricated successful answers. Messages are prefixed with task metadata, preventing a user-supplied leading slash from being treated as an OMP command.

Protocol-v2 chunks require a nonempty chunk ID, contiguous indexes, consistent count and byte length, no intervening frames, strict base64, exact final length, valid UTF-8, and one valid decoded JSON object. Nested chunks and overflow/error frames fail closed.

## Codex protocol

Launch shape:

```text
CODEX_HOME=<owned-directory> codex app-server --listen stdio://
```

The process starts in its owned directory, then the explicit thread/turn `cwd` selects the registered workspace. An isolated `CODEX_HOME` avoids inheriting the user's MCP servers, plugins, hooks, and sandbox defaults. If present, native `$CODEX_HOME/auth.json` (otherwise `~/.codex/auth.json`) is symlinked into the owned home; no auth contents are copied into relay records/events. Native auth refresh may replace the owned link with its own refreshed auth file. Native environment authentication is inherited. Native keychain/provider configurations that depend on a custom user `config.toml` are not imported; custom provider definitions may therefore be unavailable even when `modelProvider` is set. Authenticate natively or use supported native environment credentials. Do not manually add unsafe config to the owned home.

The factory sends `initialize` with Meshra client metadata, awaits its response, then notifies `initialized`. No experimental capability is requested. `config/read` resolves effective configuration for the registered workspace; nonempty `mcp_servers`, `hooks`, or `plugins` settings are rejected before starting a thread because a filesystem sandbox does not contain their external effects. `thread/start` or exact-ID `thread/resume` sets `sandbox: "read-only"` and `approvalPolicy: "on-request"`. Resume sets `excludeTurns: true` to avoid hydrating the whole transcript.

Every `turn/start` explicitly supplies the selected thread ID, workspace, text input, `approvalPolicy: "on-request"`, and `sandboxPolicy: {"type":"readOnly","networkAccess":false}`. These spellings come from the installed 0.163.0 schema. In particular, newer online examples showing `readOnly.access` were not copied into the installed-version request.

The event loop handles notifications that arrive before the `turn/start` response, verifies native turn ownership, filters thread IDs, streams `item/agentMessage/delta`, and reconciles final `item/completed` agent-message text. Only `turn/completed` with status `completed` succeeds. `failed` includes `turn.error.message`; `interrupted` is an error. Nonretryable `error` notifications also fail execution. Admission responses and streamed partial text never imply success.

Server-initiated requests always receive a response:

- `item/commandExecution/requestApproval` and `item/fileChange/requestApproval`: `decision: "decline"`.
- `item/permissions/requestApproval`: empty granted `permissions`, `scope: "turn"`.
- `mcpServer/elicitation/request`: `action: "decline"`.
- Unknown requests: JSON-RPC error `-32601`, never a guessed affirmative payload.

Denial emits an `approval_required` event naming only the method, not raw command text or secrets. There is currently no human approve endpoint for these adapters. Native sandbox support must be available; the adapter does not switch to an unrestricted sandbox when sandbox initialization fails.

## MCP limits and manual integration

These adapters do not advertise agent-initiated delegation. The reviewed stable transports do not supply a verified mechanism to replace an authenticated Meshra MCP connection for **each turn**, preserving the task-bound token and task ancestry. Factory-lifetime/global MCP configuration is not equivalent. `Turn.MCPURL` and `Turn.Token` are therefore never sent to these subprocesses. A progress event explains the limitation when an MCP URL is present.

For a separately human-managed Codex session, installed native registration supports:

```bash
# Choose a separate home: this command writes only that home, not the user's config.
export CODEX_HOME="$(mktemp -d)"
read -rsp 'Agent bearer token: ' MESHRA_AGENT_TOKEN; echo
export MESHRA_AGENT_TOKEN
codex mcp add meshra --url http://127.0.0.1:7331/mcp \
  --bearer-token-env-var MESHRA_AGENT_TOKEN
codex
unset MESHRA_AGENT_TOKEN
```

Replace the URL with the actual daemon URL and obtain an identity-bound **agent** token through Meshra's documented token command. Never use an administrator token for a harness. Authenticate this separate Codex home natively if needed. This is a manual terminal session, **not** a way to attach the adapter to that session. A stable agent token has no active parent task; do not claim task-bound ancestry or substitute it for the adapter's per-turn token. Configuration changes here are explicit human actions, not performed automatically by Meshra.

For separately managed OMP sessions, consult the linked native MCP documentation/settings before configuring `mcp.json`; the Meshra-owned tool-free process intentionally disables that discovery. No OMP MCP registration command or per-turn configuration API is invented here. A human may route messages through the Meshra CLI while an OMP response is running, but that is human-originated routing, not autonomous MCP delegation.

## Reproducible checks

Deterministic tests use actual child executables (test-binary shims), without model credentials. They exercise ready/initialization, correlated responses, async events, OMP chunk reconstruction, prompt/turn terminal errors, EOF/process failure, context cancellation, approval denial, native resume mismatch, independent ownership, and transport bounds/reaping.

Run after integration:

```bash
go test ./internal/adapters/process ./internal/adapters/omp ./internal/adapters/codex
# Optional concurrency/race checking:
go test -race ./internal/adapters/process ./internal/adapters/omp ./internal/adapters/codex
```

Opt-in real smoke tests use existing native credentials and **may incur model charges**. They create uniquely named relay-owned native sessions, request a short marker response, close, and resume the exact native ID. They leave native transcript/state for inspection. Run one harness at a time on constrained WSL machines:

```bash
MESHRA_REAL_OMP=1 go test ./internal/adapters/omp -run '^TestRealSmoke$' -count=1 -v
MESHRA_REAL_CODEX=1 go test ./internal/adapters/codex -run '^TestRealSmoke$' -count=1 -v
```

Optional model selections are `MESHRA_OMP_MODEL`, `MESHRA_OMP_PROVIDER`, `MESHRA_CODEX_MODEL`, and `MESHRA_CODEX_PROVIDER`. No model name or paid access is assumed. A failed authentication, unsupported model, missing sandbox, or missing native session must be reported as a failed smoke test with its actual error, never replaced with mock success.
