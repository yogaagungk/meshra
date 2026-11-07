# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Primary: a solo developer on their own machine (Ubuntu/WSL2 or Linux) who runs several independent AI agents (Claude Code, OMP, or any OpenAI-compatible API called directly) against local projects. Their job: chat with those agents, delegate and hand off work between them, share context deliberately, and review what actually executed.

## Product Purpose

Meshra is a local-first developer console and relay for independently named AI coding agent instances. It lets one person talk to agents, share pinned immutable context, and inspect durable task execution from one workspace. Success: the user can start working with a new agent quickly, always knows which agent did what, and can trust that nothing ran, was shared or left the machine without their say.

## Personal business automation and trust domains

Beyond coding, Meshra is the local control plane for persistent personal agents: the owner configures an agent's identity, instructions, approved knowledge and runtime, then delegates coding or business workflows (email monitoring, ads reporting, social drafting, customer conversations) under explicit authority. Business integrations are planned in `docs/MESHRA_PERSONAL_AGENT_ROADMAP.md`; none is implemented yet.

Two execution modes keep authority honest:

- **Trusted** (operator-selected, acknowledged): the harness's native tools run with the operator's full host access. It is never presented as sandboxed and never serves external-message or unattended work.
- **Restricted** (default for new profiles): no ambient host authority; only the admitted Meshra broker catalog. Only restricted profiles may receive connector, routine, webhook or delegated-external work. Restricted execution is available today for Direct API profiles. For Claude and OMP it stays unavailable until confinement of the native process is proven (no host fallback).

"Local-first" means local ownership of orchestration and state. Selected content still leaves the machine when a cloud model or external service is used; the console must disclose that.

## Positioning

Friendly like an agent-team app, but with provable boundaries: exact canonical agent identities (`claude:exampleone` is a durable identity, not an alias for a model), immutable shared context, persisted and auditable task history, explicit recipients with no default broadcast, and localhost-only operation with no cloud or paid account. A hosted or subscription-bundled agent app cannot truthfully claim this.

## Operating Context

- Single administrator on loopback (default 127.0.0.1:7331, `/console/`); sign-in with local admin token or username and password; no multi-user authorization.
- Embedded React/TypeScript console served by the Go daemon; SQLite WAL storage.
- Core surfaces: Chat (one-to-one AI chat and multi-recipient agent collaboration), Agents, Workspaces (registered local directories), Contexts, Automation, Activity, Settings.
- Messaging connectors (Slack, Discord, Telegram), provider connections, MCP bridges, browser panel and usage dashboard already exist.

## Capabilities and Constraints

- Agent identities are canonical `<harness>:<name>`; routing uses exact IDs and replies persist against the original identity and session.
- Chat turns use bounded recent-history windows; context is shared by immutable reference.
- Not present today (open product decisions, not commitments): automatic provider failover, user-facing queue with pause/steer, agent creation from a prompt, local-model (Ollama/LM Studio) presets, plugin catalog, installable mobile or desktop apps.

## Brand Commitments

Name: Meshra. The current console uses a "meshra." wordmark, Manrope and a mint action color; these are incumbent, not confirmed as binding.

## Evidence on Hand

Existing screenshots and test evidence in `docs/evidence/v0.4/` (may predate recent UI changes). Typography and color tokens documented in `docs/UI_POLISH.md`. No user testimonials, usage metrics or customer evidence exist; do not fabricate any.

## Product Principles

1. Exact identity stays discoverable: friendly names may lead, but the canonical `harness:name` is always one glance or hover away.
2. Local-first and private by default: no cloud, accounts or hidden sharing; sharing is explicit and visible.
3. Nothing happens implicitly: explicit recipients, explicit context attachment, human-approved retries.
4. Approachable first run: a new solo developer should reach a first useful conversation without learning the relay's vocabulary.
5. Every outcome is inspectable: what ran, who ran it, and what it produced is traceable from the chat.

## Accessibility & Inclusion

WCAG AA, keyboard-first operation (command palette, shortcuts), visible focus, reduced-motion support, and Axe checks in the browser test suite remain required.
