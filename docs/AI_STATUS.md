# AI Status

Settings → AI status shows provider-reported account quota, reset countdowns,
the separate Meshra daily turn budget, and daily recorded usage. One-to-one
AI chats have a compact status button above the composer; it opens details for
that conversation, including its most recent native context snapshot and usage
history. All screens preserve the console's full-width, compact layout.

## Reported data and scope

- Codex reads `account/rateLimits/read` through an isolated ephemeral app-server
  runtime. Each reported primary/secondary bucket retains its usage percentage,
  window length and reset timestamp. It never generates an answer, redeems reset
  credits, logs out, or changes native authentication.
- OMP runs `omp usage --json --redact --no-extensions`. It normalizes supported
  account reports from the installed CLI, including percentage and reset time.
  Custom extension providers are not loaded by this status probe. Accounts are
  numbered locally; emails, account IDs, raw provider metadata and credit/reset
  identifiers are never returned to the console.
- Claude and OpenCode quota collection is explicitly not connected. Their local
  token usage remains available where reported; missing quota is not zero quota.
- Account quota can include work in other apps. Different windows can cover
  different models, accounts, or shared pools. The compact indicator shows the
  highest reported window percentage; it does not claim every model maps to that
  window, or identify which OMP account will serve the next request.

Quota snapshots remain in a bounded in-memory cache: up to four known harness
keys and 64 normalized windows per snapshot. Normal refresh is once per minute
while the relevant UI is active; manual native checks have a 15-second cooldown.
Model discovery and quota reads share a single nonblocking native-helper gate,
with a 25-second deadline. OMP stdout is capped at 2 MiB and stderr is discarded;
the CLI process group is killed on cancellation. Helpers are closed after use.
No background account polling runs when the UI is closed. Existing native auth
broker behavior and native harness memory use remain owned by those tools.

The UI shows the source, last successful refresh, and provider observation time
when available. An upstream failure retains the last successful windows, marks
them stale, and reports the failed refresh without exposing raw upstream errors.
Reports with old native observation times are also marked stale. Passing a reset
time never invents a new remaining allowance: the UI requests a fresh report.

## Context and usage

Schema 8 extends the bounded 30-day attempt ledger with conversation ownership,
cached input, native context tokens/capacity, source, estimation flag and snapshot
time. Existing attempt ownership is backfilled from retained tasks; it cannot
recover relationships for tasks deleted before this update. New per-conversation
usage survives ordinary task-history pruning during that 30-day ledger window.

Codex records the active turn's `thread/tokenUsage/updated.last` token counts and
`modelContextWindow`; notifications from another thread/turn are excluded. Missing
capacity or context totals produce no context observation. OMP queries `get_state`
after successful completion, with a two-second bound, and preserves the native
`contextUsage` **estimated** classification. Claude can record the latest assistant
message's usage and matching native `modelUsage` context capacity when both are
available. OpenCode context capacity is not synthesized.

Context is the last native snapshot, not cumulative token expenditure and not a
prediction for a draft or the next request. One-to-one chat still uses a disposable
native runtime with a bounded recent-history prompt on each turn. Tool activity,
history selection and native compaction can change the next snapshot. Old replies
have no retroactively fabricated context readings.

Daily history is filtered by harness and optionally conversation, grouped by UTC
day and selected model, and returns at most 100 groups with an explicit truncation
notice. Token figures include only attempts that reported them; cache values stay
unknown when not reported. OpenCode usage covers its returned assistant message
and may omit intermediate calls. Native-default model rows do not identify the
actual model unless the harness reports it separately. Provider billing, costs,
and monetary budgets are not inferred.

Warnings appear at 80% and identify exhausted reports at 100%. They inform the
operator and never silently pause, reroute, or restart work. Meshra's daily turn
budget is displayed separately and keeps its existing dispatch enforcement.

## Verification

Deterministic tests cover quota parsing, normalized identity-free responses,
refresh cooldown, stale retention, serialized probes, administrator access,
context observation/estimation, and conversation-scoped usage. Browser coverage
includes provider/local/context separation, reset countdown, cache counts, stale
and unsupported states, keyboard access, both themes and mobile overflow.

For a read-only native quota smoke check (no AI response or credit reset):

```sh
MESHRA_NATIVE_AI_STATUS=1 go test ./tests -run '^TestNativeAIStatus$' -v
```
