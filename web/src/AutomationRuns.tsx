import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { History } from "lucide-react";
import { automation, type Routine, type RoutineRun } from "./automation-api";
import { Empty, Notice } from "./App";
import { Button } from "./components/ui/button";
import { FlowGraph } from "./FlowGraph";
import { relativeTime, runStateLabels, runTone, skipReasons, useProfiles } from "./automationShared";

export type RunWithRoutine = RoutineRun & { routineName: string };

/** The latest runs of every routine, newest first. Routines are few, so one request each is fine. */
export function useRecentRuns(routines: Routine[] | undefined, perRoutine = 8) {
  const ids = (routines || []).filter((r) => r.id).map((r) => r.id!);
  return useQuery({
    queryKey: ["runs", perRoutine, ids.join(",")],
    enabled: ids.length > 0,
    refetchInterval: 8000,
    refetchIntervalInBackground: false,
    queryFn: async (): Promise<RunWithRoutine[]> => {
      const pages = await Promise.all(
        ids.map(async (id) => {
          const page = await automation("routines.runs", { id, limit: perRoutine });
          const name = routines!.find((r) => r.id === id)?.name || "Routine";
          return (page.items || []).map((run) => ({ ...run, routineName: name }));
        }),
      );
      return pages.flat().sort((a, b) => b.createdAt.localeCompare(a.createdAt));
    },
  });
}

export function RunBadge({ state }: { state: string }) {
  return <span className={"run-badge run-" + runTone(state)}>{runStateLabels[state] || state}</span>;
}

export function RunsPanel() {
  const client = useQueryClient();
  const routines = useQuery({
    queryKey: ["routines"],
    queryFn: () => automation("routines.list", { limit: 50 }),
  });
  const runs = useRecentRuns(routines.data?.items);
  const [filter, setFilter] = useState("all");
  const [open, setOpen] = useState<string>("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const items = (runs.data || []).filter((r) => filter === "all" || r.state === filter || (filter === "problems" && ["failed", "skipped"].includes(r.state)));

  async function cancel(run: RoutineRun) {
    setBusy(run.runId);
    setError("");
    try {
      await automation("routines.cancelRun", { runId: run.runId });
      await client.invalidateQueries({ queryKey: ["runs"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  return (
    <>
      <div className="section-toolbar">
        <span className="muted">A receipt for every occurrence of every routine, including the ones that were skipped, and why.</span>
        <select aria-label="Filter runs" value={filter} onChange={(e) => setFilter(e.target.value)}>
          <option value="all">All runs</option>
          <option value="running">Running</option>
          <option value="problems">Failed or skipped</option>
          <option value="completed">Completed</option>
        </select>
      </div>
      {error && <Notice>{error}</Notice>}
      {routines.isPending || runs.isPending ? (
        routines.data?.items?.length === 0 ? (
          <NoRuns />
        ) : (
          <p role="status">Loading runs…</p>
        )
      ) : runs.isError ? (
        <Notice>{runs.error.message}</Notice>
      ) : !runs.data?.length ? (
        <NoRuns />
      ) : !items.length ? (
        <p className="muted">No runs match this filter.</p>
      ) : (
        <ol className="run-timeline">
          {items.map((run) => (
            <li key={run.runId}>
              <button className="run-row" aria-expanded={open === run.runId} onClick={() => setOpen(open === run.runId ? "" : run.runId)}>
                <RunBadge state={run.state} />
                <strong>{run.routineName}</strong>
                <span className="muted">
                  {run.source} · {relativeTime(run.scheduledAt || run.createdAt)}
                  {run.skippedCount ? ` · ${run.skippedCount} skipped before` : ""}
                </span>
              </button>
              {open === run.runId && (
                <RunDetail
                  run={run}
                  routine={routines.data?.items?.find((r) => r.id === run.routineId)}
                  busy={busy === run.runId}
                  onCancel={() => void cancel(run)}
                />
              )}
            </li>
          ))}
        </ol>
      )}
    </>
  );
}

function NoRuns() {
  return (
    <Empty icon={History} title="No runs yet">
      Runs appear here when a routine fires on its schedule or you press Run now.
    </Empty>
  );
}

function RunDetail({ run, routine, busy, onCancel }: { run: RunWithRoutine; routine?: Routine; busy: boolean; onCancel: () => void }) {
  const profiles = useProfiles();
  const steps = run.steps || [];
  const status = Object.fromEntries(steps.map((s) => [s.nodeId, s.state]));
  // The relay does not return a run's graph snapshot, so draw the flow the routine pins.
  const flow = useQuery({
    queryKey: ["flow", routine?.flowId, routine?.flowRevision],
    enabled: !!routine?.flowId && steps.length > 0,
    queryFn: () => automation("flows.get", { id: routine!.flowId!, revision: routine!.flowRevision }),
  });
  const matches = !!flow.data && steps.every((s) => flow.data!.nodes.some((n) => n.id === s.nodeId));
  const active = run.state === "running" || run.state === "claimed";
  return (
    <div className="run-detail">
      {run.reason && <p>{skipReasons[run.reason] || run.reason}</p>}
      {run.error && <p className="text-error">{run.error}</p>}
      {matches && flow.data ? (
        <FlowGraph
          nodes={flow.data.nodes}
          edges={flow.data.edges}
          status={status}
          label={(node, i) => `${i + 1}. ${profiles.name(node.profileId)}`}
          title={"Steps of this run"}
        />
      ) : steps.length > 0 ? (
        <ul className="run-steps" aria-label="Steps of this run">
          {steps.map((step) => (
            <li key={step.nodeId}>
              <RunBadge state={step.state || "claimed"} /> {step.nodeId}
              {step.reason && <span className="muted"> · {skipReasons[step.reason] || step.reason}</span>}
            </li>
          ))}
        </ul>
      ) : null}
      <div className="run-detail-meta">
        <small className="muted">Run {run.runId.slice(0, 10)} · routine revision {run.revision}</small>
        {run.conversationId && (
          <a href={"#chat/" + run.conversationId}>Open the conversation</a>
        )}
        {active && (
          <Button variant="outline" size="sm" disabled={busy} onClick={onCancel}>
            {busy ? "Cancelling…" : "Cancel run"}
          </Button>
        )}
      </div>
    </div>
  );
}
