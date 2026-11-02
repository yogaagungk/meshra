# Claude Agent SDK adapter

## Compatibility evidence

Implementation references were read before choosing protocol methods:

- [Claude Agent SDK TypeScript reference](https://code.claude.com/docs/en/agent-sdk/typescript): `query({prompt, options})`, `resume`, `SessionStart` / system `init` native session IDs, `canUseTool`, `PreToolUse`, `AbortController`, `includePartialMessages`, and HTTP `mcpServers` with `headers`.

Inspected environment: Node **24.22.0**, npm **11.19.1**, Linux x64/WSL2. Local dependency installation completed; native `--version` inspection reported:

| Component | Exact version | Evidence |
| --- | --- | --- |
| `@anthropic-ai/claude-agent-sdk` | **0.3.303** | Installed package manifest and npm lock |
| Bundled Claude Code | **2.1.303** | Bundled executable `--version` |

The capability flag `authenticatedSmokeVerified` remains false. Deterministic contracts passed in the integrated race suite. See [verification.md](verification.md) for actual outcomes. Installation/version inspection alone is not provider authentication or authenticated harness MCP proof.

Fresh v0.2 native version/help, SDK declaration, auth-status and credential-free HTTP observations are recorded in [V0.2_ADAPTER_VERIFICATION.md](V0.2_ADAPTER_VERIFICATION.md). That pass ran no provider turns or Go checks.

## Installation and discovery

From the repository root:

```sh
npm ci --prefix bridges/claude-agent-sdk --ignore-scripts
```

Both manifests pin exact direct dependency versions and include npm locks. Claude's SDK bundles its native binary as a platform optional dependency; do not omit optional dependencies. The inspected npm install already provided a working local executable; no global installation or user harness configuration was changed.

Checkout-relative and source-relative discovery also work in development. To relocate these artifacts, set:

- `MESHRA_CLAUDE_BRIDGE=/absolute/path/to/bridge.mjs`

These select executable artifacts, not provider credentials or persisted agent configuration. Claude requires `node` on the daemon PATH. Missing dependencies fail with installation instructions. No adapter starts a paid prompt in its Factory.

Configure provider credentials using each harness's supported native authentication or environment mechanism **before starting the daemon**. Claude accepts the native default model or the agent's `model`; a nonempty `provider` must be `anthropic` (alternate SDK transports remain a native environment concern). Meshra never saves provider API keys or bearer tokens in agent configuration.

## Session and process ownership

### Claude

`internal/adapters/claude.New()` runs the actual Claude Agent SDK in `bridges/claude-agent-sdk/bridge.mjs`, not the generic Anthropic Messages API. A fresh runtime has no fabricated native ID. The first paid `Run` uses SDK session allocation. The sidecar emits the native session ID from `SessionStart` immediately, with system `init` / result as fallbacks. The Go adapter emits `session` synchronously so the relay can durably update its mapping and the task-bound token before MCP calls. Later turns pass only that saved ID as `options.resume`. There is no `continue`, latest-session lookup, interactive-terminal attach, fork, or random UUID passed as a resumed session. A changed native ID on resume fails rather than remapping the identity.

Each turn has one owned bridge process and one SDK query. Text `stream_event` deltas become `delta` events; only `result` supplies the final return value. SDK permission denials become `approval_required`, never raw frame dumps. Errors are bounded/redacted in the sidecar, bearer tokens are redacted again at the Go boundary, and raw SDK stderr is discarded. Cancellation sends a process-group termination signal, allowing the sidecar's AbortController to run, with forced process/pipe cleanup bounded by Go's wait delay and a final process-group kill. Close cancels an active turn and waits for it to be reaped.

## Permissions and MCP

There is no interactive approval UI. Both adapters deny shell execution, file mutation, subagent execution, and unknown tools by default. A denial produces an observable `approval_required` event; it is **not** a pending request that a human can later approve in this MVP. Do not interpret this event as successful execution.

Claude explicitly uses `permissionMode: default`, never bypass mode, and disables user/project/local SDK settings (`settingSources: []`) and undeclared MCP servers (`strictMcpConfig: true`). A `PreToolUse` hook denies non-allowlisted tools before automatic permission rules can approve them; `canUseTool` is a second fail-closed boundary. The allowlist contains `Read`, `Glob`, `Grep`, the seven v0.1 agent MCP operations, and v0.2 `meshra_task_trace`. Administrator registration/retry/metrics/doctor tools are not automatically allowed to a harness. This does not provide autonomous code editing or a shell. Managed native policy/authentication still apply; this is not an OS sandbox.

Before a prompt it opens `/event`; matching-session `permission.asked` and `permission.v2.asked` requests produce `approval_required` and receive `reply: reject`. Loss or malformed data on the permission supervision stream aborts the turn rather than leaving potentially unobserved work running. Mutating tools are not approved automatically. Treat the configured checkout and the native harness configuration as trusted executable inputs.

`worktreeWrites: false` therefore means conservative tool policy, not a universal OS-level no-write guarantee. No write enablement or approval bypass is provided. See the [four-adapter enforcement matrix](V0.2_ADAPTER_VERIFICATION.md#enforcement-matrix-worktreewrites-false) for the precise trust boundary and unverified real-tool scenarios.

The relay supplies a **per-turn identity/session/task-bound** bearer token, not an admin token:

- Claude passes `mcpServers.meshra = {type: "http", url: Turn.MCPURL, headers: {Authorization: "Bearer " + Turn.Token}}` in memory. SDK strict MCP mode excludes other configured servers.
There is a new server for every turn, so a previous token cannot remain in a reused MCP client. No HTTP config PATCH, MCP-add route, or config file is used for credentials. `oauth: false` prevents bearer-only MCP from starting OAuth registration or writing MCP OAuth credentials. Automatic sharing and updating are disabled in this process's inline config.

Meshra does not write these credentials to disk or place them in prompts/argv. Process environments are visible to the same OS user and privileged processes; the local-user trust boundary still applies. Native harness session transcripts may contain prompts/results and should be protected as sensitive data.

## Contract tests and opt-in paid smoke

Main integration should run (from the root, after all changes land):

```sh
```

These default tests require no provider accounts. Coverage includes no paid Factory turn, initial native session persistence, resume, independent identities, explicit denial, protocol/upstream failures, no silent session replacement, cancellation, native abort, and owned runtime cleanup. Private constructor injection is available to package tests without adding transport configuration to persisted agent records.

The following commands **can incur provider charges** and are intentionally opt-in. Authenticate natively first, keep credentials out of shell history, and run each independently:

```sh
# Claude: native SDK authentication must already be available.
MESHRA_AUTH_SMOKE=1 go test -v ./internal/adapters/claude -run '^TestAuthenticatedSmoke$' -count=1
```

Each smoke allocates a real native session and performs a second turn through resume. A pass establishes those paid basic prompt/resume paths only; it is not proof of cross-agent routing, MCP, all models, cancellation during a real tool call, or permission isolation under every native customization. No such pass is claimed here.

For authenticated end-to-end MCP/identity validation, start `meshra serve` from the daemon environment with native authentication available. Send a uniquely marked prompt to one Claude identity and verify only its task/session advances. Ask that identity to use `meshra_request` to queue a message to the other, record the returned task ID, and poll `meshra_task_status` / `meshra_task_events` rather than waiting synchronously. Inspect task events and native session IDs, restart the daemon, then send another prompt to each and confirm its saved native mapping is resumed. Finally request a shell/file mutation and verify `approval_required` appears without the requested side effect. Those manual procedures remain **unverified** until actually run in an authenticated environment.


Native streaming capability and relay persistence timing differ: v0.2 buffers bounded delta text until successful completion so credentials split across native chunks can be redacted together. Progress/session/approval events remain immediate.
