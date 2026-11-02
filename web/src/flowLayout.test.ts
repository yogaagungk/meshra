import { describe, it, expect } from "vitest";
import { layoutFlow, wouldCycle } from "./flowLayout";

const n = (...ids: string[]) => ids.map((id) => ({ id }));

describe("flow layout", () => {
  it("places each node one column after its deepest parent", () => {
    const layout = layoutFlow(n("a", "b", "c", "d"), [
      { from: "a", to: "b" },
      { from: "a", to: "c" },
      { from: "b", to: "d" },
      { from: "c", to: "d" },
    ]);
    const level = Object.fromEntries(layout.nodes.map((p) => [p.id, p.level]));
    expect(level).toEqual({ a: 0, b: 1, c: 1, d: 2 });
    expect(layout.cycle).toBe(false);
    expect(layout.width).toBeGreaterThan(0);
  });
  it("uses the longest path, so a shortcut edge does not flatten the graph", () => {
    const layout = layoutFlow(n("a", "b", "c"), [
      { from: "a", to: "b" },
      { from: "b", to: "c" },
      { from: "a", to: "c" },
    ]);
    expect(layout.nodes.find((p) => p.id === "c")!.level).toBe(2);
  });
  it("ignores unknown or self edges and reports cycles", () => {
    expect(layoutFlow(n("a"), [{ from: "a", to: "ghost" }, { from: "a", to: "a" }]).cycle).toBe(false);
    expect(layoutFlow(n("a", "b"), [{ from: "a", to: "b" }, { from: "b", to: "a" }]).cycle).toBe(true);
  });
  it("detects edges that would close a loop", () => {
    const edges = [{ from: "a", to: "b" }, { from: "b", to: "c" }];
    expect(wouldCycle(edges, "c", "a")).toBe(true);
    expect(wouldCycle(edges, "a", "c")).toBe(false);
    expect(wouldCycle(edges, "b", "b")).toBe(true);
  });
});
