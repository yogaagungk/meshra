import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertCircle, Gauge, RefreshCw } from "lucide-react";
import { api, APIError } from "./api";
import { Notice } from "./App";
import { harnessName } from "./HarnessLogo";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";

type Window = {
  id: string;
  provider: string;
  account?: string;
  label: string;
  usedPercent?: number;
  remaining?: number;
  unit?: string;
  resetsAt?: string;
  observedAt?: string;
};
type Quota = {
  harness: string;
  source: string;
  state: string;
  windows: Window[];
  checkedAt?: string;
  attemptedAt?: string;
  stale: boolean;
  message?: string;
};
type Status = {
  latest: {
    taskId: string;
    attempt: number;
    state: string;
    harness: string;
    model: string;
    inputTokens: number | null;
    outputTokens: number | null;
    cachedInputTokens: number | null;
    contextUsedTokens: number | null;
    contextWindowTokens: number | null;
    estimated: boolean | null;
    contextSource: string | null;
    observedAt: string | null;
  };
  localBudget: { turns: number; limit: number; resetsAt: string };
  contextNote: string;
};
type History = {
  items: {
    day: string;
    harness: string;
    model: string;
    turns: number;
    inputTokens: number;
    outputTokens: number;
    cachedInputTokens: number;
    reportedTurns: number;
    cachedReportedTurns: number;
  }[];
  hasMore: boolean;
};
function useStatus(id: string) {
  return useQuery({
    queryKey: ["ai-status", id],
    queryFn: () =>
      api<Status>(
        "ai-status" + (id ? "?conversationId=" + encodeURIComponent(id) : ""),
      ),
    refetchInterval: 10000,
    refetchIntervalInBackground: false,
    gcTime: 0,
  });
}
function useQuota(harness: string) {
  return useQuery({
    queryKey: ["quota", harness],
    queryFn: () => api<Quota>("quota", { harness }),
    retry: (count, e) => count < 3 && e instanceof APIError && e.status === 409,
    retryDelay: 1000,
    staleTime: 60000,
    refetchInterval: 60000,
    refetchIntervalInBackground: false,
    gcTime: 0,
  });
}
function number(value: number | null | undefined) {
  return value == null ? "Not reported" : value.toLocaleString();
}
function time(value?: string | null) {
  return value ? new Date(value).toLocaleString() : "Not yet reported";
}
function countdown(value: string | undefined, current: number) {
  if (!value) return "Reset time not reported";
  const minutes = Math.ceil((new Date(value).getTime() - current) / 60000);
  if (minutes <= 0) return "Reset time passed · refresh for current allowance";
  const days = Math.floor(minutes / 1440),
    hours = Math.floor((minutes % 1440) / 60),
    mins = minutes % 60;
  return (
    "Resets in " +
    [days ? days + "d" : "", hours ? hours + "h" : "", mins ? mins + "m" : ""]
      .filter(Boolean)
      .join(" ")
  );
}
function contextPercent(status?: Status) {
  const v = status?.latest;
  return v?.contextUsedTokens != null && v.contextWindowTokens
    ? (v.contextUsedTokens / v.contextWindowTokens) * 100
    : undefined;
}
export function ChatAIStatus({
  conversationId,
  harness,
  revision,
}: {
  conversationId: string;
  harness: string;
  revision: string;
}) {
  const [open, setOpen] = useState(false);
  const client = useQueryClient();
  const status = useStatus(conversationId);
  const quota = useQuota(harness);
  useEffect(() => {
    void client.invalidateQueries({ queryKey: ["ai-status", conversationId] });
  }, [client, conversationId, revision]);
  const context = contextPercent(status.data);
  const reported =
    quota.data?.windows
      .map((w) => w.usedPercent)
      .filter((v): v is number => v != null) || [];
  const used = reported.length ? Math.max(...reported) : undefined;
  const warning =
    (used != null && used >= 80) || (context != null && context >= 80);
  return (
    <>
      <button
        type="button"
        className={"chat-ai-status" + (warning ? " ai-status-warning" : "")}
        onClick={() => setOpen(true)}
        aria-label="Open AI status"
      >
        <span>
          <Gauge size={14} />
          AI status
        </span>
        <span>
          {used == null
            ? quota.isFetching
              ? "Checking quota…"
              : "Quota not reported"
            : (quota.data?.stale ? "Last highest window " : "Highest window ") +
              used.toFixed(0) +
              "% used"}
        </span>
        <span>
          {context == null
            ? "Context not reported"
            : (status.data?.latest.estimated ? "Context ≈ " : "Context ") +
              context.toFixed(0) +
              "%"}
        </span>
      </button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="AI status"
        description="Provider quota, the last context snapshot, and this conversation’s usage."
        wide
      >
        {open && (
          <AIStatusPanel
            conversationId={conversationId}
            initialHarness={harness}
          />
        )}
      </Dialog>
    </>
  );
}
export function AIStatusPanel({
  conversationId = "",
  initialHarness = "codex",
}: {
  conversationId?: string;
  initialHarness?: string;
}) {
  const [harness, setHarness] = useState(initialHarness);
  const [current, setCurrent] = useState(Date.now());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const client = useQueryClient();
  const status = useStatus(conversationId);
  const quota = useQuota(harness);
  const history = useQuery({
    queryKey: ["usage-history", conversationId, harness],
    queryFn: () =>
      api<History>(
        "usage-history?harness=" +
          encodeURIComponent(harness) +
          (conversationId
            ? "&conversationId=" + encodeURIComponent(conversationId)
            : ""),
      ),
    refetchInterval: 30000,
    refetchIntervalInBackground: false,
    gcTime: 0,
  });
  useEffect(() => {
    const timer = setInterval(() => setCurrent(Date.now()), 30000);
    return () => clearInterval(timer);
  }, []);
  async function refresh() {
    setBusy(true);
    setError("");
    try {
      const value = await api<Quota>("quota", { harness, refresh: true });
      client.setQueryData(["quota", harness], value);
      await status.refetch();
      await history.refetch();
      setCurrent(Date.now());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  const context = contextPercent(status.data),
    latest = status.data?.latest,
    budget = status.data?.localBudget;
  return (
    <div className="ai-status-panel">
      <section>
        <div className="personal-section-heading">
          <h2>Provider quota</h2>
          <Button
            variant="outline"
            size="sm"
            disabled={busy || quota.isFetching}
            onClick={() => void refresh()}
          >
            <RefreshCw size={14} />
            {busy || quota.isFetching ? "Refreshing…" : "Refresh AI status"}
          </Button>
        </div>
        <p>
          Account allowance reported by the native harness. Windows can cover
          different models or accounts, and may include activity outside Meshra.
        </p>
        <label className="personal-harness-label">
          Status harness
          <select
            value={harness}
            onChange={(e) => {
              setHarness(e.target.value);
              setError("");
            }}
          >
            {["codex", "omp", "claude", "opencode"].map((h) => (
              <option value={h} key={h}>
                {harnessName(h)}
              </option>
            ))}
          </select>
        </label>
        {error && <Notice>{error}</Notice>}
        {quota.isError && <Notice>{quota.error.message}</Notice>}
        {quota.isPending && <p role="status">Reading native quota…</p>}
        {quota.data && (
          <>
            <div className="ai-status-source">
              <span>{quota.data.source}</span>
              <span>Last successful refresh: {time(quota.data.checkedAt)}</span>
            </div>
            {quota.data.message && (
              <p className="ai-status-unavailable" role="status">
                {quota.data.message}
                {quota.data.stale && " Showing the last successful report."}
              </p>
            )}
            {quota.data.stale && (
              <p className="ai-status-warning">
                This report is stale. Refresh before relying on the remaining
                allowance.
              </p>
            )}
            <ul className="ai-quota-list">
              {quota.data.windows.map((w) => (
                <li key={w.id}>
                  <div className="ai-quota-heading">
                    <div>
                      <strong>{w.label}</strong>
                      <small>
                        {w.provider}
                        {w.account ? " · " + w.account : ""}
                      </small>
                    </div>
                    <span>
                      {w.usedPercent == null
                        ? "Usage not reported"
                        : w.usedPercent.toFixed(1) + "% used"}
                    </span>
                  </div>
                  {w.usedPercent != null && (
                    <progress
                      aria-label={w.provider + " " + w.label + " quota used"}
                      value={Math.min(100, w.usedPercent)}
                      max={100}
                      className={w.usedPercent >= 80 ? "ai-quota-high" : ""}
                    />
                  )}
                  <div className="ai-quota-details">
                    <span>{countdown(w.resetsAt, current)}</span>
                    {w.resetsAt && (
                      <time dateTime={w.resetsAt}>{time(w.resetsAt)}</time>
                    )}
                    {w.remaining != null && (
                      <span>
                        Remaining: {number(w.remaining)} {w.unit}
                      </span>
                    )}
                  </div>
                  {w.observedAt && (
                    <small className="ai-status-observed">
                      Provider snapshot: {time(w.observedAt)}
                    </small>
                  )}
                  {w.usedPercent != null && w.usedPercent >= 80 && (
                    <p className="ai-status-warning">
                      <AlertCircle size={14} />
                      {w.usedPercent >= 100
                        ? "This reported allowance is exhausted."
                        : "This reported allowance is approaching its limit."}
                    </p>
                  )}
                </li>
              ))}
            </ul>
          </>
        )}
      </section>
      {budget && (
        <section>
          <h2>Meshra daily budget</h2>
          <p>
            This machine’s execution-count limit, separate from the provider’s
            quota. Failed attempts and regenerations count.
          </p>
          <dl className="ai-status-facts">
            <dt>Started turns today</dt>
            <dd>
              {number(budget.turns)}
              {budget.limit > 0
                ? " / " + number(budget.limit)
                : " · No local limit"}
            </dd>
            <dt>Local budget resets</dt>
            <dd>{time(budget.resetsAt)} (midnight UTC)</dd>
          </dl>
          {budget.limit > 0 && budget.turns >= budget.limit * 0.8 && (
            <p className="ai-status-warning">
              {budget.turns >= budget.limit
                ? "The local daily budget is exhausted. Queued work retains its original deadline."
                : "The local daily budget is approaching its limit."}
            </p>
          )}
        </section>
      )}
      {conversationId && (
        <section>
          <h2>Last turn context & usage</h2>
          <p>{status.data?.contextNote}</p>
          {status.isError && <Notice>{status.error.message}</Notice>}
          {latest?.taskId ? (
            <>
              <div className="ai-context-reading">
                <strong>
                  {context == null
                    ? "Context not reported"
                    : (latest.estimated ? "Estimated " : "Reported ") +
                      context.toFixed(1) +
                      "%"}
                </strong>
                {context != null && (
                  <progress
                    aria-label="Last turn context used"
                    value={Math.min(context, 100)}
                    max={100}
                  />
                )}
              </div>
              <dl className="ai-status-facts">
                <dt>Context tokens / capacity</dt>
                <dd>
                  {number(latest.contextUsedTokens)} /{" "}
                  {number(latest.contextWindowTokens)}
                </dd>
                <dt>Input tokens</dt>
                <dd>{number(latest.inputTokens)}</dd>
                <dt>Output tokens</dt>
                <dd>{number(latest.outputTokens)}</dd>
                <dt>Cached input (read/write)</dt>
                <dd>{number(latest.cachedInputTokens)}</dd>
                <dt>Source</dt>
                <dd>{latest.contextSource || "Not reported"}</dd>
                <dt>Snapshot time</dt>
                <dd>{time(latest.observedAt)}</dd>
              </dl>
              {context != null && context >= 80 && (
                <p className="ai-status-warning">
                  The last native context snapshot is approaching capacity. The
                  next turn’s context can differ after history selection or
                  native compaction.
                </p>
              )}
            </>
          ) : (
            <p>No AI turn has been recorded for this conversation yet.</p>
          )}
        </section>
      )}
      <section>
        <div className="personal-section-heading">
          <h2>
            {conversationId ? "Conversation usage history" : "Usage history"}
          </h2>
          <span className="muted">Last 30 days · UTC</span>
        </div>
        <p>
          Meshra-recorded usage for {harnessName(harness)}, grouped by day and
          selected model. Token totals are shown only for turns that reported
          them. Provider billing is not inferred.
        </p>
        {history.isError && <Notice>{history.error.message}</Notice>}
        {history.isPending && <p role="status">Loading usage history…</p>}
        {history.data &&
          (history.data.items.length ? (
            <div className="personal-table-scroll">
              <table className="personal-usage-table">
                <caption>
                  {conversationId
                    ? "Daily usage in this conversation"
                    : "Daily recorded AI usage"}
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Day / model</th>
                    <th scope="col">Turns</th>
                    <th scope="col">Input / output tokens</th>
                    <th scope="col">Cached input</th>
                  </tr>
                </thead>
                <tbody>
                  {history.data.items.map((v, i) => (
                    <tr key={v.day + v.harness + v.model + i}>
                      <th scope="row">
                        {v.day}
                        <small>{v.model || "Native default"}</small>
                      </th>
                      <td>
                        {v.turns}
                        <small>{v.reportedTurns} reported usage</small>
                      </td>
                      <td>
                        {v.reportedTurns
                          ? number(v.inputTokens) +
                            " / " +
                            number(v.outputTokens)
                          : "Not reported"}
                      </td>
                      <td>
                        {v.cachedReportedTurns
                          ? number(v.cachedInputTokens)
                          : "Not reported"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {history.data.hasMore && (
                <p>
                  Showing the latest 100 day/model groups. Open an individual
                  conversation to narrow the results.
                </p>
              )}
            </div>
          ) : (
            <p>No recorded usage for this selection.</p>
          ))}
        {harness === "opencode" && (
          <p>
            OpenCode token counts cover the returned assistant message and may
            omit intermediate model/tool steps.
          </p>
        )}
      </section>
    </div>
  );
}
