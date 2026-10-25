import { useEffect, useState, type FormEvent } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { MessageSquare } from "lucide-react";
import { automation, type Connector, type ConnectorRoute } from "./automation-api";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";
import { Empty, Notice } from "./App";

const scopeKeys = ["workspaces", "guilds", "channels", "chats", "users"] as const;
const emptyRoute = (): ConnectorRoute => ({ id: "", revision: 0, profileId: "", allow: {}, autoStart: false, enabled: false });

export function MessagingConnections() {
  const client = useQueryClient();
  const connectors = useQuery({ queryKey: ["automation", "connectors"], queryFn: () => automation("connectors.list", {}), gcTime: 0 });
  const [selected, setSelected] = useState<Connector | null>(null);
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState("");
  const [kind, setKind] = useState("slack");
  const [token, setToken] = useState("");
  const [appToken, setAppToken] = useState("");
  const [routes, setRoutes] = useState<ConnectorRoute[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [deliveryID, setDeliveryID] = useState("");
  const deliveries = useInfiniteQuery({
    queryKey: ["automation", "deliveries", deliveryID], enabled: !!deliveryID, gcTime: 0,
    initialPageParam: "", queryFn: ({ pageParam }) => automation("connectors.deliveries", { id: deliveryID, after: pageParam || undefined, limit: 50 }),
    getNextPageParam: (page) => page.length === 50 ? page[page.length - 1].createdAt + "|" + page[page.length - 1].id : undefined,
  });
  useEffect(() => {
    const clear = () => { setToken(""); setAppToken(""); setEditing(false); setSelected(null); setRoutes(null); setDeliveryID(""); };
    window.addEventListener("meshra:cleanup", clear);
    return () => { window.removeEventListener("meshra:cleanup", clear); };
  }, []);
  async function changed() { await client.invalidateQueries({ queryKey: ["automation", "connectors"] }); }
  function edit(value: Connector | null) {
    setSelected(value); setName(value?.name || ""); setKind(value?.kind || "slack");
    setToken(""); setAppToken(""); setError(""); setNotice(""); setEditing(true);
  }
  async function save(event: FormEvent) {
    event.preventDefault(); if (busy) return;
    const secrets: Record<string, string> = {};
    if (token) secrets.token = token;
    if (appToken && kind === "slack") secrets.appToken = appToken;
    setToken(""); setAppToken(""); setBusy(true); setError("");
    try {
      await automation("connectors.save", { connector: { id: selected?.id, name, kind }, expectedRevision: selected?.revision, secrets });
      setEditing(false); await changed(); setNotice("Saved disabled. Review exact scopes and startup permission before enabling.");
    } catch (failure) { setError((failure as Error).message); }
    finally { setBusy(false); }
  }
  async function control(value: Connector, action: "test" | "enable" | "remove") {
    if (busy) return;
    if (action === "enable" && !value.enabled && !window.confirm("Enable outbound " + value.kind + " connections for the reviewed routes? External messages are untrusted. Only the local console can approve execution.")) return;
    if (action === "remove" && !window.confirm("Remove this disabled connector and its delivery receipts? Conversation history is retained.")) return;
    setBusy(true); setError(""); setNotice("");
    try {
      if (action === "test") { await automation("connectors.test", { id: value.id }); setNotice("Credential check passed. No reply or model turn was sent."); }
      else if (action === "enable") await automation("connectors.setEnabled", { id: value.id, expectedRevision: value.revision, enabled: !value.enabled });
      else await automation("connectors.remove", { id: value.id, expectedRevision: value.revision });
      await changed();
    } catch (failure) { setError((failure as Error).message); }
    finally { setBusy(false); }
  }
  async function saveRoutes(event: FormEvent) {
    event.preventDefault(); if (!selected || !routes || busy) return;
    if (!window.confirm("Save these exact ID allowlists and startup grants? Empty allowlists deny all messages. autoStart permits a fresh isolated binding to start, not tool approvals.")) return;
    setBusy(true); setError("");
    try {
      await automation("connectors.routesSave", { id: selected.id, expectedRevision: selected.revision, routes: routes.map(({ revision: _revision, ...route }) => route) });
      setRoutes(null); setSelected(null); await changed();
    } catch (failure) { setError((failure as Error).message); }
    finally { setBusy(false); }
  }
  async function retry(id: string) {
    if (busy || !window.confirm("This reply may already have been delivered. Retry posting with duplicate risk? The model task will not run again.")) return;
    setBusy(true); setError("");
    try { await automation("connectors.retryDelivery", { deliveryId: id, confirmDuplicateRisk: true }); await deliveries.refetch(); }
    catch (failure) { setError((failure as Error).message); }
    finally { setBusy(false); }
  }
  function updateRoute(index: number, patch: Partial<ConnectorRoute>) { setRoutes((items) => items?.map((route, i) => i === index ? { ...route, ...patch } : route) || null); }
  return <section className="automation-card">
    <header className="automation-card-head"><h2>Messaging connectors</h2><Button type="button" disabled={busy} onClick={() => edit(null)}>Add connector</Button></header>
    <p className="muted">Use your own Slack app, Discord bot or Telegram bot. Outbound connections only; no public listener. New connections and routes start disabled. Remote users never receive approval or administration authority.</p>
    {error && <Notice>{error}</Notice>}{notice && <p role="status">{notice}</p>}{connectors.isError && <Notice>{connectors.error.message}</Notice>}
    {!connectors.isPending && !connectors.data?.length && <Empty icon={MessageSquare} title="No messaging connectors">Add your own bot credentials, then review exact IDs and a target profile.</Empty>}
    {connectors.data?.map((value) => <article className="automation-card" key={value.id}>
      <strong>{value.name}</strong><p>{value.kind} · {value.enabled ? "Enabled" : "Disabled"} · revision {value.revision}</p>
      <p className="muted">{Object.entries(value.secrets).map(([slot, status]) => slot + ": " + status).join(" · ")}</p>
      {value.lastError && <Notice>{value.lastError}</Notice>}
      <ul>{value.routes.map((route) => <li key={route.id}>Profile {route.profileId} · {route.enabled ? "route enabled" : "route disabled"} · {route.autoStart ? "automatic startup reviewed" : "manual startup"}</li>)}</ul>
      <div className="automation-actions">
        <Button variant="outline" disabled={busy} onClick={() => edit(value)}>Edit credentials</Button>
        <Button variant="outline" disabled={busy} onClick={() => { setSelected(value); setRoutes(value.routes.map((route) => ({ ...route, allow: { ...route.allow } }))); }}>Review routes</Button>
        <Button variant="outline" disabled={busy} onClick={() => void control(value, "test")}>Test credentials</Button>
        <Button variant="outline" disabled={busy} onClick={() => void control(value, "enable")}>{value.enabled ? "Disable" : "Review and enable"}</Button>
        <Button variant="outline" onClick={() => setDeliveryID(value.id)}>Deliveries</Button>
        <Button variant="ghost" disabled={busy || value.enabled} onClick={() => void control(value, "remove")}>Remove</Button>
      </div>
    </article>)}
    <Dialog open={editing} onOpenChange={(open) => { if (!busy) { setEditing(open); if (!open) { setToken(""); setAppToken(""); } } }} title={selected ? "Edit connector" : "Add connector"} description="Credentials are write-only. Saving disables the connector until you review and enable it again.">
      <form className="form-stack" onSubmit={save}>
        <label>Name<input required maxLength={128} value={name} onChange={(event) => setName(event.target.value)} /></label>
        <label>Provider<select disabled={!!selected} value={kind} onChange={(event) => setKind(event.target.value)}>{["slack", "discord", "telegram"].map((provider) => <option key={provider}>{provider}</option>)}</select></label>
        <label>{selected ? "Replacement bot token (blank keeps stored token)" : "Bot token"}<input type="password" autoComplete="off" required={!selected} value={token} onChange={(event) => setToken(event.target.value)} /></label>
        {kind === "slack" && <label>Slack app-level Socket Mode token<input type="password" autoComplete="off" required={!selected} value={appToken} onChange={(event) => setAppToken(event.target.value)} /></label>}
        <Button type="submit" disabled={busy}>Save disabled connector</Button>
      </form>
    </Dialog>
    <Dialog open={routes !== null} onOpenChange={(open) => { if (!open && !busy) setRoutes(null); }} title="Review external message routes" description="Every nonempty ID list restricts the route. Empty overall allowlists deny all. Names and wildcards are not accepted." wide>
      <form className="form-stack" onSubmit={saveRoutes}>
        {routes?.map((route, index) => <fieldset className="form-stack" key={route.id || "new-" + index}>
          <legend>Route {index + 1}</legend>
          <label>Target profile ID<input required value={route.profileId} onChange={(event) => updateRoute(index, { profileId: event.target.value })} /></label>
          {scopeKeys.map((scope) => <label key={scope}>{scope} IDs (comma-separated)<input value={(route.allow[scope] || []).join(",")} onChange={(event) => updateRoute(index, { allow: { ...route.allow, [scope]: event.target.value.split(",").map((id) => id.trim()).filter(Boolean) } })} /></label>)}
          <label><input type="checkbox" checked={route.enabled} onChange={(event) => updateRoute(index, { enabled: event.target.checked })} /> Enable this reviewed route</label>
          <label><input type="checkbox" checked={route.autoStart} onChange={(event) => updateRoute(index, { autoStart: event.target.checked })} /> Allow automatic startup of this route's separate profile binding. Does not approve tools.</label>
          <Button type="button" variant="ghost" onClick={() => setRoutes((items) => items?.filter((_, i) => i !== index) || [])}>Remove route</Button>
        </fieldset>)}
        <Button type="button" variant="outline" disabled={busy || (routes?.length || 0) >= 64} onClick={() => setRoutes((items) => [...(items || []), emptyRoute()])}>Add disabled route</Button>
        <Button type="submit" disabled={busy}>Review and save routes</Button>
      </form>
    </Dialog>
    <Dialog open={!!deliveryID} onOpenChange={(open) => { if (!open) setDeliveryID(""); }} title="Connector deliveries" description="Reply posting is separate from model execution. An uncertain delivery requires explicit duplicate-risk confirmation." wide>
      {deliveries.isError && <Notice>{deliveries.error.message}</Notice>}
      {deliveries.data?.pages.flat().map((delivery) => <article className="automation-card" key={delivery.id}><strong>{delivery.state}</strong><p>Task {delivery.taskId} · chunk {delivery.chunk}</p>{delivery.error && <Notice>{delivery.error}</Notice>}{["failed", "uncertain_delivery"].includes(delivery.state) && <Button disabled={busy} variant="outline" onClick={() => void retry(delivery.id)}>Retry posting with duplicate risk</Button>}</article>)}
      <div className="automation-actions"><Button variant="outline" onClick={() => void deliveries.refetch()}>Refresh deliveries</Button>{deliveries.hasNextPage && <Button disabled={deliveries.isFetchingNextPage} onClick={() => void deliveries.fetchNextPage()}>More deliveries</Button>}</div>
    </Dialog>
  </section>;
}
