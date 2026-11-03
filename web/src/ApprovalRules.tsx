import { useState } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { automation, type ApprovalRule } from "./automation-api";
import { Notice } from "./App";
import { Button } from "./components/ui/button";

/** Rules are profile-scoped and never apply to external or unattended calls. */
export function ApprovalRules({ profileId }: { profileId?: string }) {
  const client = useQueryClient();
  const [selected, setSelected] = useState("");
  const [kind, setKind] = useState<ApprovalRule["matcherKind"]>("argv_exact");
  const [matcher, setMatcher] = useState("");
  const [expiry, setExpiry] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const profiles = useInfiniteQuery({
    queryKey: ["approval-rule-profiles"], initialPageParam: "",
    queryFn: ({ pageParam }) => automation("profiles.list", { limit: 100, ...(pageParam ? { after: pageParam } : {}) }),
    getNextPageParam: (page) => page.hasMore ? page.nextAfter : undefined,
    enabled: !profileId,
  });
  const id = profileId || selected;
  const policy = useQuery({ queryKey: ["approval-policy", id], queryFn: () => automation("approval_policy.get", { profileId: id }), enabled: !!id });
  const rules = useQuery({ queryKey: ["approval-rules", id], queryFn: () => automation("approval_rules.list", { profileId: id }), enabled: !!id });
  const suggestions = useQuery({ queryKey: ["approval-suggestions", id], queryFn: () => automation("approval_suggestions.list", { profileId: id }), enabled: !!id });

  async function mutate(action: () => Promise<unknown>) {
    setBusy(true); setError("");
    try {
      await action();
      await Promise.all([client.invalidateQueries({ queryKey: ["approval-policy", id] }), client.invalidateQueries({ queryKey: ["approval-rules", id] }), client.invalidateQueries({ queryKey: ["approvals"] })]);
    } catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  }
  const warning = "Changing approval rules or policy cancels outstanding actions for this profile. Continue?";
  return <details className="automation-attention">
    <summary>Approval rules and suggestions</summary>
    <p className="muted">Rules apply only to eligible direct API calls in an interactive console session. External tools, publishing and applying changes always require approval.</p>
    {!profileId && <label>Profile <select value={selected} onChange={(e) => setSelected(e.target.value)}>
      <option value="">Select a profile</option>
      {profiles.data?.pages.flatMap((page) => page.items).map((profile) => <option key={profile.id} value={profile.id}>{profile.name} ({profile.id})</option>)}
    </select></label>}
    {profiles.isError && <Notice>{profiles.error.message}</Notice>}
    {!profileId && profiles.hasNextPage && <Button variant="outline" disabled={profiles.isFetchingNextPage} onClick={() => void profiles.fetchNextPage()}>Load more profiles</Button>}
    {error && <Notice>{error}</Notice>}
    {id && <>
      {policy.isError && <Notice>{policy.error.message}</Notice>}
      {rules.isError && <Notice>{rules.error.message}</Notice>}
      {suggestions.isError && <Notice>{suggestions.error.message}</Notice>}
      {policy.data && <p>Approval policy: <strong>{policy.data.policy === "manual" ? "Manual" : "Allowlist"}</strong>{" "}
        <Button variant="outline" disabled={busy} onClick={() => {
          if (window.confirm(warning)) void mutate(() => automation("approval_policy.set", { profileId: id, policy: policy.data?.policy === "manual" ? "allowlist" : "manual" }));
        }}>{policy.data.policy === "manual" ? "Enable allowlist" : "Use manual approval"}</Button>
      </p>}
      <h3>Allow rules</h3>
      {rules.isPending ? <p role="status">Loading rules…</p> : rules.data?.items.length ? <ul className="automation-attention-list">{rules.data.items.map((rule) => <li className="automation-attention-item" key={rule.id}>
        <strong>{rule.tool}</strong> · {rule.matcherKind.replaceAll("_", " ")}
        <pre className="automation-attention-detail mono">{rule.matcher}</pre>
        <p className="muted">Created {new Date(rule.createdAt).toLocaleString()}{rule.expiresAt ? ` · Expires ${new Date(rule.expiresAt).toLocaleString()}` : " · No expiry"}</p>
        <Button variant="outline" disabled={busy} onClick={() => { if (window.confirm(`Revoke this rule? ${warning}`)) void mutate(() => automation("approval_rules.revoke", { profileId: id, id: rule.id })); }}>Revoke rule</Button>
      </li>)}</ul> : !rules.isError && <p className="muted">No allow rules for this profile.</p>}
      <form onSubmit={(e) => {
        e.preventDefault();
        if (!window.confirm(`Create this rule? ${warning}`)) return;
        void mutate(async () => {
          await automation("approval_rules.create", { profileId: id, tool: kind === "path_glob" ? "meshra_workspace_write" : "meshra_workspace_command", matcherKind: kind, matcher: matcher.trim(), ...(expiry ? { expiresAt: new Date(expiry).toISOString() } : {}) });
          setMatcher(""); setExpiry("");
        });
      }}>
        <h3>Create rule</h3>
        <label>Match type <select value={kind} onChange={(e) => setKind(e.target.value as ApprovalRule["matcherKind"])}>
          <option value="argv_exact">Exact command argv</option><option value="argv_prefix">Command argv prefix</option><option value="path_glob">Workspace write path glob</option>
        </select></label>
        <p className="picker-hint">{kind === "path_glob" ? "Relative workspace path glob; a matching write may replace any content at that path." : 'Enter a JSON array of arguments, for example ["git", "status"]. Shell commands cannot match.'}</p>
        <label>Matcher <input value={matcher} onChange={(e) => setMatcher(e.target.value)} required /></label>{" "}
        <label>Expires (optional) <input type="datetime-local" value={expiry} onChange={(e) => setExpiry(e.target.value)} /></label>{" "}
        <Button type="submit" disabled={busy || !matcher.trim()}>Create rule</Button>
      </form>
      <h3>Frequently approved actions</h3>
      <p className="muted">Suggestions are read-only, based on identical approvals recorded after this feature was enabled. They never create rules automatically.</p>
      {suggestions.isPending ? <p role="status">Loading suggestions…</p> : suggestions.data?.items.length ? <ul className="automation-attention-list">{suggestions.data.items.map((item, index) => <li className="automation-attention-item" key={`${item.tool}-${index}`}><strong>{item.tool}</strong> · Approved {item.count} times<pre className="automation-attention-detail mono">{JSON.stringify(item.arguments, null, 2)}</pre></li>)}</ul> : !suggestions.isError && <p className="muted">No repeated approvals yet.</p>}
    </>}
  </details>;
}
