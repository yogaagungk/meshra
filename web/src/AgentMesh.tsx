import { useMemo, useState } from "react";
import { User } from "lucide-react";
import { AgentAvatar } from "./components/AgentAvatar";
import { deptOf } from "./HarnessLogo";
import type { Agent } from "./types";
import "./agent-mesh.css";

type Pt = { x: number; y: number };

/** Curved SVG path between two points in a 900x560 box. */
function curve(a: Pt, b: Pt, bend: number) {
  const mx = (a.x + b.x) / 2;
  const my = (a.y + b.y) / 2;
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const l = Math.hypot(dx, dy) || 1;
  return `M${a.x} ${a.y} Q${mx - (dy / l) * bend * 2} ${my + (dx / l) * bend * 2} ${b.x} ${b.y}`;
}

/**
 * Mesh view of the registered agents. Threads are real: you can reach every
 * registered agent, and an agent-to-agent thread is drawn only where its
 * policy lists the other agent as an allowed destination.
 */
export function AgentMesh({
  agents,
  onSelect,
}: {
  agents: Agent[];
  onSelect: (a: Agent) => void;
}) {
  const [hover, setHover] = useState("");
  const layout = useMemo(() => {
    const you: Pt = { x: 100, y: 280 };
    const pos: Record<string, Pt> = {};
    const n = agents.length;
    agents.forEach((a, i) => {
      const t = n === 1 ? 0.5 : i / (n - 1);
      const ang = (-72 + 144 * t) * (Math.PI / 180);
      pos[a.id] = { x: 520 + 300 * Math.cos(ang), y: 280 + 215 * Math.sin(ang) };
    });
    const ids = new Set(agents.map((a) => a.id));
    const links: { from: string; to: string }[] = [];
    agents.forEach((a) =>
      (a.policy.destinations || []).forEach((d) => {
        if (ids.has(d) && d !== a.id) links.push({ from: a.id, to: d });
      }),
    );
    return { you, pos, links };
  }, [agents]);
  const lit = (id: string) => hover === id;
  return (
    <section className="mesh-view" aria-label="Agent mesh">
      <div className="mesh-stage">
        <svg viewBox="0 0 900 560" className="mesh-edges" aria-hidden="true">
          {agents.map((a, i) => (
            <path
              key={"y" + a.id}
              d={curve(layout.you, layout.pos[a.id], i % 2 ? 18 : -18)}
              className={"mesh-edge" + (a.state === "busy" ? " live" : "") + (lit(a.id) ? " lit" : "")}
            />
          ))}
          {layout.links.map((l) => (
            <path
              key={l.from + ">" + l.to}
              d={curve(layout.pos[l.from], layout.pos[l.to], 40)}
              className={"mesh-edge peer" + (lit(l.from) || lit(l.to) ? " lit" : "")}
            />
          ))}
        </svg>
        <div
          className="mesh-node you"
          style={{ left: (layout.you.x / 900) * 100 + "%", top: (layout.you.y / 560) * 100 + "%" }}
        >
          <span className="mesh-disc">
            <User size={22} strokeWidth={1.8} />
          </span>
          <strong>You</strong>
          <small>Operator</small>
        </div>
        {agents.map((a) => (
          <button
            key={a.id}
            className={"mesh-node agent" + (a.state === "busy" ? " busy" : "")}
            data-harness={deptOf(a.harness)}
            style={{
              left: (layout.pos[a.id].x / 900) * 100 + "%",
              top: (layout.pos[a.id].y / 560) * 100 + "%",
            }}
            onClick={() => onSelect(a)}
            onMouseEnter={() => setHover(a.id)}
            onMouseLeave={() => setHover("")}
            onFocus={() => setHover(a.id)}
            onBlur={() => setHover("")}
            aria-label={`${a.name || a.id}, ${a.id}, ${a.state}`}
          >
            <span className="mesh-disc">
              <AgentAvatar icon={a.avatarIcon} color={a.avatarColor} size={34} />
            </span>
            <strong>{a.name || a.id}</strong>
            <small>{a.id}</small>
          </button>
        ))}
      </div>
      <ul className="mesh-legend">
        <li><i className="k thread" />Thread with you</li>
        <li><i className="k peer" />Allowed hand-off between agents</li>
        <li><i className="k live" />Working now</li>
      </ul>
    </section>
  );
}
