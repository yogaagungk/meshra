import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, ListOrdered, Pause, Play, Route, X, Zap } from "lucide-react";
import { api } from "./api";
import { automation } from "./automation-api";
import { Button } from "./components/ui/button";
import { handle } from "./routing";
import { writeRouting, type RoutingSettings } from "./leadRouting";
import type { Agent, Task } from "./types";

/**
 * Lead routing settings. Only members whose harness can prove a tool-free turn
 * may lead, because the lead reads untrusted message text to pick recipients.
 */
export function RoutingControl({
  conversationId,
  members,
  value,
  onChange,
}: {
  conversationId: string;
  members: Agent[];
  value: RoutingSettings;
  onChange: (next: RoutingSettings) => void;
}) {
  const eligible = members.filter((m) => m.capabilities?.toolFreeDraft);
  function update(next: RoutingSettings) {
    writeRouting(conversationId, next);
    onChange(next);
  }
  if (members.length < 2) return null;
  return (
    <details className="routing-control">
      <summary title="Let a lead agent choose recipients when you don't address anyone">
        <Route size={13} />
        {value.lead ? "Lead: " + handle(members.find((m) => m.id === value.lead) || { id: value.lead, name: "" }) : "Auto-route"}
      </summary>
      <div className="routing-panel">
        <label htmlFor="routing-lead">Lead agent</label>
        <select
          id="routing-lead"
          value={eligible.some((m) => m.id === value.lead) ? value.lead : ""}
          onChange={(e) => update({ ...value, lead: e.target.value })}
        >
          <option value="">Off: I choose or @mention</option>
          {eligible.map((m) => (
            <option key={m.id} value={m.id}>
              {m.id}
            </option>
          ))}
        </select>
        {!eligible.length && (
          <p className="muted">
            No member can lead yet. A lead must run on a harness that supports verified tool-free turns
            (Claude, OMP or an OpenAI-compatible endpoint).
          </p>
        )}
        <label htmlFor="routing-purpose">What is this chat for? (helps the lead choose)</label>
        <textarea
          id="routing-purpose"
          rows={2}
          maxLength={1024}
          value={value.purpose}
          onChange={(e) => update({ ...value, purpose: e.target.value })}
          placeholder="e.g. Ship the billing app: planner scopes, builder codes, reviewer checks."
        />
        <p className="muted">
          The lead reads only your message and the member list, in a disposable turn with no tools. It can
          only narrow a send to members of this chat. @mentions and ticked recipients always win. Saved in this browser.
        </p>
      </div>
    </details>
  );
}

/** Queued work for this conversation, with pause, run-next and cancel. */
export function ChatQueue({ conversationId, members }: { conversationId: string; members: Agent[] }) {
  const client = useQueryClient();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const ids = new Set(members.map((m) => m.id));
  const queues = useQuery({
    queryKey: ["queue", "chat", conversationId],
    queryFn: () => automation("queue.list", { includeChat: true }),
    refetchInterval: 4000,
    refetchIntervalInBackground: false,
  });
  const states = (queues.data?.items || []).filter(
    (s) => ids.has(s.agentId) && s.tasks.some((t) => t.conversationId === conversationId),
  );
  if (!states.length) return null;
  const total = states.reduce((n, s) => n + s.tasks.filter((t) => t.conversationId === conversationId).length, 0);

  async function act(key: string, work: () => Promise<unknown>) {
    setBusy(key);
    setError("");
    try {
      await work();
      await client.invalidateQueries({ queryKey: ["queue"] });
      await client.invalidateQueries({ queryKey: ["timeline", conversationId] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  return (
    <section className="chat-queue" aria-label="Queued messages">
      <button className="chat-queue-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <ChevronRight size={14} className={open ? "open" : ""} />
        <ListOrdered size={14} />
        {total} queued
        {states.some((s) => s.paused) && <span className="chat-queue-paused">paused</span>}
      </button>
      {open && (
        <div className="chat-queue-body">
          {error && <p role="alert" className="text-error">{error}</p>}
          {states.map((state) => {
            const mine = state.tasks.filter((t) => t.conversationId === conversationId);
            return (
              <div key={state.agentId} className="chat-queue-agent">
                <div className="chat-queue-agent-head">
                  <code>{state.agentId}</code>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={!!busy}
                    onClick={() =>
                      void act(state.agentId, () =>
                        automation(state.paused ? "queue.resume" : "queue.pause", {
                          agentId: state.agentId,
                          expectedRevision: state.revision,
                        }),
                      )
                    }
                  >
                    {state.paused ? <Play size={12} /> : <Pause size={12} />}
                    {state.paused ? "Resume" : "Pause"}
                  </Button>
                </div>
                <ol>
                  {mine.map((task: Task, index) => (
                    <li key={task.taskId}>
                      <span className="chat-queue-text" title={task.text}>
                        {task.text}
                      </span>
                      {index > 0 && (
                        <button
                          disabled={!!busy}
                          title="Run this before the others"
                          onClick={() =>
                            void act(task.taskId, () =>
                              automation("queue.reorder", {
                                agentId: state.agentId,
                                expectedRevision: state.revision,
                                taskIds: [task.taskId, ...state.tasks.map((t) => t.taskId).filter((x) => x !== task.taskId)],
                              }),
                            )
                          }
                        >
                          <Zap size={12} /> Run next
                        </button>
                      )}
                      <button
                        disabled={!!busy}
                        aria-label="Cancel queued message"
                        onClick={() => void act(task.taskId, () => api("tasks/" + encodeURIComponent(task.taskId) + "/cancel", {}))}
                      >
                        <X size={12} /> Cancel
                      </button>
                    </li>
                  ))}
                </ol>
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

/**
 * Retrying elsewhere is a human decision: a failed turn may already have had
 * effects, so nothing is re-sent automatically.
 */
export function RetryWith({
  task,
  members,
  onSent,
}: {
  task: Task;
  members: Agent[];
  onSent: () => Promise<void>;
}) {
  const others = members.filter((m) => m.id !== task.to);
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  if (!others.length || !["failed", "timed_out"].includes(task.state) || !task.conversationId) return null;
  const chosen = target || others[0].id;
  async function send() {
    setBusy(true);
    setError("");
    try {
      const result = await api<{ errors: Record<string, string> }>("send", {
        conversationId: task.conversationId,
        text: task.text,
        recipients: [chosen],
        contextRefs: task.contextRefs || [],
        idempotencyKey: crypto.randomUUID(),
      });
      const failure = Object.values(result.errors)[0];
      if (failure) throw new Error(failure);
      await onSent();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="retry-with">
      <label htmlFor={"retry-" + task.taskId}>Try with another agent</label>
      <select id={"retry-" + task.taskId} value={chosen} onChange={(e) => setTarget(e.target.value)} disabled={busy}>
        {others.map((m) => (
          <option key={m.id} value={m.id}>
            {m.id}
          </option>
        ))}
      </select>
      <Button size="sm" variant="outline" disabled={busy} onClick={() => void send()}>
        {busy ? "Sending…" : "Send instead"}
      </Button>
      {task.ambiguous && (
        <small className="muted">The first attempt may have had effects. Review before resending.</small>
      )}
      {error && <small role="alert" className="text-error">{error}</small>}
    </div>
  );
}
