import { MessageSquare, Pin, Plus, Search, Users, X } from "lucide-react";
import { Button } from "./components/ui/button";
import { HarnessLogo, harnessName } from "./HarnessLogo";
import { localRead } from "./api";
import type { InboxItem } from "./types";
export type ConversationFilter = "all" | "ai" | "agents";

type Props = {
  items: InboxItem[];
  currentId: string;
  search: string;
  onSearch: (value: string) => void;
  filter: ConversationFilter;
  onFilter: (value: ConversationFilter) => void;
  pins: string[];
  onPin: (id: string) => void;
  onNew: () => void;
  loading: boolean;
  error: boolean;
  onRetry: () => void;
};
function dayGroup(value: string) {
  const date = new Date(value),
    today = new Date();
  today.setHours(0, 0, 0, 0);
  const yesterday = new Date(today);
  yesterday.setDate(today.getDate() - 1);
  if (date >= today) return "Today";
  if (date >= yesterday) return "Yesterday";
  return "Earlier";
}
function updatedLabel(value: string) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "";
  return dayGroup(value) === "Today"
    ? date.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" })
    : date.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}
export function ConversationList({
  items,
  currentId,
  search,
  onSearch,
  filter,
  onFilter,
  pins,
  onPin,
  onNew,
  loading,
  error,
  onRetry,
}: Props) {
  const filtered = items.filter(
    ({ conversation: c }) =>
      filter === "all" ||
      (filter === "ai" ? c.mode === "chat" : c.mode !== "chat"),
  );
  const pinned = filtered.filter(({ conversation: c }) =>
    pins.includes(c.conversationId),
  );
  const recent = filtered.filter(
    ({ conversation: c }) => !pins.includes(c.conversationId),
  );
  const groups = [
    { label: "Pinned", items: pinned },
    ...(["Today", "Yesterday", "Earlier"] as const).map((label) => ({
      label,
      items: recent.filter((item) => dayGroup(item.updatedAt) === label),
    })),
  ].filter((group) => group.items.length);
  return (
    <>
      <div className="conversation-list-header">
        <div className="conversation-list-heading">
          <h1>Conversations</h1>
          <span
            className="conversation-count"
            aria-label={`${filtered.length} conversations`}
          >
            {filtered.length}
          </span>
        </div>
        <Button className="new-conversation-button" onClick={onNew}>
          <Plus size={17} />
          New conversation
        </Button>
        <div className="conversation-search">
          <Search size={16} />
          <input
            aria-label="Search conversations"
            placeholder="Find a conversation…"
            value={search}
            onChange={(e) => onSearch(e.target.value)}
          />
          {search && (
            <button
              type="button"
              aria-label="Clear conversation search"
              onClick={() => onSearch("")}
            >
              <X size={15} />
            </button>
          )}
        </div>
        <div
          className="conversation-filters"
          role="group"
          aria-label="Filter conversations"
        >
          {(
            [
              ["all", "All"],
              ["ai", "AI chats"],
              ["agents", "Agents"],
            ] as const
          ).map(([value, label]) => (
            <button
              key={value}
              type="button"
              aria-pressed={filter === value}
              onClick={() => onFilter(value)}
            >
              {label}
            </button>
          ))}
        </div>
      </div>
      <div
        className="conversation-list"
        aria-label="Conversations"
        aria-busy={loading}
      >
        {loading ? (
          <p className="conversation-list-status" role="status">
            Loading conversations…
          </p>
        ) : error ? (
          <div className="conversation-list-empty" role="alert">
            <strong>Conversations couldn’t load</strong>
            <p>
              Your saved chats are still available once the connection returns.
            </p>
            <Button variant="outline" onClick={onRetry}>
              Try again
            </Button>
          </div>
        ) : !filtered.length ? (
          <div className="conversation-list-empty">
            <MessageSquare size={24} />
            <strong>
              {search
                ? "No matching conversations"
                : filter !== "all"
                  ? "Nothing in this view yet"
                  : "Start your first conversation"}
            </strong>
            <p>
              {search
                ? "Try another title or clear your search."
                : filter !== "all"
                  ? "Switch to All to see your other conversations."
                  : "Choose an AI or bring your agents together."}
            </p>
            {search ? (
              <Button variant="outline" size="sm" onClick={() => onSearch("")}>
                Clear search
              </Button>
            ) : filter !== "all" ? (
              <Button
                variant="outline"
                size="sm"
                onClick={() => onFilter("all")}
              >
                Show all conversations
              </Button>
            ) : null}
          </div>
        ) : (
          <>
            {search && (
              <p className="conversation-search-summary">
                {filtered.length} result{filtered.length === 1 ? "" : "s"} for “
                {search}”
              </p>
            )}
            {groups.map((group) => (
              <section
                className="conversation-group"
                key={group.label}
                aria-label={group.label + " conversations"}
              >
                <h2>
                  {group.label === "Pinned" && <Pin size={12} />}
                  <span>{group.label}</span>
                  <span className="conversation-group-count">
                    {group.items.length}
                  </span>
                </h2>
                <ul>
                  {group.items.map((item) => {
                    const c = item.conversation;
                    const selected = currentId === c.conversationId;
                    const isPinned = pins.includes(c.conversationId);
                    const unread =
                      !selected &&
                      !!item.preview &&
                      new Date(item.updatedAt).getTime() >
                        localRead<number>("last-viewed:" + c.conversationId, 0);
                    const pending = [
                      "queued",
                      "dispatching",
                      "running",
                    ].includes(item.state);
                    const failed = ["failed", "timed_out"].includes(item.state);
                    return (
                      <li
                        key={c.conversationId}
                        className={
                          "conversation-item " +
                          (selected ? "selected " : "") +
                          (unread ? "unread" : "")
                        }
                      >
                        <a
                          href={"#chat/" + c.conversationId}
                          aria-current={selected ? "page" : undefined}
                        >
                          <span
                            className="conversation-avatar"
                            aria-hidden="true"
                          >
                            {c.mode === "chat" ? (
                              <HarnessLogo id={c.participants[0] || "AI"} />
                            ) : (
                              <Users size={18} />
                            )}
                          </span>
                          <div className="conversation-item-content">
                            <div className="conversation-item-title">
                              <strong title={c.title}>{c.title}</strong>
                              {unread && (
                                <span
                                  className="conversation-unread"
                                  role="img"
                                  aria-label="Unread activity"
                                />
                              )}
                            </div>
                            <p>
                              {pending
                                ? "Waiting for a reply…"
                                : failed
                                  ? "Reply needs attention"
                                  : item.preview ||
                                    "Ready for your first message"}
                            </p>
                            <div className="conversation-item-meta">
                              <span>
                                {c.mode === "chat"
                                  ? harnessName(c.participants[0] || "AI")
                                  : `${c.participants.length} agent${c.participants.length === 1 ? "" : "s"}`}
                              </span>
                              <time dateTime={item.updatedAt}>
                                {updatedLabel(item.updatedAt)}
                              </time>
                            </div>
                          </div>
                        </a>
                        <button
                          className={
                            "conversation-pin " + (isPinned ? "is-pinned" : "")
                          }
                          type="button"
                          aria-label={(isPinned ? "Unpin " : "Pin ") + c.title}
                          aria-pressed={isPinned}
                          title={
                            isPinned ? "Unpin conversation" : "Pin conversation"
                          }
                          onClick={() => onPin(c.conversationId)}
                        >
                          <Pin size={15} />
                        </button>
                      </li>
                    );
                  })}
                </ul>
              </section>
            ))}
          </>
        )}
      </div>
    </>
  );
}
