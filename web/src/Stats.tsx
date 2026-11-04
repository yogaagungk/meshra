import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { RefreshCw, Search } from "lucide-react";
import {
  automation,
  type StatsRequestItem,
  type StatsSummary,
  type StatsTotals,
} from "./automation-api";
import { Notice } from "./App";
import { TabBar, TabPanel } from "./components/Tabs";
import { Button } from "./components/ui/button";
import { harnessName } from "./HarnessLogo";
import "./stats.css";

type Tab = "overview" | "models" | "providers" | "requests";
const TABS: [Tab, string][] = [
  ["overview", "Overview"],
  ["models", "Models"],
  ["providers", "Providers"],
  ["requests", "Requests"],
];
const RANGES: [24 | 168 | 720, string][] = [
  [24, "24h"],
  [168, "7d"],
  [720, "30d"],
];

/* ---------- formatting ---------- */

const compact = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 });
const full = new Intl.NumberFormat();
const num = (n: number) => (n >= 10000 ? compact.format(n) : full.format(Math.round(n)));
const pct = (n: number, digits = 1) => `${(n * 100).toFixed(digits)}%`;
const ms = (n: number | null | undefined) =>
  n == null ? "—" : n >= 1000 ? `${(n / 1000).toFixed(1)}s` : `${Math.round(n)}ms`;
const money = (n: number | null | undefined) =>
  n == null ? "—" : n === 0 ? "$0" : n < 0.01 ? "<$0.01" : `$${n.toFixed(n < 10 ? 2 : 0)}`;
const rate = (n: number | null | undefined) => (n == null ? "—" : n.toFixed(1));
const tokensOf = (t: Pick<StatsTotals, "input" | "output" | "cacheRead" | "cacheWrite">) =>
  t.input + t.output + t.cacheRead + t.cacheWrite;
const cacheRate = (t: StatsTotals) => {
  const prompt = t.input + t.cacheRead + t.cacheWrite;
  return prompt ? t.cacheRead / prompt : 0;
};
const errorRate = (t: StatsTotals) => (t.requests ? t.failed / t.requests : 0);

function ago(ts: number) {
  const s = Math.max(0, Math.round((Date.now() - ts) / 1000));
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

/* ---------- stable categorical colors ---------- */

const SLOTS = 6;
const slotOf = new Map<string, number>();
/** A name keeps the slot it first received, so filters never repaint it. */
function slot(name: string) {
  if (!slotOf.has(name)) slotOf.set(name, slotOf.size % SLOTS);
  return slotOf.get(name)!;
}
const colorVar = (name: string) => `var(--s${slot(name) + 1})`;

/* ---------- small SVG pieces ---------- */

function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(600);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) => setWidth(Math.max(240, Math.floor(entry.contentRect.width))));
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  return [ref, width] as const;
}

function Sparkline({ values, color = "var(--s1)" }: { values: number[]; color?: string }) {
  const w = 240;
  const h = 34;
  const max = Math.max(...values, 0);
  if (values.length < 2 || max === 0) {
    return (
      <svg className="stats-spark" viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" aria-hidden="true">
        <line x1="0" x2={w} y1={h - 2} y2={h - 2} stroke="var(--border)" strokeWidth="1.5" />
      </svg>
    );
  }
  const step = w / (values.length - 1);
  const points = values.map((v, i) => `${(i * step).toFixed(1)},${(h - 3 - (v / max) * (h - 8)).toFixed(1)}`);
  return (
    <svg className="stats-spark" viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" aria-hidden="true">
      <polyline points={points.join(" ")} fill="none" stroke={color} strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

function niceMax(value: number) {
  if (value <= 0) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(value)));
  const f = value / exp;
  return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10) * exp;
}

type Stack = { key: string; label: string; color: string; values: number[] };

function StackedBars({
  times,
  stacks,
  bucketMs,
  format,
  height = 220,
  label,
  hourOfDay = false,
}: {
  times: number[];
  stacks: Stack[];
  bucketMs: number;
  format: (n: number) => string;
  height?: number;
  label: string;
  hourOfDay?: boolean;
}) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const left = 44;
  const bottom = 22;
  const top = 8;
  const plotW = width - left - 6;
  const plotH = height - bottom - top;
  const totals = times.map((_, i) => stacks.reduce((sum, s) => sum + (s.values[i] || 0), 0));
  const max = niceMax(Math.max(...totals, 0));
  const slotW = plotW / Math.max(times.length, 1);
  const barW = Math.max(2, Math.min(28, slotW - 2));
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * max);
  const every = Math.max(1, Math.ceil(times.length / Math.max(1, Math.floor(plotW / 54))));
  const day = bucketMs >= 86400000;
  const fmtTime = (t: number) =>
    hourOfDay
      ? String(Math.round(t / 3600000)).padStart(2, "0")
      : day
      ? new Date(t).toLocaleDateString(undefined, { month: "short", day: "numeric" })
      : new Date(t).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
  return (
    <div className="stats-chart" ref={ref}>
      <svg width={width} height={height} role="img" aria-label={label} onMouseLeave={() => setHover(null)}>
        {ticks.map((v) => {
          const y = top + plotH - (v / max) * plotH;
          return (
            <g key={v}>
              <line x1={left} x2={width - 6} y1={y} y2={y} className="stats-grid" />
              <text x={left - 6} y={y + 3} textAnchor="end" className="stats-axis">{format(v)}</text>
            </g>
          );
        })}
        {times.map((t, i) => {
          const x = left + i * slotW + (slotW - barW) / 2;
          let y = top + plotH;
          return (
            <g key={t}>
              {stacks.map((s) => {
                const v = s.values[i] || 0;
                if (v <= 0) return null;
                const bh = Math.max(1.5, (v / max) * plotH);
                y -= bh;
                return <rect key={s.key} x={x} y={y} width={barW} height={Math.max(0, bh - 1)} rx={Math.min(3, barW / 2)} fill={s.color} opacity={hover == null || hover === i ? 1 : 0.55} />;
              })}
              <rect x={left + i * slotW} y={top} width={slotW} height={plotH} fill="transparent" onMouseEnter={() => setHover(i)} />
              {i % every === 0 && (
                <text x={left + i * slotW + slotW / 2} y={height - 6} textAnchor="middle" className="stats-axis">{fmtTime(t)}</text>
              )}
            </g>
          );
        })}
      </svg>
      {hover != null && (
        <div className="stats-tip" style={{ left: Math.min(Math.max(left + hover * slotW, 8), width - 190) }} role="status">
          <strong>{hourOfDay ? `${fmtTime(times[hover])}:00` : day ? new Date(times[hover]).toLocaleDateString() : new Date(times[hover]).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false })}</strong>
          {stacks.filter((s) => (s.values[hover] || 0) > 0).map((s) => (
            <span key={s.key}><i style={{ background: s.color }} />{s.label}<b>{format(s.values[hover])}</b></span>
          ))}
          {stacks.length > 1 && <span className="stats-tip-total">Total<b>{format(totals[hover])}</b></span>}
        </div>
      )}
    </div>
  );
}

function Legend({ items }: { items: { label: string; color: string; note?: string }[] }) {
  return (
    <ul className="stats-legend">
      {items.map((i) => (
        <li key={i.label}><i style={{ background: i.color }} />{i.label}{i.note && <small>{i.note}</small>}</li>
      ))}
    </ul>
  );
}

function ShareBar({ parts }: { parts: { key: string; color: string; value: number }[] }) {
  const total = parts.reduce((s, p) => s + p.value, 0) || 1;
  return (
    <div className="stats-share" role="img" aria-label="Share">
      {parts.filter((p) => p.value > 0).map((p) => (
        <span key={p.key} style={{ flexGrow: p.value / total, background: p.color }} />
      ))}
    </div>
  );
}

function Segmented<T extends string | number>({ value, options, onChange, label }: { value: T; options: [T, string][]; onChange: (v: T) => void; label: string }) {
  return (
    <TabBar
      label={label}
      variant="segment"
      value={String(value)}
      onChange={(id) => onChange((typeof value === "number" ? Number(id) : id) as T)}
      items={options.map(([v, text]) => ({ id: String(v), label: text }))}
    />
  );
}

function Kpi({ label, value, sub, spark, color }: { label: string; value: string; sub?: string; spark?: number[]; color?: string }) {
  return (
    <div className="stats-kpi">
      <span>{label}</span>
      <strong>{value}</strong>
      {sub && <small>{sub}</small>}
      {spark && <Sparkline values={spark} color={color} />}
    </div>
  );
}

/* ---------- series helpers ---------- */

function fullTimes(data: StatsSummary) {
  const tz = data.tzOffsetMinutes * 60000;
  const align = (t: number) => Math.floor((t + tz) / data.bucketMs) * data.bucketMs - tz;
  const out: number[] = [];
  for (let t = align(data.from); t < data.to; t += data.bucketMs) out.push(t);
  return out;
}

function foldTop<T extends { requests: number }>(rows: T[], limit: number) {
  const sorted = [...rows].sort((a, b) => b.requests - a.requests);
  return { top: sorted.slice(0, limit), rest: sorted.slice(limit) };
}

/* ---------- tabs ---------- */

function Overview({ data }: { data: StatsSummary }) {
  const t = data.totals;
  const [metric, setMetric] = useState<"requests" | "tokens" | "cost">("requests");
  const times = useMemo(() => fullTimes(data), [data]);
  const byTime = new Map(data.series.map((p) => [p.t, p]));
  const pick = (key: "requests" | "tokens" | "cost" | "failed") => times.map((x) => (byTime.get(x)?.[key] as number | null | undefined) ?? 0);
  const failedSeries = pick("failed");
  const requestSeries = pick("requests");
  const tokenSeries = pick("tokens");
  const costSeries = pick("cost");
  const mixTotal = tokensOf(t) || 1;
  const mix = [
    { key: "input", label: "Uncached input", color: "var(--s1)", value: t.input },
    { key: "read", label: "Cache read", color: "var(--s3)", value: t.cacheRead },
    { key: "write", label: "Cache write", color: "var(--s4)", value: t.cacheWrite },
    { key: "out", label: "Output", color: "var(--s5)", value: t.output },
  ];
  const agentTotal = data.agents.reduce((s, a) => s + a.tokens, 0) || 1;
  const stacks: Stack[] = [
    {
      key: metric,
      label: metric === "requests" ? "Requests" : metric === "tokens" ? "Tokens" : "Cost",
      color: "var(--s1)",
      values: metric === "requests" ? requestSeries : metric === "tokens" ? tokenSeries : costSeries,
    },
  ];
  return (
    <>
      <div className="stats-kpis five">
        <Kpi label="API-equivalent cost" value={money(t.cost)} sub={t.pricedRequests < t.requests ? `${num(t.pricedRequests)} of ${num(t.requests)} requests priced` : "all requests priced"} spark={costSeries} color="var(--s5)" />
        <Kpi label="Requests" value={num(t.requests)} sub={`${num(t.failed)} failed${t.aborted ? ` · ${num(t.aborted)} aborted` : ""}`} spark={requestSeries} />
        <Kpi label="Conversation tokens" value={num(tokensOf(t))} sub={`${num(t.output)} output`} spark={tokenSeries} />
        <Kpi label="Cache rate" value={pct(cacheRate(t))} sub={`${num(t.cacheRead)} read from cache`} />
        <Kpi label="Error rate" value={pct(errorRate(t))} sub={`${num(t.requests - t.failed)} succeeded`} spark={failedSeries} color="var(--s2)" />
      </div>
      <div className="stats-strip">
        <div><span>Uncached input</span><strong>{num(t.input)}</strong></div>
        <div><span>Cache read</span><strong>{num(t.cacheRead)}</strong></div>
        <div><span>Cache write</span><strong>{num(t.cacheWrite)}</strong></div>
        <div><span>Output</span><strong>{num(t.output)}</strong></div>
        <div><span>Premium requests</span><strong>{num(t.premium)}</strong></div>
        <div><span>Tokens/s</span><strong>{rate(t.tokensPerSec)}</strong></div>
        <div><span>Avg latency</span><strong>{ms(t.avgDurationMs)}</strong></div>
        <div><span>Avg TTFT</span><strong>{ms(t.avgTtftMs)}</strong></div>
      </div>
      <div className="stats-split">
        <section className="stats-card">
          <header>
            <div><h3>Activity</h3><p>{data.bucketMs >= 86400000 ? "Per day" : "Per hour"}</p></div>
            <Segmented value={metric} onChange={setMetric} label="Metric" options={[["requests", "Requests"], ["tokens", "Tokens"], ["cost", "Cost"]]} />
          </header>
          <StackedBars times={times} stacks={stacks} bucketMs={data.bucketMs} format={metric === "cost" ? money : num} label={`${metric} per bucket`} />
        </section>
        <section className="stats-card">
          <header><div><h3>Token mix</h3><p>Where conversation tokens went</p></div></header>
          <ShareBar parts={mix} />
          <Legend items={mix.map((m) => ({ label: m.label, color: m.color, note: pct(m.value / mixTotal, 0) }))} />
          <h4>By agent</h4>
          <ShareBar parts={data.agents.map((a) => ({ key: a.agent, color: a.agent === "main" ? "var(--s1)" : "var(--s5)", value: a.tokens }))} />
          <ul className="stats-agents">
            {data.agents.map((a) => (
              <li key={a.agent}>
                <i style={{ background: a.agent === "main" ? "var(--s1)" : "var(--s5)" }} />
                <span>{a.agent === "main" ? "Main agent" : a.agent === "subagent" ? "Subagents" : a.agent}</span>
                <small>{num(a.requests)} req</small>
                <small>{num(a.tokens)}</small>
                <b>{pct(a.tokens / agentTotal)}</b>
              </li>
            ))}
          </ul>
        </section>
      </div>
    </>
  );
}

type SortKey = "requests" | "cost" | "tokens" | "cacheRate" | "errors" | "tps" | "ttft";

function ModelsTab({ data }: { data: StatsSummary }) {
  const [share, setShare] = useState<"share" | "requests">("requests");
  const [sort, setSort] = useState<SortKey>("requests");
  const times = useMemo(() => fullTimes(data), [data]);
  const key = (m: { model: string; provider: string; harness: string }) => `${m.model}\u0000${m.provider}\u0000${m.harness}`;
  const { top, rest } = foldTop(data.models, 6);
  const topKeys = new Set(top.map(key));
  const total = data.totals.requests || 1;
  const stacks: Stack[] = top.map((m) => ({
    key: key(m),
    label: m.model,
    color: colorVar(m.model),
    values: times.map((t) => data.modelSeries.find((p) => p.t === t && key(p) === key(m))?.requests ?? 0),
  }));
  if (rest.length) {
    stacks.push({ key: "other", label: "Other", color: "var(--s-other)", values: times.map((t) => data.modelSeries.filter((p) => p.t === t && !topKeys.has(key(p))).reduce((s, p) => s + p.requests, 0)) });
  }
  const shown: Stack[] = share === "share"
    ? stacks.map((s) => ({ ...s, values: s.values.map((v, i) => { const tot = stacks.reduce((sum, x) => sum + (x.values[i] || 0), 0); return tot ? (v / tot) * 100 : 0; }) }))
    : stacks;
  const rows = [...data.models].sort((a, b) => {
    const v = (m: typeof a) => ({ requests: m.requests, cost: m.cost ?? -1, tokens: tokensOf(m), cacheRate: cacheRate(m), errors: errorRate(m), tps: m.tokensPerSec ?? -1, ttft: m.avgTtftMs ?? Infinity })[sort];
    return sort === "ttft" ? v(a) - v(b) : v(b) - v(a);
  });
  const maxRequests = Math.max(...data.models.map((m) => m.requests), 1);
  const providers = new Set(data.models.map((m) => m.provider));
  const most = top[0];
  const th = (k: SortKey, label: string) => (
    <th scope="col" aria-sort={sort === k ? "descending" : "none"}><button type="button" onClick={() => setSort(k)}>{label}{sort === k ? " ↓" : ""}</button></th>
  );
  return (
    <>
      <div className="stats-kpis four">
        <Kpi label="Models used" value={String(data.models.length)} sub={`across ${providers.size} provider${providers.size === 1 ? "" : "s"}`} />
        <Kpi label="Most used" value={most?.model ?? "—"} sub={most ? `${pct(most.requests / total)} of requests · ${most.provider}` : ""} />
        <Kpi label="Requests" value={num(data.totals.requests)} sub={`${num(data.totals.failed)} failed`} spark={times.map((t) => data.series.find((p) => p.t === t)?.requests ?? 0)} />
        <Kpi label="API-equivalent cost" value={money(data.totals.cost)} />
      </div>
      <section className="stats-card">
        <header>
          <div><h3>Request share</h3><p>Each model's share of requests per {data.bucketMs >= 86400000 ? "day" : "hour"}</p></div>
          <Segmented value={share} onChange={setShare} label="Chart mode" options={[["share", "Share"], ["requests", "Requests"]]} />
        </header>
        <StackedBars times={times} stacks={shown} bucketMs={data.bucketMs} format={share === "share" ? (n) => `${Math.round(n)}%` : num} label="Requests by model" />
        <Legend items={stacks.map((s) => ({ label: s.label, color: s.color, note: pct(data.models.find((m) => key(m) === s.key)?.requests! / total || (rest.reduce((a, m) => a + m.requests, 0) / total), 1) }))} />
      </section>
      <section className="stats-card">
        <header><div><h3>All models</h3><p>Sorted by {sort}; choose a column heading to re-sort</p></div></header>
        <div className="stats-table-wrap">
          <table className="stats-table">
            <thead><tr><th scope="col">Model</th>{th("requests", "Requests")}{th("cost", "Cost")}{th("tokens", "Tokens")}{th("cacheRate", "Cache rate")}{th("errors", "Errors")}{th("tps", "Tokens/s")}{th("ttft", "TTFT")}<th scope="col">Trend</th></tr></thead>
            <tbody>
              {rows.map((m) => (
                <tr key={key(m)}>
                  <th scope="row"><i style={{ background: topKeys.has(key(m)) ? colorVar(m.model) : "var(--s-other)" }} /><span><code>{m.model}</code><small>{m.provider} · {harnessName(m.harness)}</small></span></th>
                  <td className="num"><span className="stats-inline">{num(m.requests)}<u style={{ width: `${(m.requests / maxRequests) * 56}px` }} /></span></td>
                  <td className="num">{money(m.cost)}</td>
                  <td className="num">{num(tokensOf(m))}</td>
                  <td className="num">{pct(cacheRate(m))}</td>
                  <td className="num">{pct(errorRate(m), 0)}</td>
                  <td className="num">{rate(m.tokensPerSec)}</td>
                  <td className="num">{ms(m.avgTtftMs)}</td>
                  <td className="trend"><Sparkline values={times.map((t) => data.modelSeries.find((p) => p.t === t && key(p) === key(m))?.requests ?? 0)} color={colorVar(m.model)} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </>
  );
}

function ProvidersTab({ data }: { data: StatsSummary }) {
  const [metric, setMetric] = useState<"tokens" | "cost" | "requests">("tokens");
  const [peak, setPeak] = useState("");
  const times = useMemo(() => fullTimes(data), [data]);
  const t = data.totals;
  const { top, rest } = foldTop(data.providers, 6);
  const topNames = new Set(top.map((p) => p.provider));
  const totalTokens = tokensOf(t) || 1;
  const stacks: Stack[] = top.map((p) => ({
    key: p.provider,
    label: p.provider,
    color: colorVar(p.provider),
    values: times.map((x) => {
      const row = data.providerSeries.find((s) => s.t === x && s.provider === p.provider);
      return (metric === "cost" ? row?.cost : row?.[metric]) ?? 0;
    }),
  }));
  if (rest.length) {
    stacks.push({ key: "other", label: "Other", color: "var(--s-other)", values: times.map((x) => data.providerSeries.filter((s) => s.t === x && !topNames.has(s.provider)).reduce((sum, s) => sum + ((metric === "cost" ? s.cost : s[metric]) ?? 0), 0)) });
  }
  const hours = Array.from({ length: 24 }, (_, h) => h);
  const peakRows = data.hourOfDay.filter((r) => !peak || r.provider === peak);
  const hourStacks: Stack[] = (peak ? [peak] : top.map((p) => p.provider)).map((name) => ({
    key: name,
    label: name,
    color: colorVar(name),
    values: hours.map((h) => peakRows.filter((r) => r.hour === h && r.provider === name).reduce((s, r) => s + r.tokens, 0)),
  }));
  const hourTotals = hours.map((h) => hourStacks.reduce((s, x) => s + x.values[h], 0));
  const peakHour = hourTotals.indexOf(Math.max(...hourTotals));
  const maxTokens = Math.max(...data.providers.map(tokensOf), 1);
  return (
    <>
      <div className="stats-kpis five">
        <Kpi label="Providers" value={String(data.providers.length)} sub={top[0] ? `Most tokens: ${data.providers.slice().sort((a, b) => tokensOf(b) - tokensOf(a))[0].provider}` : ""} />
        <Kpi label="Requests" value={num(t.requests)} sub={`${num(t.failed)} failed`} spark={times.map((x) => data.series.find((p) => p.t === x)?.requests ?? 0)} />
        <Kpi label="Tokens" value={num(tokensOf(t))} spark={times.map((x) => data.series.find((p) => p.t === x)?.tokens ?? 0)} color="var(--s3)" />
        <Kpi label="API-equivalent cost" value={money(t.cost)} />
        <Kpi label="Error rate" value={pct(errorRate(t))} sub={`${num(t.requests - t.failed)} succeeded`} />
      </div>
      <section className="stats-card">
        <header><div><h3>Provider totals</h3><p>Cost is an API-equivalent estimate reported by the harness; Meshra does not price requests.</p></div></header>
        <div className="stats-table-wrap">
          <table className="stats-table">
            <thead><tr><th scope="col">Provider</th><th scope="col">Requests</th><th scope="col">Error rate</th><th scope="col">Models</th><th scope="col">Tokens</th><th scope="col">Share</th><th scope="col">Cost</th><th scope="col">Tokens/s</th><th scope="col">Premium</th></tr></thead>
            <tbody>
              {data.providers.map((p) => (
                <tr key={p.provider}>
                  <th scope="row"><i style={{ background: topNames.has(p.provider) ? colorVar(p.provider) : "var(--s-other)" }} /><code>{p.provider}</code></th>
                  <td className="num">{num(p.requests)}</td>
                  <td className="num">{p.failed}<span className="stats-pill">{pct(errorRate(p))}</span></td>
                  <td className="num">{p.models}</td>
                  <td className="num"><span className="stats-inline">{num(tokensOf(p))}<u style={{ width: `${(tokensOf(p) / maxTokens) * 56}px` }} /></span></td>
                  <td className="num">{pct(tokensOf(p) / totalTokens)}</td>
                  <td className="num">{money(p.cost)}</td>
                  <td className="num">{rate(p.tokensPerSec)}</td>
                  <td className="num">{num(p.premium)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
      <div className="stats-split even">
        <section className="stats-card">
          <header>
            <div><h3>Burn by provider</h3><p>Per {data.bucketMs >= 86400000 ? "day" : "hour"}, top 6 providers stacked</p></div>
            <Segmented value={metric} onChange={setMetric} label="Metric" options={[["tokens", "Tokens"], ["cost", "Cost"], ["requests", "Requests"]]} />
          </header>
          <StackedBars times={times} stacks={stacks} bucketMs={data.bucketMs} format={metric === "cost" ? money : num} label={`${metric} by provider`} />
          <Legend items={stacks.map((s) => ({ label: s.label, color: s.color }))} />
        </section>
        <section className="stats-card">
          <header>
            <div><h3>Peak burn hours</h3><p>Tokens by local hour of day{Math.max(...hourTotals) > 0 ? `; peak at ${String(peakHour).padStart(2, "0")}:00` : ""}</p></div>
            <select aria-label="Provider" value={peak} onChange={(e) => setPeak(e.target.value)}>
              <option value="">All providers</option>
              {data.providers.map((p) => <option key={p.provider} value={p.provider}>{p.provider}</option>)}
            </select>
          </header>
          <StackedBars times={hours.map((h) => h * 3600000)} stacks={hourStacks} bucketMs={3600000} format={num} label="Tokens by hour of day" hourOfDay />
        </section>
      </div>
    </>
  );
}

const STATUS_TABS: ["all" | "ok" | "aborted" | "failed", string][] = [
  ["all", "All"],
  ["ok", "OK"],
  ["aborted", "Aborted"],
  ["failed", "Failed"],
];

function RequestsTab({ range, harness, project, tz }: { range: 24 | 168 | 720; harness: string; project: string; tz: number }) {
  const [status, setStatus] = useState<"all" | "ok" | "aborted" | "failed">("all");
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState<number | null>(null);
  useEffect(() => {
    const id = setTimeout(() => setQuery(search.trim()), 250);
    return () => clearTimeout(id);
  }, [search]);
  const log = useInfiniteQuery({
    queryKey: ["stats", "requests", range, harness, project, status, query],
    queryFn: ({ pageParam }) =>
      automation("stats.requests", {
        rangeHours: range, harness, project, tzOffsetMinutes: tz, status, search: query, limit: 100,
        ...(pageParam ? { beforeTs: pageParam.ts, beforeId: pageParam.id } : {}),
      }),
    initialPageParam: undefined as { ts: number; id: number } | undefined,
    getNextPageParam: (last) => {
      const tail = last.items[last.items.length - 1];
      return last.hasMore && tail ? { ts: tail.ts, id: tail.id } : undefined;
    },
    refetchInterval: 30000,
    gcTime: 60000,
  });
  const first = log.data?.pages[0];
  const items: StatsRequestItem[] = log.data?.pages.flatMap((p) => p.items) ?? [];
  const s = first?.summary;
  return (
    <>
      <div className="stats-kpis six">
        <Kpi label="Requests" value={s ? num(s.requests) : "…"} sub={s?.firstTs ? `since ${ago(s.firstTs)}` : ""} />
        <Kpi label="Failed" value={s ? num(s.failed) : "…"} sub={s ? `${pct(s.requests ? s.failed / s.requests : 0)}${s.aborted ? ` · ${s.aborted} aborted` : ""}` : ""} />
        <Kpi label="Tokens" value={s ? num(s.tokens) : "…"} />
        <Kpi label="API-equivalent cost" value={s ? money(s.cost) : "…"} />
        <Kpi label="Median duration" value={ms(s?.medianDurationMs)} sub={s?.p95DurationMs != null ? `p95 ${ms(s.p95DurationMs)}` : ""} />
        <Kpi label="Median TTFT" value={ms(s?.medianTtftMs)} sub={s?.p95TtftMs != null ? `p95 ${ms(s.p95TtftMs)}` : ""} />
      </div>
      <section className="stats-card">
        <header className="wrap">
          <div><h3>Request log</h3><p>{items.length} of {s ? num(status === "all" ? s.requests : s[status]) : "…"} requests, newest first</p></div>
          <div className="stats-log-tools">
            <label className="stats-search"><Search size={14} /><input type="search" placeholder="Model, provider or project" value={search} onChange={(e) => setSearch(e.target.value)} aria-label="Search requests" /></label>
            <TabBar
              label="Status"
              variant="segment"
              value={status}
              onChange={(id) => setStatus(id as typeof status)}
              items={STATUS_TABS.map(([id, label]) => ({ id, label, count: s ? (id === "all" ? s.requests : s[id]) : undefined }))}
            />
          </div>
        </header>
        {log.isError ? <Notice>{log.error.message}</Notice> : log.isPending ? <p role="status" className="muted">Loading requests…</p> : !items.length ? <p className="muted">No requests match.</p> : (
          <div className="stats-table-wrap">
            <table className="stats-table log">
              <thead><tr><th scope="col">Model</th><th scope="col">When</th><th scope="col">Project</th><th scope="col">Input</th><th scope="col">Cache read</th><th scope="col">Output</th><th scope="col">Cost</th><th scope="col">Duration</th><th scope="col">TTFT</th><th scope="col">Status</th></tr></thead>
              <tbody>
                {items.map((r) => (
                  <Fragment key={r.id}>
                    <tr className={open === r.id ? "is-open" : ""} onClick={() => setOpen(open === r.id ? null : r.id)} tabIndex={0} onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); setOpen(open === r.id ? null : r.id); } }} aria-expanded={open === r.id}>
                      <th scope="row"><code>{r.model}</code><small>{r.provider} · {harnessName(r.harness)}</small></th>
                      <td title={new Date(r.ts).toLocaleString()}>{ago(r.ts)}</td>
                      <td className="proj" title={r.project}>{r.project || "—"}</td>
                      <td className="num">{num(r.input)}</td>
                      <td className="num">{num(r.cacheRead)}</td>
                      <td className="num">{num(r.output)}</td>
                      <td className="num">{money(r.cost)}</td>
                      <td className="num">{ms(r.durationMs)}</td>
                      <td className="num">{ms(r.ttftMs)}</td>
                      <td><span className={`stats-status ${r.status}`}><i />{r.status === "ok" ? "OK" : r.status === "aborted" ? "Aborted" : "Failed"}<small>{r.stopReason}</small></span></td>
                    </tr>
                    {open === r.id && (
                      <tr className="detail"><td colSpan={10}>
                        <dl>
                          <div><dt>Time</dt><dd>{new Date(r.ts).toLocaleString()}</dd></div>
                          <div><dt>Agent</dt><dd>{r.agent}</dd></div>
                          <div><dt>Cache write</dt><dd>{num(r.cacheWrite)}</dd></div>
                          <div><dt>Stop reason</dt><dd>{r.stopReason || "—"}</dd></div>
                          {r.error && <div className="wide"><dt>Error</dt><dd>{r.error}</dd></div>}
                        </dl>
                      </td></tr>
                    )}
                  </Fragment>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {log.hasNextPage && <Button variant="outline" size="sm" disabled={log.isFetchingNextPage} onClick={() => void log.fetchNextPage()}>{log.isFetchingNextPage ? "Loading…" : "Load more"}</Button>}
      </section>
    </>
  );
}

/* ---------- page ---------- */

const HARNESS_ORDER = ["openai", "omp", "claude"];

export function Stats() {
  const client = useQueryClient();
  const [tab, setTab] = useState<Tab>("overview");
  const [range, setRange] = useState<24 | 168 | 720>(24);
  const [harness, setHarness] = useState("");
  const [project, setProject] = useState("");
  const tz = -new Date().getTimezoneOffset();
  const summary = useQuery({
    queryKey: ["stats", "summary", range, harness, project],
    queryFn: () => automation("stats.summary", { rangeHours: range, harness, project, tzOffsetMinutes: tz }),
    refetchInterval: (q) => (q.state.data?.importing ? 4000 : 30000),
    gcTime: 60000,
  });
  const data = summary.data;
  async function refresh() {
    await automation("stats.refresh", {});
    setTimeout(() => void client.invalidateQueries({ queryKey: ["stats"] }), 1500);
  }
  const direct = data?.harnesses.find((h) => h.name === "openai")?.requests ?? 0;
  const harnesses = [...(data?.harnesses ?? [])];
  if (data && !harnesses.some((h) => h.name === "openai")) harnesses.push({ name: "openai", requests: 0 });
  harnesses.sort((a, b) => HARNESS_ORDER.indexOf(a.name) - HARNESS_ORDER.indexOf(b.name));
  return (
    <div className="stats-root">
      <div className="stats-bar">
        <TabBar label="Stats sections" value={tab} onChange={setTab} items={TABS.map(([id, label]) => ({ id, label }))} />
        <div className="stats-filters">
          <Segmented value={range} onChange={setRange} label="Time range" options={RANGES} />
          <select aria-label="Harness" value={harness} onChange={(e) => setHarness(e.target.value)}>
            <option value="">All harnesses</option>
            {harnesses.map((h) => <option key={h.name} value={h.name}>{h.name === "openai" ? "Direct API" : harnessName(h.name)} ({num(h.requests)})</option>)}
            {harness && !harnesses.some((h) => h.name === harness) && <option value={harness}>{harnessName(harness)}</option>}
          </select>
          <select aria-label="Project" value={project} onChange={(e) => setProject(e.target.value)}>
            <option value="">All projects</option>
            {(data?.projects ?? []).map((p) => <option key={p.name} value={p.name}>{p.name || "(none)"} ({num(p.requests)})</option>)}
          </select>
          <Button variant="outline" size="sm" onClick={() => void refresh()} disabled={summary.isFetching}><RefreshCw size={14} />Refresh</Button>
        </div>
      </div>
      {data && (
        <p className="stats-sources" role="status">
          {data.importing ? "Importing local logs… " : ""}
          {data.sources.map((s) => (
            <span key={s.source} className={s.available ? "" : "bad"}>
              {s.source === "omp" ? "OMP" : s.source === "claude" ? "Claude" : s.source}: {s.available ? `${num(s.requests)} requests, imported ${ago(s.lastRunAt)}` : s.error || "unavailable"}
            </span>
          ))}
          <span className={direct ? "" : "bad"}>
            Direct API: {direct ? `${num(direct)} requests, recorded live` : "no requests yet; they appear as soon as a Direct API agent answers"}
          </span>
          <span>Kept {data.retentionDays} days</span>
        </p>
      )}
      <TabPanel value={tab} order={TABS.map(([id]) => id)}>
      {summary.isError ? <Notice>{summary.error.message}</Notice> : summary.isPending ? <p role="status" className="muted">Loading stats…</p> : tab === "requests" ? (
        <RequestsTab range={range} harness={harness} project={project} tz={tz} />
      ) : !data || data.totals.requests === 0 ? (
        <div className="stats-empty">
          <h3>No requests in this range</h3>
          <p>Direct API requests appear as soon as an agent answers. OMP and Claude requests are imported read-only from their local logs{data?.importing ? " (importing now)" : ""}. Try a longer range or clear the filters.</p>
        </div>
      ) : tab === "overview" ? (
        <Overview data={data} />
      ) : tab === "models" ? (
        <ModelsTab data={data} />
      ) : (
        <ProvidersTab data={data} />
      )}
      </TabPanel>
    </div>
  );
}
