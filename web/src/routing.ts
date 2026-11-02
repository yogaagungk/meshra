import type { Agent, Task } from "./types";

/**
 * Deterministic recipient routing for collaboration chats. It never calls a
 * model and never broadcasts implicitly: every route is one of the explicit
 * cases below, and the composer shows the reason so the user can correct it.
 */
export type RouteReason =
  | "mention"
  | "everyone"
  | "selected"
  | "only-member"
  | "follow-up"
  | "none";

export type Route = { ids: string[]; reason: RouteReason; label: string };

export const EVERYONE = "everyone";

type Member = Pick<Agent, "id" | "name">;

/** The short handle of an agent: `claude:planner` is addressed as `@planner`. */
export function handle(member: Member): string {
  return member.name || member.id.split(":").slice(1).join(":") || member.id;
}

const token = /^@([A-Za-z0-9_.:-]+)/;

/**
 * Reads the agents addressed by the leading `@mentions` of a draft. Mentions
 * match the exact canonical ID or the short handle, case-insensitively, and
 * the longest match wins. Unknown mentions are ignored, never guessed.
 */
export function leadingMentions(
  draft: string,
  members: Member[],
): { ids: string[]; everyone: boolean } {
  const ids: string[] = [];
  let everyone = false;
  let rest = draft.trimStart();
  for (;;) {
    const match = token.exec(rest);
    if (!match) break;
    const raw = match[1].toLowerCase();
    if (raw === EVERYONE) everyone = true;
    else {
      const found = members
        .filter((m) => m.id.toLowerCase() === raw || handle(m).toLowerCase() === raw)
        .sort((a, b) => b.id.length - a.id.length)[0];
      if (found && !ids.includes(found.id)) ids.push(found.id);
    }
    rest = rest.slice(match[0].length).trimStart();
  }
  return { ids, everyone };
}

/** The agents the user last wrote to, so a follow-up needs no re-selection. */
export function lastAddressed(tasks: Task[], members: Member[]): string[] {
  const known = new Set(members.map((m) => m.id));
  const human = tasks.filter((t) => known.has(t.to) && !known.has(t.from));
  const last = human[human.length - 1];
  if (!last) return [];
  return human.filter((t) => t.messageId === last.messageId).map((t) => t.to);
}

export function resolveRoute(input: {
  draft: string;
  members: Member[];
  selected: string[];
  tasks: Task[];
}): Route {
  const { draft, members, selected, tasks } = input;
  const mentions = leadingMentions(draft, members);
  if (mentions.everyone && members.length) {
    return {
      ids: members.map((m) => m.id),
      reason: "everyone",
      label: "Everyone (" + members.length + ")",
    };
  }
  if (mentions.ids.length) {
    return {
      ids: mentions.ids,
      reason: "mention",
      label: mentions.ids.map((id) => "@" + handle(members.find((m) => m.id === id)!)).join(" "),
    };
  }
  const picked = selected.filter((id) => members.some((m) => m.id === id));
  if (picked.length) {
    return { ids: picked, reason: "selected", label: picked.join(", ") };
  }
  if (members.length === 1) {
    return { ids: [members[0].id], reason: "only-member", label: members[0].id };
  }
  const previous = lastAddressed(tasks, members);
  if (previous.length) {
    return { ids: previous, reason: "follow-up", label: previous.join(", ") };
  }
  return { ids: [], reason: "none", label: "" };
}

export const reasonText: Record<RouteReason, string> = {
  mention: "Addressed by mention",
  everyone: "Addressed to every member",
  selected: "Recipients you chose",
  "only-member": "Only member of this chat",
  "follow-up": "Follow-up to your last message",
  none: "Type @ to address an agent",
};

/** The open `@query` ending at the caret, if the user is typing a mention. */
export function mentionQuery(
  draft: string,
  caret: number,
): { start: number; query: string } | null {
  const before = draft.slice(0, caret);
  const match = /(^|\s)@([A-Za-z0-9_.:-]*)$/.exec(before);
  if (!match) return null;
  return { start: before.length - match[2].length - 1, query: match[2] };
}

export function mentionOptions(query: string, members: Member[]): Member[] {
  const q = query.toLowerCase();
  const all: Member[] = [{ id: EVERYONE, name: EVERYONE }, ...members];
  return all.filter(
    (m) => !q || m.id.toLowerCase().includes(q) || handle(m).toLowerCase().includes(q),
  );
}

export function applyMention(
  draft: string,
  caret: number,
  start: number,
  member: Member,
): { text: string; caret: number } {
  const insert = "@" + (member.id === EVERYONE ? EVERYONE : handle(member)) + " ";
  const text = draft.slice(0, start) + insert + draft.slice(caret);
  return { text, caret: start + insert.length };
}
