# Direct API prompt caching plan

> **Reconciliation (2026-11-05):** superseded in scope by `docs/MESHRA_PERSONAL_AGENT_ROADMAP.md`. Implemented features below are still present, but historical statements about profile automation being "broker-only" for Claude/OMP are not current: trusted Claude/OMP profiles run native tools with full host access, and restricted Claude/OMP execution is unavailable until proven. Verification claims here are historical and were not re-run.

## Implementation status

- **Phase 0:** deterministic encoding is covered for both formats. Live
  OpenAI and Anthropic cache measurements remain blocked by missing provider
  credentials.
- **Phase A:** implemented: Anthropic breakpoints, cache modes, usage
  accounting and the remembered rejection fallback.
- **Phase B:** implemented bounded alternating history, block eviction, stable
  profile instructions and threshold-based bulk compaction. Prefix stability
  is not guaranteed for every turn: attachment/context excerpts and tool-loop
  messages are not replayed, and sensitive output may be redacted. These cases
  can cause a cache miss on the next turn.
- **Phase C:** implemented the optional, default-off OpenAI cache key.
- **Phase D:** implemented cache rates, read/write statistics, the zero-read
  diagnostic and compaction markers in the Requests log.

The implemented defaults are `auto`, with `1h` opt-in, and alternating history
for Direct API chats. These are implementation choices, not live-provider
verification.

## Goal

Long chats and tool loops on the Direct API harness should reuse the provider's
prompt cache, as Claude Code and OMP do, and Meshra should show the resulting
cache rate. Claude and OMP cache inside their own CLIs; Meshra only reads their
numbers from logs, so this plan covers the Direct API path only.

Prompt caching saves input processing, not answers. The provider reuses its
processed state for a byte-identical prefix of the request. Output is unchanged;
only cost and time to first token improve.

## Current state (read from code, not measured on a live endpoint)

1. **Anthropic endpoints never request caching.** `encodeAnthropicRequest` in
   `internal/adapters/openai/anthropic.go` sends no `cache_control`, so every
   step of a tool loop is billed at the full input price.
2. **Cache writes are dropped.** `cache_creation_input_tokens` is added into the
   prompt total and never recorded on its own, so the stats Cache write column is
   always 0 for Direct API. Cache reads are recorded.
3. **Chat history is one JSON blob in a single user message.** `chatPrompt`
   (`internal/relay/chat.go`) wraps up to 12 exchanges / 32 KiB as
   `{"recentConversation":[...],"currentUserMessage":...}`. Appending a turn keeps
   the earlier bytes stable. Once the window is full the oldest exchange drops off
   the front, the whole prefix shifts and every later turn misses the cache.
4. **Compaction rewrites earlier messages.** `compact()` in
   `internal/adapters/openai/compact.go` replaces old tool results with stubs
   mid-turn, which invalidates the cache from the first changed message onward.
5. **Unverified:** whether the Direct API path uses `stableFirst` in
   `profilePrompt` (`internal/relay/profiles_send.go`), and whether the tool list
   and JSON key order are identical on every request.
6. **UI already partly exists.** `web/src/Stats.tsx` computes a cache rate from
   cache read and write; `web/src/AIStatus.tsx` shows cached input.

## Non-goals

- No response cache: a repeated prompt still gets a fresh answer.
- No change to the Claude or OMP harnesses.
- No Meshra-side pricing; savings are shown as tokens, not money.
- No Anthropic streaming work (separate topic).

## Phase 0: baseline (read-only, about 1 hour)

- Unit test: encoding the same request twice yields byte-identical output
  (tool order, key order, no timestamps).
- Run a few long turns on one OpenAI-format and one Anthropic-format endpoint.
  Record the `cached_tokens` / `cache_read_input_tokens` the provider returns.
  This shows what already caches for free.
- Confirm item 5 above and note the result in this file.

Baseline inspection: Direct API calls `profilePrompt` with `stableFirst=true`;
profile instructions, memories and skills precede its reconstructed history.
Live provider cache measurements were not made: neither `OPENAI_API_KEY` nor
`ANTHROPIC_API_KEY` was available in the environment. Provider cache hits and
gateway compatibility therefore remain unverified against live endpoints.

## Phase A: Anthropic `cache_control` (largest saving)

- Request shape: `System` becomes a block list; add
  `cache_control: {"type":"ephemeral"}` to the last tool, the system block and the
  last block of the final message. The last marker moves forward every step.
  Provider order is tools, system, messages. Stay within the limit of 4
  breakpoints.
- Skip markers on prompts below the model's minimum cacheable size.
- Accounting: record `cache_creation_input_tokens` as cache write through
  `model_request` events, `recordModelRequest` and `run_usage`.
- Per-endpoint setting `promptCaching`: `auto` (default), `off`, `1h`. The 1-hour
  tier costs more to write; make it opt-in.
- Compatibility: some gateways (for example OpenCode Go) may reject the field. On
  a definite HTTP 400 that names `cache_control`, retry once without it and
  remember the result for that endpoint. This is a definite rejection, so the
  retry is safe.
- Tests: request shape, usage accounting (read, write, uncached), fallback.

## Phase B: keep the prefix stable

- **Compaction:** trigger only when the prompt passes about 75% of
  `ContextWindowTokens`, and rewrite the oldest results in one bulk step, never a
  little on every step. Expect one cold request afterwards and mark it in the log.
- **History:** send real alternating user and assistant messages
  (`Turn.History`) instead of the JSON blob. When the window is full, drop history
  in blocks of several exchanges so the prefix changes rarely. Other adapters
  ignore the new field.
- **Profile prompt:** keep instructions, skills and memories first and anything
  volatile after them.
- Test: the prefix bytes are identical between turn N and turn N+1 until a
  block drop or a memory change.

## Phase C: OpenAI-format endpoints

- Caching there is automatic, so Phase B does most of the work.
- Optional per-endpoint `prompt_cache_key` derived from profile and conversation,
  off by default because some compatible servers reject unknown fields.

## Phase D: show the cache rate

- Cache rate and cache read tokens in the chat footer and AI Status for Direct
  API turns.
- Stats: cache rate per model and provider (reuse the existing calculation);
  include cache write now that it is recorded.
- Diagnostic hint when a long turn shows 0% cache read: the endpoint may not
  cache, or the prefix is changing.
- Mark compaction events in the Requests log so a drop in cache rate is
  explainable.

## Cross-cutting rules

- Caching must never change answers. Behavior with caching off is the current
  behavior.
- No secret in a cached block. The per-turn token stays out of the stable prefix;
  add a test.
- Entries live about 5 minutes (Anthropic default, refreshed on use). A longer
  pause costs one cold request; this is expected.
- Update `docs/adapters.md` and `CHANGELOG.md`.

## Order and sizing

| Step | Size | Notes |
|---|---|---|
| Phase 0 | 1 h | decides whether C is needed |
| Phase A | about 0.5 day | biggest cost saving |
| Phase B | about 1 day | history shape is the main change |
| Phase D | about 0.5 day | mostly UI wiring |
| Phase C | about 2 h | only if Phase 0 shows gaps |

## Open questions for the owner

1. Default for `promptCaching`: `auto` for all Anthropic-format endpoints, or off
   until enabled per endpoint?
2. Is the 1-hour tier worth offering, given its higher write cost?
3. Should history become real messages for all Direct API chats, or only profile
   chats first?
