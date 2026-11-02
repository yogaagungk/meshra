# OMP stdio adapter

## Compatibility evidence and verification status

Both opt-in authenticated smoke tests passed. Exact outcomes and verification boundaries are recorded in [verification.md](verification.md); destructive-tool cancellation and native MCP are not claimed.

The [v0.2 inspection](V0.2_ADAPTER_VERIFICATION.md) reconfirmed those installed versions and regenerated version-matched schemas without running Go checks or provider turns.

References consulted before choosing protocol methods:

- [OMP official RPC reference](https://raw.githubusercontent.com/can1357/oh-my-pi/main/docs/rpc.md).
- [OMP settings and discovery-source controls](https://raw.githubusercontent.com/can1357/oh-my-pi/main/docs/settings.md).
- [OMP approval semantics](https://raw.githubusercontent.com/can1357/oh-my-pi/main/docs/approval-mode.md).

The installed OMP bundled source was also inspected: `--no-tools` sets the builtin tool list to empty, but MCP discovery/refresh is a separate path. Consequently the adapter additionally disables discovery sources with its own process-local settings overlay; it does not rely on `--no-tools` alone.

| Feature | OMP |
| --- | --- |
| Native transport | custom JSONL RPC, **not** JSON-RPC |
| Prompt / text stream | yes |
| Owned native session resume | exact persisted session ID |
| Cancel | `abort`, then owned process teardown |
| Workspace writes | builtin and discovered tools disabled |
| Interactive approvals | no |
| Automatic Meshra MCP | **no** |
| Attach arbitrary interactive session | no |

## Ownership, bounds, shutdown

Both adapters implement `New() model.Adapter`. Factories use the daemon-lifetime context; each `Run` uses its task context. Startup is bounded to 20 seconds plus bounded teardown. A native ID is emitted as `session` as soon as the selected session is verified; text uses `delta`. No raw protocol frames, model thinking, tool arguments, native stderr, or relay credentials are emitted. Provider-supplied error messages remain untrusted user-visible data.

Native state lives below `os.UserConfigDir()/meshra/runtimes/<harness>/<sha256(canonical-agent-ID)>/`, directories created with mode 0700. It is deliberately not a guessed latest-session directory. An exclusive Unix flock prevents two live owners of the same identity's native state, including daemons with different relay data directories. The shared Agent contract has no data-directory parameter, so changing `meshra`'s relay data directory does not relocate this native state. Preserve native state alongside relay backups. Different canonical identities get different state directories, subprocesses, and native sessions.

Stdout physical JSONL frames and stdin commands are bounded to 1 MiB. The transport buffers at most 64 physical frames. OMP logical reassembly is capped at 4 MiB, below OMP's advertised 64 MiB maximum: larger frames fail explicitly, not silently truncate. Meshra text results are bounded to 64 KiB. Invalid JSON, oversized frames, malformed/interleaved chunks, process exit, and EOF fail the active operation.

Each child has its own Unix process group. Shutdown closes stdin while continuing to drain stdout, gives 500 ms for native cleanup, kills the remaining group, and waits/reaps the direct process. `Close` is idempotent. Cancellation attempts the native abort/interrupt with a 200 ms bound and then closes the entire runtime before another Meshra turn can reuse it. This does not guarantee rollback of provider/tool effects, cancellation of remote services, or termination of deliberately detached processes. After cancellation or a protocol/turn error, explicitly restart the agent before further execution; restarting resumes its stored native session, not a different session. This process helper targets Linux/macOS Unix process groups; Windows native support is not implemented.

## OMP protocol

Launch shape:

```text
omp --mode=rpc --no-ui --no-tools --no-extensions --no-skills --no-rules \
    --no-lsp --no-pty --no-title --approval-mode=always-ask \
    --config=<owned-directory>/safety.yml --session-dir=<owned-directory>/sessions
```

Provider/model, when supplied, use installed `--provider=` and `--model=` flags. Authentication and model defaults otherwise use native OMP configuration/environment. User configuration is not edited. The native `always-ask` mode is defense in depth, not an auto-approval grant. No API key appears on a command line.

The factory requires the initial `ready` frame to advertise protocol 2 and sends correlated `negotiate_protocol`. It intentionally does **not** fall back to v1: v1 may elide oversized event data without a lossless client-side reconstruction. Every response is correlated by command ID and checked against the command name. New sessions explicitly issue `new_session`; resume launches with only `--resume=<persisted-native-ID>`. `get_state.sessionId` must match the requested ID on resume, and `sessionFile` must be contained in the identity-owned session directory.

`set_event_filter` selects `message_update` with `messageUpdates: "delta"`. Only `assistantMessageEvent.type == "text_delta"` becomes visible text. Prompt admission (`response.success`) is not completion: the adapter waits for its correlated `prompt_result`, checks `status` (`completed`, `aborted`, `error`), and additionally waits for quiescence if `sessionSettled` is false. An `agent_end` by itself cannot complete a task. Provider failures may arrive after successful admission and are errors, not fabricated successful answers. Messages are prefixed with task metadata, preventing a user-supplied leading slash from being treated as an OMP command.

Protocol-v2 chunks require a nonempty chunk ID, contiguous indexes, consistent count and byte length, no intervening frames, strict base64, exact final length, valid UTF-8, and one valid decoded JSON object. Nested chunks and overflow/error frames fail closed.

## MCP limits and manual integration

These adapters do not advertise agent-initiated delegation. The reviewed stable transports do not supply a verified mechanism to replace an authenticated Meshra MCP connection for **each turn**, preserving the task-bound token and task ancestry. Factory-lifetime/global MCP configuration is not equivalent. `Turn.MCPURL` and `Turn.Token` are therefore never sent to these subprocesses. A progress event explains the limitation when an MCP URL is present.

## Reproducible checks

Deterministic tests use actual child executables (test-binary shims), without model credentials. They exercise ready/initialization, correlated responses, async events, OMP chunk reconstruction, prompt/turn terminal errors, EOF/process failure, context cancellation, approval denial, native resume mismatch, independent ownership, and transport bounds/reaping.

Run after integration:

```bash
go test ./internal/adapters/process ./internal/adapters/omp
# Optional concurrency/race checking:
go test -race ./internal/adapters/process ./internal/adapters/omp
```

Opt-in real smoke tests use existing native credentials and **may incur model charges**. They create uniquely named relay-owned native sessions, request a short marker response, close, and resume the exact native ID. They leave native transcript/state for inspection. Run one harness at a time on constrained WSL machines:

```bash
MESHRA_REAL_OMP=1 go test ./internal/adapters/omp -run '^TestRealSmoke$' -count=1 -v
```

No model name or paid access is assumed. A failed authentication, unsupported model, missing sandbox, or missing native session must be reported as a failed smoke test with its actual error, never replaced with mock success.
