import { describe, it, expect, vi } from "vitest";
import { routeWithLead, RoutingError } from "./leadRouting";

const input = { conversationId: "c1", lead: "omp:lead", text: "hi", purpose: "" };
const fast = { intervalMs: 1, timeoutMs: 500 };

describe("lead routing client", () => {
  it("waits for the lead's verified answer", async () => {
    const call = vi
      .fn()
      .mockResolvedValueOnce({ taskId: "t1", state: "queued" })
      .mockResolvedValueOnce({ taskId: "t1", state: "running" })
      .mockResolvedValueOnce({ taskId: "t1", state: "completed", agentIds: ["a:x"], reason: "fits" });
    await expect(routeWithLead(input, call as never, fast)).resolves.toEqual({ ids: ["a:x"], reason: "fits" });
    expect(call).toHaveBeenCalledWith("routing.route", expect.objectContaining({ leadAgentId: "omp:lead" }));
  });
  it("surfaces unreadable answers instead of guessing recipients", async () => {
    const call = vi
      .fn()
      .mockResolvedValueOnce({ taskId: "t1", state: "queued" })
      .mockResolvedValueOnce({ taskId: "t1", state: "completed", error: "routing answer was not readable" });
    await expect(routeWithLead(input, call as never, fast)).rejects.toThrow(RoutingError);
  });
  it("gives up when the lead never answers", async () => {
    const call = vi.fn().mockImplementation(async (op: string) =>
      op === "routing.route" ? { taskId: "t1", state: "queued" } : { taskId: "t1", state: "running" },
    );
    await expect(routeWithLead(input, call as never, { intervalMs: 1, timeoutMs: 20 })).rejects.toThrow(/too long/);
  });
});
