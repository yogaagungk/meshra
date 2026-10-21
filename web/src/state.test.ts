import { describe, it, expect, vi, afterEach } from "vitest";
import { mergeTasks, stateLabel, type Task, type Page } from "./types";
import { api, setCSRF, APIError } from "./api";
const task = (id: string, seq: number, state = "queued") =>
  ({ taskId: id, seq, state }) as Task;
describe("durable timeline reconciliation", () => {
  it("deduplicates overlapping replay pages and orders by durable sequence", () => {
    const pages = [
      { items: [task("b", 20, "completed"), task("a", 10, "completed")] },
      { items: [task("a", 10), task("old", 1)] },
    ] as Page[];
    expect(mergeTasks(pages).map((t) => [t.taskId, t.state])).toEqual([
      ["old", "queued"],
      ["a", "completed"],
      ["b", "completed"],
    ]);
  });
  it("preserves uncertainty rather than claiming failure means no execution", () => {
    expect(stateLabel({ ...task("a", 1, "failed"), ambiguous: true })).toBe(
      "Outcome uncertain",
    );
    expect(stateLabel(task("a", 1, "dispatching"))).toBe("Accepted");
    expect(stateLabel(task("a", 1, "completed"))).toBe("Completed");
  });
});
describe("browser transport", () => {
  afterEach(() => vi.unstubAllGlobals());
  it("sends CSRF only on mutations and uses same-origin cookies without bearer auth", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValue({ ok: true, json: async () => ({}) });
    vi.stubGlobal("fetch", fetch);
    setCSRF("csrf-test");
    await api("send", { text: "hi" });
    const options = fetch.mock.calls[0][1];
    expect(options.credentials).toBe("same-origin");
    expect(options.headers["X-Meshra-CSRF"]).toBe("csrf-test");
    expect(options.headers.Authorization).toBeUndefined();
    await api("agents");
    expect(fetch.mock.calls[1][1].headers["X-Meshra-CSRF"]).toBeUndefined();
  });
  it("surfaces denied access without treating it as an empty successful response", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue({
          ok: false,
          status: 403,
          json: async () => ({ error: "context access denied" }),
        }),
    );
    await expect(api("send", {})).rejects.toMatchObject({
      status: 403,
      message: "context access denied",
    });
  });
});
