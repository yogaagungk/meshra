import { useEffect, useRef, useState, type FormEvent } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowUp,
  BookOpen,
  Clock,
  FileText,
  Layers3,
  ListOrdered,
  Pause,
  Play,
  Plus,
  RefreshCw,
  ShieldCheck,
  Sparkles,
  Trash2,
} from "lucide-react";
import {
  automation,
  type Flow,
  type FlowEdge,
  type FlowNode,
  type QueueState,
  type Routine,
  type RoutineRun,
  type RoutineTrigger,
  type Skill,
} from "./automation-api";
import type { RoutineToken, Template, TemplatePreview } from "./automation-api";
import { downloadAttachment, uploadAttachment } from "./automation-api";
import { api, collab } from "./api";
import type { Agent, Conversation } from "./types";
import { Approvals } from "./Approvals";
import { Empty, Notice } from "./App";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";
import { Markdown } from "./Markdown";
import { stateLabel } from "./types";
import { BindingControls } from "./BindingControls";

const TRIGGER_LABELS: Record<string, string> = {
  once: "Once",
  interval: "Every interval",
  cron: "Cron schedule",
  webhook: "Webhook",
  manual: "Manual only",
};

function randomKey() {
  return typeof crypto !== "undefined" && "randomUUID" in crypto
    ? crypto.randomUUID()
    : Math.random().toString(36).slice(2) + Date.now().toString(36);
}

/**
 * Automation is the durable surface: queued work, scheduled routines, flow
 * graphs, reviewed skills and reusable templates.
 */
export function Automation() {
  const [tab, setTab] = useState("queue");
  return (
    <div className="automation-page">
      <Approvals />
      <div className="automation-tabs" role="group" aria-label="Automation sections">
        {[
          ["queue", "Queue"],
          ["routines", "Routines"],
          ["flows", "Flows"],
          ["skills", "Skills"],
          ["templates", "Templates"],
        ].map(([key, label]) => (
          <button
            key={key}
            aria-pressed={tab === key}
            onClick={() => setTab(key)}
          >
            {label}
          </button>
        ))}
      </div>
      {tab === "queue" ? (
        <QueuePanel />
      ) : tab === "routines" ? (
        <RoutinesPanel />
      ) : tab === "flows" ? (
        <FlowsPanel />
      ) : tab === "skills" ? (
        <SkillsPanel />
      ) : (
        <TemplatesPanel />
      )}
    </div>
  );
}

function QueuePanel() {
  const client = useQueryClient();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [orders, setOrders] = useState<Record<string, string[]>>({});
  const queues = useQuery({
    queryKey: ["queue"],
    queryFn: () => automation("queue.list", {}),
    refetchInterval: 15000,
    refetchIntervalInBackground: false,
  });
  const items = queues.data?.items || [];

  async function run(
    state: QueueState,
    operation: "queue.pause" | "queue.resume",
  ) {
    setBusy(state.agentId);
    setError("");
    try {
      await automation(operation, {
        agentId: state.agentId,
        expectedRevision: state.revision,
      });
      await client.invalidateQueries({ queryKey: ["queue"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  async function reorder(state: QueueState, taskIds: string[]) {
    setBusy(state.agentId);
    setError("");
    try {
      await automation("queue.reorder", {
        agentId: state.agentId,
        expectedRevision: state.revision,
        taskIds,
      });
      setOrders((v) => {
        const next = { ...v };
        delete next[state.agentId];
        return next;
      });
      await client.invalidateQueries({ queryKey: ["queue"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  function order(state: QueueState) {
    return orders[state.agentId] || state.tasks.map((t) => t.taskId);
  }

  function move(state: QueueState, index: number, delta: number) {
    const current = [...order(state)];
    const target = index + delta;
    if (target < 0 || target >= current.length) return;
    const [moved] = current.splice(index, 1);
    current.splice(target, 0, moved);
    setOrders((v) => ({ ...v, [state.agentId]: current }));
  }

  return (
    <>
      <div className="section-toolbar">
        <span className="muted">
          Queued work per recipient. Reordering changes dispatch order only.
        </span>
        <Button variant="outline" onClick={() => void queues.refetch()}>
          <RefreshCw size={15} />
          Refresh
        </Button>
      </div>
      {error && <Notice>{error}</Notice>}
      {queues.isError && <Notice>{queues.error.message}</Notice>}
      {queues.isPending ? (
        <p role="status">Loading queues…</p>
      ) : !items.length ? (
        <Empty icon={ListOrdered} title="Nothing is queued">
          Queued tasks appear here with their recipient, so you can pause a
          recipient or reorder its pending work.
        </Empty>
      ) : (
        <div className="automation-queue-list">
          {items.map((state) => {
            const current = order(state);
            const changed =
              current.join(",") !== state.tasks.map((t) => t.taskId).join(",");
            return (
              <section className="automation-card" key={state.agentId}>
                <header className="automation-card-head">
                  <div>
                    <strong>{state.agentId}</strong>
                    {state.profileId && (
                      <small className="muted">Profile {state.profileId}</small>
                    )}
                  </div>
                  <div className="automation-actions">
                    <span className="automation-attention-status">
                      {state.paused ? "Paused" : "Dispatching"}
                    </span>
                    {state.profileId && state.tasks[0]?.conversationId && (
                      <BindingControls
                        profileId={state.profileId}
                        conversationId={state.tasks[0].conversationId}
                        agentId={state.agentId}
                      />
                    )}
                    {state.paused ? (
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={busy === state.agentId}
                        onClick={() => void run(state, "queue.resume")}
                      >
                        <Play size={13} />
                        Resume
                      </Button>
                    ) : (
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={busy === state.agentId}
                        onClick={() => void run(state, "queue.pause")}
                      >
                        <Pause size={13} />
                        Pause
                      </Button>
                    )}
                  </div>
                </header>
                {!state.tasks.length ? (
                  <p className="muted">No queued tasks for this recipient.</p>
                ) : (
                  <ol className="automation-task-list">
                    {current.map((taskId, index) => {
                      const task = state.tasks.find((t) => t.taskId === taskId);
                      return (
                        <li key={taskId}>
                          <div className="automation-task-body">
                            <span className="automation-task-text">
                              {task?.text || taskId}
                            </span>
                            <small className="muted">
                              {task ? stateLabel(task) : taskId}
                            </small>
                          </div>
                          <div className="automation-actions">
                            <Button
                              variant="ghost"
                              size="icon"
                              aria-label={"Move " + taskId + " up"}
                              disabled={index === 0}
                              onClick={() => move(state, index, -1)}
                            >
                              <ArrowUp size={14} />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              aria-label={"Move " + taskId + " down"}
                              disabled={index === current.length - 1}
                              onClick={() => move(state, index, 1)}
                            >
                              <ArrowDown size={14} />
                            </Button>
                          </div>
                        </li>
                      );
                    })}
                  </ol>
                )}
                <footer className="automation-card-foot">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={!changed || busy === state.agentId}
                    onClick={() => void reorder(state, current)}
                  >
                    Apply new order
                  </Button>
                  {changed && (
                    <span className="muted">
                      The order must name every queued task of this recipient.
                    </span>
                  )}
                </footer>
              </section>
            );
          })}
        </div>
      )}
    </>
  );
}

function RoutinesPanel() {
  const client = useQueryClient();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [run, setRun] = useState<RoutineRun | null>(null);
  const [creating, setCreating] = useState(false);
  const routines = useQuery({
    queryKey: ["routines"],
    queryFn: () => automation("routines.list", { limit: 50 }),
  });
  const items = routines.data?.items || [];

  async function setEnabled(routine: Routine, enabled: boolean) {
    if (!routine.id || routine.revision === undefined) return;
    setBusy(routine.id);
    setError("");
    try {
      await automation("routines.setEnabled", {
        id: routine.id,
        enabled,
        expectedRevision: routine.revision,
      });
      await client.invalidateQueries({ queryKey: ["routines"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  async function runNow(routine: Routine) {
    if (!routine.id) return;
    setBusy(routine.id);
    setError("");
    try {
      const result = await automation("routines.run", {
        id: routine.id,
        idempotencyKey: randomKey(),
      });
      setRun(result);
      await client.invalidateQueries({ queryKey: ["routines"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  return (
    <>
      <div className="section-toolbar">
        <span className="muted">
          Scheduled work with a durable receipt for every occurrence.
        </span>
        <Button onClick={() => setCreating(true)}>
          <Plus size={16} />
          New routine
        </Button>
      </div>
      {error && <Notice>{error}</Notice>}
      {routines.isError && <Notice>{routines.error.message}</Notice>}
      {run && (
        <Notice>
          Run {run.runId}: {run.state}
          {run.reason ? " · " + run.reason : ""}
        </Notice>
      )}
      {routines.isPending ? (
        <p role="status">Loading routines…</p>
      ) : !items.length ? (
        <Empty
          icon={Clock}
          title="No routines yet"
          action={
            <Button onClick={() => setCreating(true)}>
              <Plus size={16} />
              Create a routine
            </Button>
          }
        >
          A routine runs one profile on a schedule, and keeps a receipt for
          every occurrence it claims.
        </Empty>
      ) : (
        <div className="automation-card-list">
          {items.map((routine) => (
            <section className="automation-card" key={routine.id || routine.name}>
              <header className="automation-card-head">
                <div>
                  <strong>{routine.name}</strong>
                  <small className="muted">
                    {routine.trigger
                      ? TRIGGER_LABELS[routine.trigger.kind] ||
                        routine.trigger.kind
                      : "Schedule"}
                    {routine.nextDueAt
                      ? " · next " + new Date(routine.nextDueAt).toLocaleString()
                      : ""}
                  </small>
                </div>
                <div className="automation-actions">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy === routine.id || !routine.id}
                    onClick={() => void runNow(routine)}
                  >
                    <Play size={13} />
                    Run now
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy === routine.id || !routine.id}
                    onClick={() => void setEnabled(routine, !routine.enabled)}
                  >
                    {routine.enabled ? "Pause" : "Enable"}
                  </Button>
                </div>
              </header>
              <p className="automation-card-text">{routine.instruction}</p>
              <div className="agent-card-details">
                <span>Profile {routine.profileId}</span>
                {routine.flowId && <span>Flow {routine.flowId}</span>}
                <span>
                  {routine.autoStart
                    ? "Starts its own bindings"
                    : "Keeps bindings stopped"}
                </span>
                {routine.webhookSecretStatus && (
                  <span>Webhook secret {routine.webhookSecretStatus}</span>
                )}
              </div>
            </section>
          ))}
        </div>
      )}
      <RoutineDialog open={creating} onClose={() => setCreating(false)} />
    </>
  );
}

function RoutineDialog({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}) {
  const client = useQueryClient();
  const [name, setName] = useState("");
  const [profileId, setProfileId] = useState("");
  const [instruction, setInstruction] = useState("");
  const [kind, setKind] = useState<RoutineTrigger["kind"]>("interval");
  const [everySeconds, setEverySeconds] = useState("3600");
  const [expression, setExpression] = useState("");
  const [autoStart, setAutoStart] = useState(false);
  const [enabled, setEnabled] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [secret, setSecret] = useState("");

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    const trigger: RoutineTrigger =
      kind === "cron"
        ? { kind: "cron", expression, timeZone: "UTC" }
        : kind === "interval"
          ? { kind: "interval", everySeconds: Number(everySeconds) }
          : { kind };
    try {
      const result = await automation("routines.save", {
        routine: {
          name: name.trim(),
          profileId,
          instruction,
          trigger,
          enabled,
          autoStart,
          timeoutSeconds: 3600,
        },
      });
      await client.invalidateQueries({ queryKey: ["routines"] });
      if (result.webhookSecret) {
        setSecret(result.webhookSecret);
        return;
      }
      onClose();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onClose}
      title="New routine"
      description="A routine claims one occurrence at a time and keeps a receipt for it."
    >
      <form className="form-stack" onSubmit={save}>
        <label htmlFor="routine-name">Name</label>
        <input
          id="routine-name"
          required
          maxLength={128}
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <label htmlFor="routine-profile">Profile</label>
        <input
          id="routine-profile"
          required
          value={profileId}
          onChange={(e) => setProfileId(e.target.value)}
          placeholder="Profile id"
        />
        <label htmlFor="routine-instruction">Instruction</label>
        <textarea
          id="routine-instruction"
          rows={4}
          maxLength={65536}
          value={instruction}
          onChange={(e) => setInstruction(e.target.value)}
        />
        <label htmlFor="routine-trigger">Trigger</label>
        <select
          id="routine-trigger"
          value={kind}
          onChange={(e) => setKind(e.target.value as RoutineTrigger["kind"])}
        >
          <option value="interval">Every interval</option>
          <option value="cron">Cron schedule</option>
          <option value="once">Once</option>
          <option value="manual">Manual only</option>
        </select>
        {kind === "interval" && (
          <>
            <label htmlFor="routine-interval">Interval in seconds</label>
            <input
              id="routine-interval"
              type="number"
              min={60}
              value={everySeconds}
              onChange={(e) => setEverySeconds(e.target.value)}
            />
          </>
        )}
        {kind === "cron" && (
          <>
            <label htmlFor="routine-cron">Cron expression</label>
            <input
              id="routine-cron"
              required
              placeholder="0 9 * * 1-5"
              value={expression}
              onChange={(e) => setExpression(e.target.value)}
            />
          </>
        )}
        <label className="automation-inline">
          <input
            type="checkbox"
            checked={autoStart}
            onChange={(e) => setAutoStart(e.target.checked)}
          />
          Allow this routine to start its own bindings
        </label>
        <label className="automation-inline">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          Enable immediately
        </label>
        {error && <Notice>{error}</Notice>}
        {secret && (
          <div className="policy-note">
            <ShieldCheck size={15} />
            Copy this webhook secret now. It is shown once and never again.
            <code className="mono">{secret}</code>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => {
                void navigator.clipboard.writeText(secret);
              }}
            >
              Copy secret
            </Button>
            <Button
              type="button"
              size="sm"
              onClick={() => {
                setSecret("");
                onClose();
              }}
            >
              Done
            </Button>
          </div>
        )}
        <div className="dialog-footer">
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || !name.trim() || !profileId}>
            {busy ? "Saving…" : "Create routine"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function FlowsPanel() {
  const [id, setId] = useState("");
  const [flow, setFlow] = useState<Flow | null>(null);
  const [nodes, setNodes] = useState<FlowNode[]>([]);
  const [edges, setEdges] = useState<FlowEdge[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function load(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const result = await automation("flows.get", { id: id.trim() });
      setFlow(result);
      setNodes(result.nodes || []);
      setEdges(result.edges || []);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function save() {
    setBusy(true);
    setError("");
    try {
      const result = await automation("flows.save", {
        ...(flow ? { id: flow.id, expectedRevision: flow.revision } : {}),
        nodes,
        edges,
      });
      setFlow(result);
      setNodes(result.nodes || []);
      setEdges(result.edges || []);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <div className="section-toolbar">
        <form className="automation-inline" onSubmit={load}>
          <label htmlFor="flow-id" className="sr-only">
            Flow id
          </label>
          <input
            id="flow-id"
            value={id}
            placeholder="Flow id"
            onChange={(e) => setId(e.target.value)}
          />
          <Button type="submit" variant="outline" disabled={busy || !id.trim()}>
            Load flow
          </Button>
        </form>
        <Button onClick={() => void save()} disabled={busy}>
          Save flow
        </Button>
      </div>
      {error && <Notice>{error}</Notice>}
      {flow && (
        <p className="muted">
          Flow {flow.id} · revision {flow.revision}
        </p>
      )}
      <div className="automation-flow-grid">
        <section className="automation-card">
          <header className="automation-card-head">
            <strong>Nodes</strong>
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                setNodes((v) => [
                  ...v,
                  { id: "node-" + (v.length + 1), profileId: "", instruction: "" },
                ])
              }
            >
              <Plus size={13} />
              Add node
            </Button>
          </header>
          <ul className="automation-node-list">
            {nodes.map((node, index) => (
              <li key={node.id}>
                <div className="automation-inline">
                  <input
                    aria-label={"Node " + (index + 1) + " id"}
                    value={node.id}
                    onChange={(e) =>
                      setNodes((v) =>
                        v.map((n, i) =>
                          i === index ? { ...n, id: e.target.value } : n,
                        ),
                      )
                    }
                  />
                  <input
                    aria-label={"Node " + (index + 1) + " profile"}
                    placeholder="Profile id"
                    value={node.profileId}
                    onChange={(e) =>
                      setNodes((v) =>
                        v.map((n, i) =>
                          i === index ? { ...n, profileId: e.target.value } : n,
                        ),
                      )
                    }
                  />
                  <input
                    aria-label={"Node " + (index + 1) + " instruction"}
                    placeholder="Instruction"
                    value={node.instruction}
                    onChange={(e) =>
                      setNodes((v) =>
                        v.map((n, i) =>
                          i === index
                            ? { ...n, instruction: e.target.value }
                            : n,
                        ),
                      )
                    }
                  />
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={"Remove node " + (index + 1)}
                    onClick={() =>
                      setNodes((v) => v.filter((_, i) => i !== index))
                    }
                  >
                    <Trash2 size={14} />
                  </Button>
                </div>
              </li>
            ))}
            {!nodes.length && <li className="muted">No nodes yet.</li>}
          </ul>
        </section>
        <section className="automation-card">
          <header className="automation-card-head">
            <strong>Edges</strong>
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                setEdges((v) => [...v, { from: "", to: "" }])
              }
            >
              <Plus size={13} />
              Add edge
            </Button>
          </header>
          <ul className="automation-node-list">
            {edges.map((edge, index) => (
              <li key={index}>
                <div className="automation-inline">
                  <input
                    aria-label={"Edge " + (index + 1) + " from"}
                    placeholder="From node"
                    value={edge.from}
                    onChange={(e) =>
                      setEdges((v) =>
                        v.map((edgeValue, i) =>
                          i === index ? { ...edgeValue, from: e.target.value } : edgeValue,
                        ),
                      )
                    }
                  />
                  <input
                    aria-label={"Edge " + (index + 1) + " to"}
                    placeholder="To node"
                    value={edge.to}
                    onChange={(e) =>
                      setEdges((v) =>
                        v.map((edgeValue, i) =>
                          i === index ? { ...edgeValue, to: e.target.value } : edgeValue,
                        ),
                      )
                    }
                  />
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={"Remove edge " + (index + 1)}
                    onClick={() =>
                      setEdges((v) => v.filter((_, i) => i !== index))
                    }
                  >
                    <Trash2 size={14} />
                  </Button>
                </div>
              </li>
            ))}
            {!edges.length && <li className="muted">No edges yet.</li>}
          </ul>
        </section>
      </div>
    </>
  );
}

function SkillsPanel() {
  const client = useQueryClient();
  const [selected, setSelected] = useState<Skill | null>(null);
  const [markdown, setMarkdown] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const skills = useQuery({
    queryKey: ["skills"],
    queryFn: () => automation("skills.list", {}),
  });
  const items = skills.data?.items || [];

  async function read(skill: Skill) {
    if (!skill.id) return;
    setBusy(true);
    setError("");
    try {
      const result = await automation("skills.read", { id: skill.id });
      setSelected(result);
      setMarkdown(result.markdown || "");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function save() {
    if (!selected?.id) return;
    setBusy(true);
    setError("");
    try {
      await automation("skills.save", {
        id: selected.id,
        ...(selected.revision !== undefined
          ? { expectedRevision: selected.revision }
          : {}),
        name: selected.name || "",
        description: selected.description || "",
        markdown,
      });
      await client.invalidateQueries({ queryKey: ["skills"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <div className="section-toolbar">
        <span className="muted">
          Reviewed instructions a profile can be pinned to. Nothing here runs
          on its own.
        </span>
      </div>
      {error && <Notice>{error}</Notice>}
      {skills.isError && <Notice>{skills.error.message}</Notice>}
      {skills.isPending ? (
        <p role="status">Loading skills…</p>
      ) : !items.length ? (
        <Empty icon={Sparkles} title="No skills yet">
          Skills are local, reviewed instruction documents. They never install
          packages or grant execution rights.
        </Empty>
      ) : (
        <div className="automation-card-list">
          {items.map((skill) => (
            <section className="automation-card" key={skill.id || skill.name}>
              <header className="automation-card-head">
                <div>
                  <strong>{skill.name}</strong>
                  <small className="muted">{skill.description}</small>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy || !skill.id}
                  onClick={() => void read(skill)}
                >
                  <BookOpen size={13} />
                  Open
                </Button>
              </header>
              {skill.version !== undefined && (
                <div className="agent-card-details">
                  <span>Version {skill.version}</span>
                </div>
              )}
            </section>
          ))}
        </div>
      )}
      {selected && (
        <section className="automation-card">
          <header className="automation-card-head">
            <strong>{selected.name}</strong>
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={() => void save()}
            >
              Save skill
            </Button>
          </header>
          <label htmlFor="skill-markdown" className="sr-only">
            Skill instructions
          </label>
          <textarea
            id="skill-markdown"
            rows={10}
            value={markdown}
            onChange={(e) => setMarkdown(e.target.value)}
          />
          {markdown && (
            <div className="automation-preview">
              <Markdown text={markdown} />
            </div>
          )}
        </section>
      )}
    </>
  );
}

function TemplatesPanel() {
  const templates = useQuery({
    queryKey: ["templates"],
    queryFn: () => automation("templates.list", {}),
  });
  const items = templates.data?.items || [];
  return (
    <>
      <div className="section-toolbar">
        <span className="muted">
          Reusable definitions. Exported templates never carry credentials or
          execution grants.
        </span>
      </div>
      {templates.isError && <Notice>{templates.error.message}</Notice>}
      {templates.isPending ? (
        <p role="status">Loading templates…</p>
      ) : !items.length ? (
        <Empty icon={FileText} title="No templates yet">
          Templates are portable definitions of a profile, routine, flow or
          skill. Imports always arrive as disabled drafts.
        </Empty>
      ) : (
        <div className="automation-card-list">
          {items.map((template) => (
            <section className="automation-card" key={template.id || template.name}>
              <header className="automation-card-head">
                <div>
                  <strong>{template.name}</strong>
                  <small className="muted">
                    {template.kind ? "Kind " + template.kind : "Template"}
                  </small>
                </div>
                <Layers3 size={15} />
              </header>
            </section>
          ))}
        </div>
      )}
    </>
  );
}
