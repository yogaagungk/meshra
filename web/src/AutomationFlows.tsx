import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Trash2, Workflow } from "lucide-react";
import { automation, type Flow, type FlowEdge, type FlowNode } from "./automation-api";
import { Empty, Notice } from "./App";
import { Button } from "./components/ui/button";
import { FlowGraph } from "./FlowGraph";
import { layoutFlow, wouldCycle } from "./flowLayout";
import { goAutomation, useProfiles } from "./automationShared";

const emptyDraft = { name: "", nodes: [] as FlowNode[], edges: [] as FlowEdge[] };

export function FlowsPanel({ focus }: { focus?: string }) {
  const [selected, setSelected] = useState<string | "new" | undefined>(focus);
  const flows = useQuery({
    queryKey: ["flows"],
    queryFn: () => automation("flows.list", { limit: 100 }),
  });
  const items = flows.data?.items || [];
  const current = selected && selected !== "new" ? items.find((f) => f.id === selected) : undefined;
  const editing = selected === "new" || !!current;

  return (
    <>
      <div className="section-toolbar">
        <span className="muted">
          A flow is a set of steps where each step runs one profile. A step starts only after the steps it follows have succeeded.
        </span>
        <Button onClick={() => setSelected("new")}>
          <Plus size={16} />
          New flow
        </Button>
      </div>
      {flows.isError && <Notice>{flows.error.message}</Notice>}
      {flows.isPending ? (
        <p role="status">Loading flows…</p>
      ) : !items.length && !editing ? (
        <Empty
          icon={Workflow}
          title="No flows yet"
          action={
            <Button onClick={() => setSelected("new")}>
              <Plus size={16} />
              Build your first flow
            </Button>
          }
        >
          Chain profiles together, for example a planner, then a builder, then a reviewer. A routine can then run the whole chain on a schedule.
        </Empty>
      ) : (
        <div className="automation-split">
          <ul className="automation-pick-list" aria-label="Flows">
            {items.map((flow) => (
              <li key={flow.id}>
                <button aria-pressed={flow.id === selected} onClick={() => setSelected(flow.id)}>
                  <strong>{flow.name || "Untitled flow"}</strong>
                  <small className="muted">
                    {flow.nodes.length} step{flow.nodes.length === 1 ? "" : "s"} · revision {flow.revision}
                  </small>
                </button>
              </li>
            ))}
            {selected === "new" && (
              <li>
                <button aria-pressed>
                  <strong>New flow</strong>
                  <small className="muted">Not saved yet</small>
                </button>
              </li>
            )}
          </ul>
          {editing ? (
            <FlowEditor
              key={selected === "new" ? "new" : current!.id + ":" + current!.revision}
              flow={current}
              onSaved={(flow) => setSelected(flow.id)}
            />
          ) : (
            <p className="muted automation-split-hint">Select a flow to see its steps.</p>
          )}
        </div>
      )}
    </>
  );
}

function FlowEditor({ flow, onSaved }: { flow?: Flow; onSaved: (flow: Flow) => void }) {
  const client = useQueryClient();
  const profiles = useProfiles();
  const [draft, setDraft] = useState(() => (flow ? { name: flow.name || "", nodes: flow.nodes, edges: flow.edges } : emptyDraft));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const routines = useQuery({
    queryKey: ["routines"],
    queryFn: () => automation("routines.list", { limit: 50 }),
  });
  const usedBy = (routines.data?.items || []).filter((r) => flow && r.flowId === flow.id);
  const active = profiles.items.filter((p) => !p.archived);
  const { nodes, edges } = draft;
  const stepLabel = (node: FlowNode, index: number) => `${index + 1}. ${profiles.name(node.profileId)}`;
  const cycle = layoutFlow(nodes, edges).cycle;
  const problem = !nodes.length
    ? "Add at least one step."
    : nodes.some((n) => !n.profileId)
      ? "Choose a profile for every step."
      : nodes.some((n) => !n.instruction.trim())
        ? "Write an instruction for every step."
        : cycle
          ? "Steps cannot follow each other in a loop."
          : "";
  const dirty =
    !flow || JSON.stringify({ n: draft.name, a: nodes, e: edges }) !== JSON.stringify({ n: flow.name || "", a: flow.nodes, e: flow.edges });

  function setNode(index: number, patch: Partial<FlowNode>) {
    setDraft((d) => ({ ...d, nodes: d.nodes.map((n, i) => (i === index ? { ...n, ...patch } : n)) }));
  }
  function addStep() {
    setDraft((d) => {
      let n = d.nodes.length + 1;
      while (d.nodes.some((x) => x.id === "step-" + n)) n++;
      const id = "step-" + n;
      const previous = d.nodes[d.nodes.length - 1];
      return {
        ...d,
        nodes: [...d.nodes, { id, profileId: previous?.profileId || "", instruction: "" }],
        edges: previous ? [...d.edges, { from: previous.id, to: id }] : d.edges,
      };
    });
  }
  function removeStep(index: number) {
    const id = nodes[index].id;
    setDraft((d) => ({
      ...d,
      nodes: d.nodes.filter((_, i) => i !== index),
      edges: d.edges.filter((e) => e.from !== id && e.to !== id),
    }));
  }
  function setFollows(node: FlowNode, parentId: string, follows: boolean) {
    setDraft((d) => {
      const has = d.edges.some((e) => e.from === parentId && e.to === node.id);
      if (follows && !has) return { ...d, edges: [...d.edges, { from: parentId, to: node.id }] };
      if (!follows) return { ...d, edges: d.edges.filter((e) => !(e.from === parentId && e.to === node.id)) };
      return d;
    });
  }

  async function save() {
    setBusy(true);
    setError("");
    try {
      const saved = await automation("flows.save", {
        ...(flow ? { id: flow.id, expectedRevision: flow.revision } : {}),
        name: draft.name.trim() || undefined,
        nodes,
        edges,
      });
      await client.invalidateQueries({ queryKey: ["flows"] });
      onSaved(saved);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="automation-card flow-editor" aria-label="Flow editor">
      {error && <Notice>{error}</Notice>}
      <label htmlFor="flow-name">Flow name</label>
      <input
        id="flow-name"
        value={draft.name}
        maxLength={128}
        placeholder="Plan, build, review"
        onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
      />
      <FlowGraph
        nodes={nodes}
        edges={edges}
        label={stepLabel}
        title={"Flow diagram: " + (nodes.length || "no") + " steps"}
      />
      <ol className="flow-steps">
        {nodes.map((node, index) => {
          const others = nodes.filter((n) => n.id !== node.id);
          return (
            <li key={node.id} className="flow-step">
              <header>
                <strong>Step {index + 1}</strong>
                <Button variant="ghost" size="sm" aria-label={`Remove step ${index + 1}`} onClick={() => removeStep(index)}>
                  <Trash2 size={14} /> Remove
                </Button>
              </header>
              <label htmlFor={"step-profile-" + node.id}>Runs this profile</label>
              <select id={"step-profile-" + node.id} value={node.profileId} onChange={(e) => setNode(index, { profileId: e.target.value })}>
                <option value="">Choose a profile…</option>
                {active.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
              <label htmlFor={"step-instruction-" + node.id}>Instruction</label>
              <textarea
                id={"step-instruction-" + node.id}
                rows={2}
                maxLength={65536}
                value={node.instruction}
                onChange={(e) => setNode(index, { instruction: e.target.value })}
                placeholder="What should this step do with the previous steps' results?"
              />
              {others.length > 0 && (
                <fieldset className="flow-follows">
                  <legend>Starts after</legend>
                  {others.map((other) => {
                    const checked = edges.some((e) => e.from === other.id && e.to === node.id);
                    const blocked = !checked && wouldCycle(edges, other.id, node.id);
                    return (
                      <label key={other.id} className={blocked ? "is-disabled" : ""} title={blocked ? "That would create a loop" : undefined}>
                        <input type="checkbox" checked={checked} disabled={blocked} onChange={(e) => setFollows(node, other.id, e.target.checked)} />
                        Step {nodes.indexOf(other) + 1}
                      </label>
                    );
                  })}
                  {!edges.some((e) => e.to === node.id) && <small className="muted">Nothing: starts immediately.</small>}
                </fieldset>
              )}
            </li>
          );
        })}
      </ol>
      <Button variant="outline" onClick={addStep}>
        <Plus size={14} /> Add step
      </Button>
      {flow && (
        <p className="muted">
          {usedBy.length ? (
            <>
              Used by{" "}
              {usedBy.map((r, i) => (
                <span key={r.id}>
                  {i > 0 && ", "}
                  <button className="link-button" onClick={() => goAutomation("routines")}>
                    {r.name}
                  </button>
                </span>
              ))}
              . Routines keep running the revision they pinned.
            </>
          ) : (
            "No routine uses this flow yet."
          )}
        </p>
      )}
      <div className="automation-sticky-actions">
        <Button disabled={busy || !!problem || !dirty} onClick={() => void save()}>
          {busy ? "Saving…" : flow ? "Save as revision " + (flow.revision + 1) : "Save flow"}
        </Button>
        {problem ? <span className="muted">{problem}</span> : !dirty ? <span className="muted">All changes saved</span> : null}
      </div>
    </section>
  );
}
