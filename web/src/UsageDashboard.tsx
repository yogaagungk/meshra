import { useQuery } from "@tanstack/react-query";
import { Activity, Coins, Database, RefreshCw } from "lucide-react";
import { automation, type RunUsage } from "./automation-api";
import { Notice } from "./App";
import { Button } from "./components/ui/button";
import { harnessName } from "./HarnessLogo";
import "./usage-dashboard.css";

function tokens(items: RunUsage[], field: "inputTokens" | "outputTokens" | "cachedInputTokens") {
  const reported = items.filter((item) => item[field] != null);
  return { count: reported.length, total: reported.reduce((sum, item) => sum + item[field]!, 0) };
}

function amount(value: number, currency: string | null) {
  return `${value.toLocaleString(undefined, { maximumFractionDigits: 8 })} ${currency || "currency not reported"}`;
}

export function UsageDashboard() {
  const usage = useQuery({
    queryKey: ["automation", "usage-dashboard"],
    queryFn: () => automation("usage.list", { limit: 100 }),
    refetchInterval: 30000,
    gcTime: 0,
  });
  const items = usage.data?.items || [];
  const input = tokens(items, "inputTokens");
  const output = tokens(items, "outputTokens");
  const cached = tokens(items, "cachedInputTokens");
  const costs = items.filter((item) => item.reportedCost != null);
  const currencies = new Map<string, number>();
  for (const item of costs) {
    if (item.currency) currencies.set(item.currency, (currencies.get(item.currency) || 0) + item.reportedCost!);
  }
  const unknownCurrency = costs.filter((item) => !item.currency).length;
  return <section className="usage-dashboard">
    <div className="personal-section-heading"><div><h2>Usage &amp; cost</h2><p>Recent recorded AI attempts</p></div>
      <Button variant="outline" size="sm" disabled={usage.isFetching} onClick={() => void usage.refetch()}><RefreshCw size={14} /> Refresh</Button>
    </div>
    {usage.isPending ? <p role="status">Loading usage…</p> : usage.isError ? <Notice>{usage.error.message}</Notice> : <>
      <p>Showing {items.length} most recent attempts{usage.data.hasMore ? " (more records exist)" : ""}, retained for {usage.data.retentionDays} days. Totals below cover only these displayed attempts, including failed and regenerated turns.</p>
      <div className="usage-dashboard-stats">
        <div><Activity size={18} /><span>Attempts</span><strong>{items.length.toLocaleString()}</strong><small>In this view</small></div>
        <div><Database size={18} /><span>Input tokens</span><strong>{input.count ? input.total.toLocaleString() : "Not reported"}</strong><small>{input.count} of {items.length} reported</small></div>
        <div><Database size={18} /><span>Output tokens</span><strong>{output.count ? output.total.toLocaleString() : "Not reported"}</strong><small>{output.count} of {items.length} reported</small></div>
        <div><Coins size={18} /><span>Reported cost</span><strong>{costs.length ? [...Array.from(currencies, ([currency, value]) => amount(value, currency)), ...(unknownCurrency ? [`${unknownCurrency} without currency`] : [])].join(" · ") : "Not reported"}</strong><small>{costs.length} of {items.length} reported</small></div>
      </div>
      <p>Cached input: {cached.count ? cached.total.toLocaleString() : "Not reported"} ({cached.count} of {items.length} attempts reported). Cached tokens may be included in input tokens; do not add them together.</p>
      <p>Costs are shown only when reported by the runtime; missing values are unknown, not zero. Totals may be incomplete and are not an invoice. Meshra does not estimate prices or convert currencies. Provider subscriptions and other charges may not be represented.</p>
      {!items.length ? <p>No usage recorded yet.</p> : <div className="personal-table-scroll"><table className="personal-usage-table usage-dashboard-table">
        <caption>Recent attempts and runtime-reported usage</caption><thead><tr><th scope="col">Started</th><th scope="col">Model</th><th scope="col">Input / output</th><th scope="col">Reported cost</th><th scope="col">Status</th></tr></thead>
        <tbody>{items.map((item) => <tr key={`${item.taskId}/${item.attempt}`}>
          <td>{new Date(item.startedAt).toLocaleString()}<small>Attempt {item.attempt}</small></td>
          <th scope="row">{item.reportedModel || item.configuredModel || "Native default"}<small>{harnessName(item.harness)}{(item.reportedProvider || item.configuredProvider) ? ` · ${item.reportedProvider || item.configuredProvider}` : ""}{!item.reportedModel ? " · configured" : " · reported"}</small></th>
          <td>{item.inputTokens == null ? "Unknown" : item.inputTokens.toLocaleString()} / {item.outputTokens == null ? "Unknown" : item.outputTokens.toLocaleString()}<small>Cached: {item.cachedInputTokens == null ? "Unknown" : item.cachedInputTokens.toLocaleString()}</small></td>
          <td>{item.reportedCost == null ? "Not reported" : amount(item.reportedCost, item.currency)}</td><td>{item.state}{item.stale && <small>Previous binding</small>}</td>
        </tr>)}</tbody>
      </table></div>}
    </>}
  </section>;
}
