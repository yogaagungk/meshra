# Console typography and accents

This pass refines the existing Meshra console in Operate mode: readable chat,
consistent controls, and clear navigation. It preserves Manrope, the mint primary
action color, existing harness marks, workflows, themes, and bounded chat history.

## Typography

The locally bundled Manrope variable font is the UI family. Monospace is reserved
for code and paths. Shared roles are defined at the top of `web/src/style.css`:

| Role | Token | Default size |
| --- | --- | --- |
| Chat, prose, form input | `--font-body` | 16px / 1rem |
| Controls and row titles | `--font-ui` | 14px / .875rem |
| Labels and code blocks | `--font-label` | 13px / .8125rem |
| Supporting metadata | `--font-meta` | 12px / .75rem |
| Page headings | `--font-title` | 28px / 1.75rem |
| Dialog and section headings | `--font-section` | 20px / 1.25rem |

Body, label, and heading weights use 450, 550, and 650. Body tracking is neutral;
headings use modest negative tracking. Chat text uses 1.7 line height and a bounded
reading column. Browser zoom remains enabled. Timestamps use tabular numerals.
Decorative eyebrow labels above headings were removed.

## Color

Mint remains the action color. Blue provides a distinct wayfinding and reference
role: active navigation, selected conversations, links, shared context, focus, and
thinking indicators. Status colors retain their previous meaning and include text
or icons. Blue values are explicit in each theme:

| Token | Dark | Light |
| --- | --- | --- |
| `--secondary` | `#b6c6ff` | `#4058a3` |
| `--secondary-strong` | `#d1dcff` | `#293e80` |
| `--secondary-tint` | `#20283d` | `#edf1ff` |
| `--secondary-border` | `#465a83` | `#a3b3de` |

Text selection, carets, scrollbars, and focus indicators use the same palette.
Thinking retains its CSS dot animation with a readable solid label. Reduced-motion
support remains. No extra animation timers, font services, or image dependencies
were introduced.

## Flow corrections

Dialog action rows stay visible when the form scrolls. Existing folder selection,
keyboard controls, mobile navigation, and conversation controls remain available.
Live timeline refresh no longer cancels a pending older-message page request.

## Validation

Impeccable's mechanical detector reported no findings. The first visual and browser
pass inspected desktop and mobile, both themes, forms, chat, agent cards, context,
and preferences. The final confirmation passed all nine browser flows, including corrected paging
and Axe accessibility checks. Desktop/mobile captures and both themes were
inspected. The production TypeScript/build and all four frontend unit tests passed.

The live restart also exposed a managed Codex startup edge case: an idle readiness
thread can have no native rollout to resume. Managed chat slots now start fresh
with disposable native state after restart. SQLite chat history continues to supply
conversation context; ordinary named agents retain their durable-session behavior.
A relay regression covers explicit restart, daemon recovery, and ordinary-session
preservation.
