import type { FlowEdge, FlowNode } from "./automation-api";

export type Placed = { id: string; level: number; row: number; x: number; y: number };
export type Layout = {
  nodes: Placed[];
  width: number;
  height: number;
  cycle: boolean;
};

export const NODE_W = 196;
export const NODE_H = 64;
const GAP_X = 56;
const GAP_Y = 18;
const PAD = 12;

/**
 * Layers a flow left to right: a node sits one column after its deepest
 * parent. Edges that point at unknown nodes are ignored, and a cycle is
 * reported rather than laid out because the relay rejects cyclic flows.
 */
export function layoutFlow(nodes: Pick<FlowNode, "id">[], edges: FlowEdge[]): Layout {
  const ids = new Set(nodes.map((n) => n.id));
  const valid = edges.filter((e) => ids.has(e.from) && ids.has(e.to) && e.from !== e.to);
  const parents = new Map<string, string[]>();
  for (const n of nodes) parents.set(n.id, []);
  for (const e of valid) parents.get(e.to)!.push(e.from);

  const level = new Map<string, number>();
  let cycle = false;
  const visiting = new Set<string>();
  const depth = (id: string): number => {
    if (level.has(id)) return level.get(id)!;
    if (visiting.has(id)) {
      cycle = true;
      return 0;
    }
    visiting.add(id);
    const d = parents.get(id)!.reduce((max, p) => Math.max(max, depth(p) + 1), 0);
    visiting.delete(id);
    level.set(id, d);
    return d;
  };
  for (const n of nodes) depth(n.id);

  const rows = new Map<number, number>();
  const placed: Placed[] = nodes.map((n) => {
    const l = level.get(n.id) ?? 0;
    const row = rows.get(l) ?? 0;
    rows.set(l, row + 1);
    return { id: n.id, level: l, row, x: PAD + l * (NODE_W + GAP_X), y: PAD + row * (NODE_H + GAP_Y) };
  });
  const levels = placed.reduce((m, p) => Math.max(m, p.level + 1), 1);
  const tallest = Math.max(1, ...rows.values());
  return {
    nodes: placed,
    cycle,
    width: PAD * 2 + levels * NODE_W + (levels - 1) * GAP_X,
    height: PAD * 2 + tallest * NODE_H + (tallest - 1) * GAP_Y,
  };
}

/** Whether adding `from -> to` would close a loop. */
export function wouldCycle(edges: FlowEdge[], from: string, to: string): boolean {
  if (from === to) return true;
  const next = new Map<string, string[]>();
  for (const e of edges) next.set(e.from, [...(next.get(e.from) || []), e.to]);
  const stack = [to];
  const seen = new Set<string>();
  while (stack.length) {
    const id = stack.pop()!;
    if (id === from) return true;
    if (seen.has(id)) continue;
    seen.add(id);
    stack.push(...(next.get(id) || []));
  }
  return false;
}
