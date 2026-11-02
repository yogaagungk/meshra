import type { FlowEdge, FlowNode } from "./automation-api";
import { layoutFlow, NODE_H, NODE_W } from "./flowLayout";

export type NodeStatus = "reserved" | "admitted" | "completed" | "failed" | "cancelled" | "skipped";

const statusText: Record<string, string> = {
  reserved: "Waiting",
  admitted: "Running",
  completed: "Done",
  failed: "Failed",
  cancelled: "Cancelled",
  skipped: "Skipped",
};

/**
 * Read-only picture of a flow. When `status` is given (from a run), each step
 * shows its state, so the same drawing explains both design and execution.
 */
export function FlowGraph({
  nodes,
  edges,
  label,
  status,
  title,
}: {
  nodes: FlowNode[];
  edges: FlowEdge[];
  label: (node: FlowNode, index: number) => string;
  status?: Record<string, string | undefined>;
  title: string;
}) {
  const layout = layoutFlow(nodes, edges);
  if (!nodes.length) return null;
  const at = new Map(layout.nodes.map((p) => [p.id, p]));
  return (
    <div className="flow-graph-scroll">
      <svg
        className="flow-graph"
        role="img"
        aria-label={title}
        width={layout.width}
        height={layout.height}
        viewBox={`0 0 ${layout.width} ${layout.height}`}
      >
        <defs>
          <marker id="flow-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
            <path d="M0 0 L8 4 L0 8 z" className="flow-arrow-head" />
          </marker>
        </defs>
        {edges.map((edge, i) => {
          const a = at.get(edge.from);
          const b = at.get(edge.to);
          if (!a || !b || a === b) return null;
          const x1 = a.x + NODE_W;
          const y1 = a.y + NODE_H / 2;
          const x2 = b.x;
          const y2 = b.y + NODE_H / 2;
          const mid = (x1 + x2) / 2;
          return (
            <path
              key={i}
              className="flow-edge"
              d={`M${x1} ${y1} C${mid} ${y1} ${mid} ${y2} ${x2 - 2} ${y2}`}
              markerEnd="url(#flow-arrow)"
              fill="none"
            />
          );
        })}
        {nodes.map((node, index) => {
          const p = at.get(node.id)!;
          const state = status?.[node.id];
          return (
            <g key={node.id} transform={`translate(${p.x} ${p.y})`} className={"flow-node" + (state ? " flow-node-" + state : "")}>
              <rect width={NODE_W} height={NODE_H} rx={9} />
              <text x={12} y={25} className="flow-node-title">
                {clip(label(node, index), 24)}
              </text>
              <text x={12} y={46} className="flow-node-sub">
                {state ? statusText[state] || state : clip(node.instruction || "No instruction yet", 28)}
              </text>
            </g>
          );
        })}
      </svg>
      {layout.cycle && <p role="alert" className="text-error">These steps depend on each other in a loop. A flow cannot loop.</p>}
    </div>
  );
}

function clip(text: string, max: number) {
  return text.length > max ? text.slice(0, max - 1) + "…" : text;
}
