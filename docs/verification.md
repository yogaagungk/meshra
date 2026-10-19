# Executed verification

This records observed results, not planned integrations. Platform: Linux x64 in WSL2; Go 1.27.2, Node 24.22.0, npm 11.19.1. Provider credentials were never printed or stored in Meshra's SQLite records.

## Deterministic and executable checks

The final implementation passed:

```sh
go test -race ./...
go vet ./...
go build -o meshra ./cmd/meshra
node --check bridges/claude-agent-sdk/bridge.mjs
node scripts/smoke.mjs
gofmt -l cmd internal tests
```

The formatting check printed nothing. All tested Go packages passed, including all four adapter contracts, process transport, relay, HTTP/MCP, CLI and local API end-to-end tests. The model package contains domain contracts and no separate test file. Opt-in authenticated tests skip by default and are not counted as deterministic native integration.

The real-executable credential-free smoke printed:

```json
{"smoke":"passed","registeredAgents":5,"independentClaudeIdentities":2,"queuedRestartRecovery":true,"duplicateAdmissionDeduplicated":true,"invalidCredentialStatus":401,"providerTurns":0}
```

This smoke launches the built daemon twice, invokes the real CLI, preserves a queued task across restart, verifies exact destination identity and zero dispatch attempts, checks duplicate admission and invalid authentication, cancels the task, shuts down and removes its temporary state. It does not pretend to execute model responses.

Permanent acceptance tests additionally cover executing identity isolation, original session reply ownership, concurrent requests to different destinations, serialized busy/repository queues, ancestry/cycle/hop checks, permission/impersonation failures, size/queue limits, process failures, timeouts, cancellation, manual retry, persistent paginated event replay, and an actually killed helper daemon process followed by safe recovery.

## Native integration outcomes

| Surface | Observed outcome | Verification boundary |
| --- | --- | --- |
| OMP 18.12.1 | Opt-in `TestRealSmoke` passed; real marker prompt and exact native-ID reopen | No real destructive tools or automatic MCP delegation |
| Codex 0.163.0 | Opt-in `TestRealSmoke` passed; real marker prompt and exact thread reopen | No native automatic MCP wiring or destructive-tool execution |
| Live OMP ↔ Codex | Both directions completed through the built CLI/API/daemon | Real native credentials and provider replies, not mock transports |
| Live Codex multi-turn/resume | Corrected adapter completed repeated turns and a queued task after daemon restart | Saved native thread retained; stopped origin could read its reply |
| OpenCode 1.19.3 server | Real owned localhost server: health/version, authenticated session create/resume, permission PATCH, delete; unauthenticated health returned 401 | **No provider-backed prompt**; native credentials/API-key environment absent |
| Claude SDK 0.3.303 / CLI 2.1.303 | Dependency installed/version inspected, sidecar syntax checked, fake-process contracts passed | **No provider-backed SDK query**; native credentials/API-key environment absent |
| Meshra MCP | Real CLI stdio proxy negotiated 2025-11-25, listed all seven tools and read a completed owned task | Meshra transport verified; real Claude/OpenCode model-initiated MCP remains unverified |

Authenticated stdio commands actually executed:

```sh
MESHRA_REAL_OMP=1 go test ./internal/adapters/omp -run '^TestRealSmoke$' -count=1 -v
MESHRA_REAL_CODEX=1 go test ./internal/adapters/codex -run '^TestRealSmoke$' -count=1 -v
```

Both passed. No Anthropic/OpenAI API-key environment variables, Claude credential file or OpenCode credential file were present during inspection; Codex reported native ChatGPT login. Native OMP authentication supported its successful real turns. No attempt was made to copy native credentials between harness products.

## Live named-instance workflow

An isolated daemon registered `omp:liveone`, `omp:livetwo`, `codex:livereviewer`, `claude:exampleone`, `claude:exampletwo`, and `opencode:livetest`. Both OMP identities were started simultaneously and had different native IDs. Codex owned a third native ID.

| Task ID | Route / scenario | Persisted result |
| --- | --- | --- |
| `2ea21c4543d40e20fecfd158f8480c91` | `omp:liveone → codex:livereviewer` | `MESHRA_OMP_TO_CODEX_OK` |
| `56952548a240ac2327e332a66b079c9e` | `codex:livereviewer → omp:liveone` | `MESHRA_CODEX_TO_OMP_OK` |
| `a29af6691aefe1a50310a279bb54ad97` | Fresh queued Codex request; daemon stopped/restarted before dispatch | `MESHRA_DURABLE_QUEUE_OK`; exactly one recorded dispatch attempt |

The live records retained their original `originAgentId`/`originSessionId` and matching destination session. A queued message to Claude prod left wallet registered with no native session; the second OMP stayed idle and received no routed turn. The API end-to-end tests separately execute a Claude prod fake transport and assert wallet receives zero turns.

The CLI's followed SSE stream replayed queued/dispatching/running/progress/delta/completed events through the terminal event. The real agent-only MCP stdio proxy returned seven named tools and the completed task via authenticated status lookup, with exit code zero and no diagnostics.

After restart, a fresh queued task remained `queued` with zero attempts. Starting Codex resumed the same saved thread and completed it with one attempt. The originating OMP remained stopped; its previously issued session-bound token still retrieved the result. Native harness children were zero after explicit stops, and all demonstration daemon processes exited cleanly with code zero.

## Failures found and fixed during verification

The first live Codex queued-restart attempt failed with `Codex turn ownership mismatch`. Its earlier-turn notifications could arrive before the new `turn/start` response, and the adapter had incorrectly adopted an event's turn ID. A failing-before/passing-after regression reproduces both the ownership failure and stale-text leakage. The adapter now buffers bounded early relevant events, takes identity only from the command response, and filters stale/unrelated turn events before emitting output.

That failed task was **not** replayed automatically. An explicit administrative retry completed in attempt two after the fix; another turn on the same runtime also completed. The separate fresh queued-restart task above then passed with exactly one attempt. This is admission deduplication and explicit retry semantics, not exactly-once external execution.

An additional regression caught unintended shrinking of an inherited nondefault delegation hop limit. Unspecified child limits now inherit the parent; only an explicit tighter limit narrows it. Both regressions pass in the final race suite. Interrupted runtimes also pause further inbox dispatch until explicit restart, preventing queued tasks from cascading into an already-closed stdio process.

## Idle resource measurement

Measured from `/proc` over a 5.002-second interval after the event-driven scheduler change. The daemon had durable offline queued work and no running harnesses; this is not a provider-turn footprint.

| Metric | Observed value |
| --- | ---: |
| Resident memory | 13,496 KiB (13.18 MiB) |
| Sampled CPU, one-core basis | 0.000% |
| Daemon processes | 1 |
| Harness child processes | 0 |

Zero sampled CPU means no process CPU ticks were recorded during that interval, not a claim of zero CPU under all workloads. Harnesses consume separate memory/CPU and provider cost when started. The initial periodic queue scan was replaced with coalesced wakeups and next-deadline scheduling, avoiding repeated scans of offline inboxes.

## Remaining verification limits

- No authenticated Claude/OpenCode provider turns or model-initiated native MCP delegation were possible with the inspected credentials. Reproducible opt-in commands are in [adapters.md](adapters.md) and its detailed adapter references.
- No claim of real destructive-tool cancellation/approval safety beyond the conservative policies, protocol contracts and inspected native interfaces.
- No macOS or native Windows execution was performed. Unix process groups/flock target Linux/macOS; native Windows is not implemented.
- Repository locks coordinate only Meshra and documented roots, not unrelated editors/processes or all shared linked-worktree metadata.
