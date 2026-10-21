# Conversation list

The list is an Operate surface: start a conversation, find an existing one, and
switch without losing chat state. Its warm clay palette is scoped to the inbox;
mint remains the primary action color and blue remains the app-navigation accent.

The list now provides:

- A full-width New conversation action above search.
- All / AI chats / Agents filters, with explicit pressed states.
- Pinned conversations first, then Today, Yesterday, and Earlier groups.
- Harness marks for AI chats and participant icons for agent conversations.
- Two-line titles, message/state previews, and localized update times.
- Separate SVG pin buttons, persisted locally, that do not overlap long titles.
- Clear-search, retry, no-results, and empty-filter recovery actions.
- A selected conversation marked by both its warm surface and `aria-current`.

Creating a conversation resets the list's search and filter so the new item can
be found immediately. Search is debounced by 200 ms and inactive result caches
are released. The existing API limit of 100 results remains; title search can
find older conversations. Local pin preferences and the user's saved sidebar
width remain supported. New installations default to a 320px sidebar.

The component lives in `web/src/ConversationList.tsx`, with scoped styles in
`web/src/conversation-list.css`. Header, row, and group spacing use a 4px rhythm;
mobile pin controls and navigation preserve independent touch targets.

Verification covers pin/unpin persistence, mode filters, selected links, long
titles, no-result search recovery, dark/light themes, and mobile overflow, as well
as the existing chat and keyboard flows. Impeccable's detector found no issues.

Default spacing is compact: 10px row insets, 2px preview separation, 4px
metadata separation, and 16px between groups. The header uses 16px insets
and mobile pin targets remain 40×44px. Chat transcripts use 16px response
separation and 24px between exchanges, with a two-line composer to leave
more room for the conversation. Font sizes and history bounds are unchanged.
