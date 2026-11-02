import { describe, it, expect } from "vitest";
import {
  applyMention,
  leadingMentions,
  mentionOptions,
  mentionQuery,
  resolveRoute,
} from "./routing";
import type { Task } from "./types";

const members = [
  { id: "claude:planner", name: "planner" },
  { id: "codex:builder", name: "builder" },
  { id: "omp:reviewer", name: "reviewer" },
];
const task = (messageId: string, from: string, to: string) =>
  ({ taskId: messageId + to, messageId, from, to }) as Task;

describe("recipient routing", () => {
  it("routes leading mentions by handle or exact ID and ignores unknown names", () => {
    expect(leadingMentions("@planner @CODEX:builder @ghost hi", members).ids).toEqual([
      "claude:planner",
      "codex:builder",
    ]);
    expect(leadingMentions("hello @planner", members).ids).toEqual([]);
  });
  it("sends @everyone to each member explicitly", () => {
    const route = resolveRoute({ draft: "@everyone status?", members, selected: [], tasks: [] });
    expect(route.reason).toBe("everyone");
    expect(route.ids).toHaveLength(3);
  });
  it("prefers a mention over a stored selection", () => {
    const route = resolveRoute({
      draft: "@reviewer look",
      members,
      selected: ["claude:planner"],
      tasks: [],
    });
    expect(route).toMatchObject({ ids: ["omp:reviewer"], reason: "mention" });
  });
  it("uses the only member, then the last addressed agents, and otherwise nothing", () => {
    expect(
      resolveRoute({ draft: "hi", members: [members[0]], selected: [], tasks: [] }).reason,
    ).toBe("only-member");
    const tasks = [
      task("m1", "human:operator", "codex:builder"),
      task("m1", "human:operator", "omp:reviewer"),
      task("m1r", "codex:builder", "human:operator"),
    ];
    expect(resolveRoute({ draft: "and?", members, selected: [], tasks })).toMatchObject({
      ids: ["codex:builder", "omp:reviewer"],
      reason: "follow-up",
    });
    expect(resolveRoute({ draft: "hi", members, selected: [], tasks: [] }).ids).toEqual([]);
  });
  it("never broadcasts when nothing resolves", () => {
    expect(resolveRoute({ draft: "@ghost hi", members, selected: [], tasks: [] }).reason).toBe(
      "none",
    );
  });
});

describe("mention autocomplete", () => {
  it("detects an open mention at the caret and completes it", () => {
    const draft = "please @bu";
    const open = mentionQuery(draft, draft.length)!;
    expect(open.query).toBe("bu");
    const [first] = mentionOptions(open.query, members);
    expect(first.id).toBe("codex:builder");
    expect(applyMention(draft, draft.length, open.start, first).text).toBe("please @builder ");
    expect(mentionQuery("mail me@home", 12)).toBeNull();
  });
});
