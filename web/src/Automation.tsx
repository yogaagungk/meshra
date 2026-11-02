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
  X,
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
import { api, collab, localRead, localWrite } from "./api";
import type { Agent, Conversation } from "./types";
import { Approvals } from "./Approvals";
import { SkillsPanel } from "./AutomationSkills";
import { FlowsPanel } from "./AutomationFlows";
import { RunBadge, RunsPanel, useRecentRuns } from "./AutomationRuns";
import { goAutomation, relativeTime, skipReasons, useProfiles } from "./automationShared";
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
 * Automation is the durable surface: queued work, scheduled routines and their
 * runs, flow graphs, reviewed skills and reusable templates.
 */
const TABS = [
  ["queue", "Queue"],
  ["routines", "Routines"],
  ["runs", "Runs"],
  ["flows", "Flows"],
  ["skills", "Skills"],
  ["templates", "Templates"],
] as const;

export function Automation({ tab }: { tab?: string }) {
  const current = TABS.some(([key]) => key === tab) ? (tab as string) : "queue";
  const item = decodeURIComponent(location.hash.slice(1).split("/")[2] || "");
  const queue = useQuery({ queryKey: ["queue"], queryFn: () => automation("queue.list", {}), refetchInterval: 5000, refetchIntervalInBackground: false });
  const routines = useQuery({ queryKey: ["routines"], queryFn: () => automation("routines.list", { limit: 50 }) });
  const flows = useQuery({ queryKey: ["flows"], queryFn: () => automation("flows.list", { limit: 100 }) });
  const skills = useQuery({ queryKey: ["skills"], queryFn: () => automation("skills.list", { limit: 100 }) });
  const counts: Record<string, number | undefined> = {
    queue: queue.data?.items.reduce((n, s) => n + s.tasks.length, 0),
    routines: routines.data?.items?.length,
    flows: flows.data?.items.length,
    skills: skills.data?.items.length,
  };
  return (
    <div className="automation-page">
      <Approvals />
      <div className="automation-tabs" role="group" aria-label="Automation sections">
        {TABS.map(([key, label]) => (
          <button key={key} aria-pressed={current === key} onClick={() => goAutomation(key)}>
            {label}
            {!!counts[key] && <span className="tab-count">{counts[key]}</span>}
          </button>
        ))}
      </div>
      {current === "queue" ? (
        <QueuePanel />
      ) : current === "routines" ? (
        <RoutinesPanel />
      ) : current === "runs" ? (
        <RunsPanel />
      ) : current === "flows" ? (
        <FlowsPanel focus={item || undefined} />
      ) : current === "skills" ? (
        <SkillsPanel focus={item || undefined} />
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
  const queues = useQuery({
    queryKey: ["queue"],
    queryFn: () => automation("queue.list", {}),
    refetchInterval: 5000,
    refetchIntervalInBackground: false,
  });
  const all = queues.data?.items || [];
  const items = all.filter((s) => s.tasks.length > 0 || s.paused);
  const queued = items.reduce((n, s) => n + s.tasks.length, 0);
  const idle = all.length - items.length;

  async function act(key: string, work: () => Promise<unknown>) {
    setBusy(key);
    setError("");
    try {
      await work();
      await client.invalidateQueries({ queryKey: ["queue"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }
  const control = (state: QueueState, operation: "queue.pause" | "queue.resume") =>
    act(state.agentId, () => automation(operation, { agentId: state.agentId, expectedRevision: state.revision }));
  const reorder = (state: QueueState, taskIds: string[]) =>
    act(state.agentId, () => automation("queue.reorder", { agentId: state.agentId, expectedRevision: state.revision, taskIds }));
  function moved(state: QueueState, index: number, to: number) {
    const ids = state.tasks.map((t) => t.taskId);
    const [id] = ids.splice(index, 1);
    ids.splice(to, 0, id);
    return ids;
  }

  return (
    <>
      <div className="section-toolbar">
        <span className="muted" role="status">
          {queued
            ? `${queued} message${queued === 1 ? "" : "s"} waiting across ${items.length} agent${items.length === 1 ? "" : "s"}. Changes apply immediately.`
            : "Messages wait here when an agent is busy, offline or paused."}
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
          When you message an agent that is busy or stopped, the message waits here. You can pause an agent, send a message first, or cancel it.
        </Empty>
      ) : (
        <div className="automation-queue-list">
          {items.map((state) => (
            <section className="automation-card" key={state.agentId} aria-label={"Queue for " + state.agentId}>
              <header className="automation-card-head">
                <div>
                  <strong>{state.agentId}</strong>
                  <small className={"queue-state " + (state.paused ? "is-paused" : "")}>
                    {state.paused
                      ? "Paused: nothing is sent to this agent until you resume."
                      : `${state.tasks.length} waiting, sent one at a time.`}
                  </small>
                </div>
                <div className="automation-actions">
                  {state.profileId && state.tasks[0]?.conversationId && (
                    <BindingControls profileId={state.profileId} conversationId={state.tasks[0].conversationId} agentId={state.agentId} />
                  )}
                  {state.paused ? (
                    <Button variant="positive" size="sm" disabled={busy === state.agentId} onClick={() => void control(state, "queue.resume")}>
                      <Play size={13} />
                      Resume
                    </Button>
                  ) : (
                    <Button variant="outline" size="sm" disabled={busy === state.agentId} onClick={() => void control(state, "queue.pause")}>
                      <Pause size={13} />
                      Pause
                    </Button>
                  )}
                </div>
              </header>
              <ol className="automation-task-list">
                {state.tasks.map((task, index) => {
                  const snippet = task.text.length > 40 ? task.text.slice(0, 40) + "…" : task.text;
                  return (
                    <li key={task.taskId}>
                      <span className="queue-position" aria-hidden="true">
                        {index + 1}
                      </span>
                      <div className="automation-task-body">
                        <span className="automation-task-text">{task.text}</span>
                        <small className="muted">
                          {index === 0 && !state.paused ? "Next to send" : "Waiting"} · added {relativeTime(task.createdAt)}
                          {task.conversationId && (
                            <>
                              {" · "}
                              <a href={"#chat/" + task.conversationId}>Open conversation</a>
                            </>
                          )}
                        </small>
                      </div>
                      <div className="automation-actions">
                        {index > 0 && (
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={busy === state.agentId}
                            aria-label={`Send “${snippet}” next`}
                            onClick={() => void reorder(state, moved(state, index, 0))}
                          >
                            <ArrowUp size={13} />
                            Send next
                          </Button>
                        )}
                        {index < state.tasks.length - 1 && (
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={busy === state.agentId}
                            aria-label={`Move “${snippet}” later`}
                            onClick={() => void reorder(state, moved(state, index, index + 1))}
                          >
                            <ArrowDown size={13} />
                            Later
                          </Button>
                        )}
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={busy === task.taskId}
                          aria-label={`Cancel “${snippet}”`}
                          onClick={() => void act(task.taskId, () => api("tasks/" + encodeURIComponent(task.taskId) + "/cancel", {}))}
                        >
                          <X size={13} />
                          Cancel
                        </Button>
                      </div>
                    </li>
                  );
                })}
              </ol>
            </section>
          ))}
          {idle > 0 && <p className="muted">{idle} other agent{idle === 1 ? " has" : "s have"} nothing queued.</p>}
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
  const [prefill, setPrefill] = useState<RoutinePrefill | undefined>();
  useEffect(() => {
    // "Schedule this" in a profile chat hands its message over here.
    const handed = localRead<RoutinePrefill | null>("routine-prefill", null);
    if (handed && typeof handed === "object" && handed.instruction) {
      setPrefill(handed);
      setCreating(true);
    }
    localWrite("routine-prefill", null);
  }, []);
  const routines = useQuery({
    queryKey: ["routines"],
    queryFn: () => automation("routines.list", { limit: 50 }),
  });
  const items = routines.data?.items || [];
  const profiles = useProfiles();
  const flows = useQuery({ queryKey: ["flows"], queryFn: () => automation("flows.list", { limit: 100 }) });
  const runs = useRecentRuns(items, 3);
  const lastRun = (id?: string) => runs.data?.find((r) => r.routineId === id);

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
                    variant={routine.enabled ? "warning" : "positive"}
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
                <span>Runs {profiles.name(routine.profileId)}</span>
                {routine.flowId && (
                  <button className="link-button" onClick={() => goAutomation("flows", routine.flowId)}>
                    Flow: {flows.data?.items.find((f) => f.id === routine.flowId)?.name || "Untitled flow"}
                  </button>
                )}
                <span>
                  {routine.autoStart
                    ? "Starts the agent if it is stopped"
                    : "Skips when the agent is stopped"}
                </span>
                {routine.trigger?.kind === "webhook" && routine.webhookSecretStatus && (
                  <span>Webhook secret {routine.webhookSecretStatus}</span>
                )}
              </div>
              {(() => {
                const last = lastRun(routine.id);
                return (
                  <p className="routine-last-run">
                    {last ? (
                      <>
                        Last run <RunBadge state={last.state} /> {relativeTime(last.scheduledAt || last.createdAt)}
                        {last.reason && <span className="muted"> · {skipReasons[last.reason] || last.reason}</span>}{" "}
                      </>
                    ) : (
                      <span className="muted">Has not run yet. </span>
                    )}
                    <button className="link-button" onClick={() => goAutomation("runs")}>
                      All runs
                    </button>
                  </p>
                );
              })()}
            </section>
          ))}
        </div>
      )}
      <RoutineDialog open={creating} initial={prefill} onClose={() => { setCreating(false); setPrefill(undefined); }} />
    </>
  );
}

export type RoutinePrefill = { name?: string; profileId?: string; instruction?: string };

function RoutineDialog({
  open,
  onClose,
  initial,
}: {
  open: boolean;
  onClose: () => void;
  initial?: RoutinePrefill;
}) {
  const client = useQueryClient();
  const [name, setName] = useState(initial?.name || "");
  const [profileId, setProfileId] = useState(initial?.profileId || "");
  const [instruction, setInstruction] = useState(initial?.instruction || "");
  useEffect(() => {
    if (open && initial) {
      setName(initial.name || "");
      setProfileId(initial.profileId || "");
      setInstruction(initial.instruction || "");
    }
  }, [open, initial]);
  const [kind, setKind] = useState<RoutineTrigger["kind"]>("interval");
  const [everySeconds, setEverySeconds] = useState("3600");
  const [expression, setExpression] = useState("");
  const [autoStart, setAutoStart] = useState(false);
  const [enabled, setEnabled] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [secret, setSecret] = useState("");
  const [at, setAt] = useState("");
  const [timeZone, setTimeZone] = useState(() => Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC");
  const [flowId, setFlowId] = useState("");
  const profiles = useProfiles();
  const flows = useQuery({ queryKey: ["flows"], queryFn: () => automation("flows.list", { limit: 100 }), enabled: open });
  const activeProfiles = profiles.items.filter((p) => !p.archived);
  const needsDate = kind === "once";
  const ready = !!name.trim() && !!profileId && !!instruction.trim() && (!needsDate || !!at) && (kind !== "cron" || !!expression.trim());

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    const trigger: RoutineTrigger =
      kind === "cron"
        ? { kind: "cron", expression, timeZone }
        : kind === "interval"
          ? { kind: "interval", everySeconds: Number(everySeconds) }
          : kind === "once"
            ? { kind: "once", at: new Date(at).toISOString() }
            : { kind };
    try {
      const result = await automation("routines.save", {
        routine: {
          name: name.trim(),
          profileId,
          instruction,
          trigger,
          ...(flowId ? { flowId } : {}),
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
      description="Runs one profile with your instruction, on a schedule or when you trigger it. Every occurrence keeps a receipt in Runs."
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
        <label htmlFor="routine-profile">Who runs it</label>
        {profiles.isPending ? (
          <p role="status" className="muted">Loading profiles…</p>
        ) : !activeProfiles.length ? (
          <p className="policy-note">
            A routine needs a profile: a saved agent with instructions, a workspace and a provider.
            You have none yet. <a href="#agents" onClick={onClose}>Create one under Agents → Profiles</a>, then come back.
          </p>
        ) : (
          <select id="routine-profile" required value={profileId} onChange={(e) => setProfileId(e.target.value)}>
            <option value="">Choose a profile…</option>
            {activeProfiles.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
        )}
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
          <option value="webhook">When a webhook is called</option>
          <option value="manual">Manual only (Run now)</option>
        </select>
        {kind === "once" && (
          <>
            <label htmlFor="routine-at">Run at</label>
            <input
              id="routine-at"
              type="datetime-local"
              required
              value={at}
              min={new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 16)}
              onChange={(e) => setAt(e.target.value)}
            />
            <small className="muted">Your local time. It must be in the future.</small>
          </>
        )}
        {kind === "webhook" && (
          <small className="muted">A secret is generated when you create the routine and shown once. Calls must be signed with it.</small>
        )}
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
            <small className="muted">Minute, hour, day, month, weekday. Example: 0 9 * * 1-5 is 09:00 on weekdays.</small>
            <label htmlFor="routine-zone">Time zone</label>
            <input id="routine-zone" value={timeZone} onChange={(e) => setTimeZone(e.target.value)} />
          </>
        )}
        {!!flows.data?.items.length && (
          <>
            <label htmlFor="routine-flow">Run a flow (optional)</label>
            <select id="routine-flow" value={flowId} onChange={(e) => setFlowId(e.target.value)}>
              <option value="">No: just run the instruction above</option>
              {flows.data.items.map((f) => (
                <option key={f.id} value={f.id}>
                  {f.name || "Untitled flow"} ({f.nodes.length} steps)
                </option>
              ))}
            </select>
          </>
        )}
        <label className="automation-inline">
          <input
            type="checkbox"
            checked={autoStart}
            onChange={(e) => setAutoStart(e.target.checked)}
          />
          Start the agent automatically if it is stopped
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
          <Button type="submit" disabled={busy || !ready}>
            {busy ? "Saving…" : "Create routine"}
          </Button>
        </div>
      </form>
    </Dialog>
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
