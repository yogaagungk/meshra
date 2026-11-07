import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { History } from "lucide-react";
import { automation, type Effect, type EffectStatus } from "./automation-api";
import { Empty, Notice } from "./App";
import { relativeTime } from "./automationShared";

const STATUS: Record<EffectStatus, { label: string; tone: "ok" | "bad" | "warn" | "idle" }> = {
  intended: { label: "Recorded, not sent", tone: "idle" },
  dispatched: { label: "Sending", tone: "warn" },
  completed: { label: "Completed", tone: "ok" },
  rejected: { label: "Not executed", tone: "bad" },
  cancelled: { label: "Cancelled", tone: "idle" },
  uncertain: { label: "Outcome unknown", tone: "warn" },
};
const FILTERS: (EffectStatus | "")[] = ["", "uncertain", "dispatched", "intended", "completed", "rejected", "cancelled"];

/**
 * External actions and their real outcomes, independent of task status. This
 * view only reads. An unknown outcome is never shown as success or failure, and
 * nothing here resends: duplicate risk is the operator's decision.
 */
export function EffectsPanel() {
  const [status, setStatus] = useState<EffectStatus | "">("");
  const effects = useQuery({
    queryKey: ["automation", "effects", status],
    queryFn: () => automation("effects.list", { limit: 100, ...(status ? { status } : {}) }),
    refetchInterval: 8000,
    refetchIntervalInBackground: false,
  });
  const items = effects.data?.effects || [];
  const unknown = items.filter((e) => e.duplicateRisk).length;
  return (
    <section className="effects-panel" aria-label="External effects">
      <p className="effects-intro">
        Actions Meshra took outside itself: tool changes and messages sent through connectors. Status is the
        provider outcome we recorded, not the task state.
      </p>
      {unknown > 0 && !status ? (
        <Notice>
          {unknown} {unknown === 1 ? "action has" : "actions have"} an unknown outcome. It may already have happened.
          Check the destination before sending it again.
        </Notice>
      ) : null}
      <label className="effects-filter">
        Show
        <select value={status} onChange={(e) => setStatus(e.target.value as EffectStatus | "")}>
          {FILTERS.map((f) => (
            <option key={f} value={f}>
              {f ? STATUS[f].label : "All outcomes"}
            </option>
          ))}
        </select>
      </label>
      {effects.isError ? <Notice>Could not load effects.</Notice> : null}
      {effects.isSuccess && items.length === 0 ? (
        <Empty icon={History} title="No external actions">Tool changes and connector messages appear here once Meshra performs them.</Empty>
      ) : null}
      <ul className="effects-list">
        {items.map((e) => (
          <EffectRow key={e.source + e.id} effect={e} />
        ))}
      </ul>
    </section>
  );
}

function EffectRow({ effect: e }: { effect: Effect }) {
  const s = STATUS[e.status] || { label: e.status, tone: "idle" as const };
  return (
    <li className="effect-row">
      <div className="effect-head">
        <span className={"run-badge run-" + s.tone}>{s.label}</span>
        <strong>{e.operation}</strong>
        <span className="effect-source">{e.source === "tool" ? "Tool" : "Connector"}</span>
        <time dateTime={e.createdAt}>{relativeTime(e.createdAt)}</time>
      </div>
      {e.duplicateRisk ? (
        <p className="effect-risk">Sending this again may duplicate it. Meshra never retries it automatically.</p>
      ) : null}
      {e.error ? <p className="effect-error">{e.error}</p> : null}
      <details>
        <summary>Details</summary>
        <dl className="effect-details">
          <dt>Task</dt>
          <dd>{e.taskId || "none"}</dd>
          {e.providerRef ? (
            <>
              <dt>Provider reference</dt>
              <dd>{e.providerRef}</dd>
            </>
          ) : null}
          {e.requestDigest ? (
            <>
              <dt>Request digest</dt>
              <dd>{e.requestDigest}</dd>
            </>
          ) : null}
          {e.policyRevision ? (
            <>
              <dt>Policy revision</dt>
              <dd>{e.policyRevision}</dd>
            </>
          ) : null}
          <dt>Duplicate protection</dt>
          <dd>{e.idempotency}</dd>
        </dl>
      </details>
    </li>
  );
}
