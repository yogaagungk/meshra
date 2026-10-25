import { useEffect, useRef, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { authenticatedFetch } from "./api";
import { automation, type BrowserAction, type BrowserSession } from "./automation-api";
import { Notice } from "./App";
import { Button } from "./components/ui/button";

export function BrowserPanel({ profileId, conversationId, enabled }: { profileId: string; conversationId: string; enabled: boolean }) {
  const client = useQueryClient();
  const [selected, setSelected] = useState("");
  const [session, setSession] = useState<BrowserSession | null>(null);
  const [frame, setFrame] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [action, setAction] = useState("navigate");
  const [url, setURL] = useState("");
  const [selector, setSelector] = useState("");
  const [text, setText] = useState("");
  const lease = useRef<BrowserSession | null>(null);
  const alive = useRef(true);
  const sessions = useQuery({ queryKey: ["automation", "browser", profileId, conversationId], queryFn: () => automation("browser.sessions", { profileId, conversationId }), refetchInterval: 5000, gcTime: 0 });
  const current = useQuery({ queryKey: ["automation", "browser-state", selected], queryFn: () => automation("browser.state", { id: selected }), enabled: !!selected, refetchInterval: 3000, gcTime: 0 });
  useEffect(() => { if (current.data) { setSession(current.data); if (lease.current?.id === current.data.id) lease.current = current.data.state === "taking_over" ? current.data : null; } }, [current.data]);
  useEffect(() => {
    if (!selected && sessions.data?.items.length) setSelected(sessions.data.items[0].id);
  }, [selected, sessions.data]);
  useEffect(() => {
    alive.current = true;
    // Disconnect never resumes model input. The durable taking_over state
    // remains paused until the operator explicitly clicks Release control.
    const release = () => { lease.current = null; };
    window.addEventListener("meshra:cleanup", release);
    return () => { alive.current = false; release(); window.removeEventListener("meshra:cleanup", release); };
  }, []);
  useEffect(() => {
    if (!session || ["closed", "error"].includes(session.state)) { setFrame(""); return; }
    const controller = new AbortController(); let objectURL = "";
    void authenticatedFetch("browser/" + encodeURIComponent(session.id) + "/frame", { signal: controller.signal }).then(async (response) => {
      if (!response.ok) throw new Error("Browser frame unavailable (" + response.status + ").");
      const blob = await response.blob(); if (controller.signal.aborted) return;
      objectURL = URL.createObjectURL(blob); setFrame(objectURL);
    }).catch((e: Error) => { if (!controller.signal.aborted) setError(e.message); });
    return () => { controller.abort(); URL.revokeObjectURL(objectURL); };
  }, [session?.id, session?.frameSeq, session?.state]);
  async function run(operation: "open" | "takeover" | "release" | "close" | "input" | "clear", event?: FormEvent) {
    event?.preventDefault(); setBusy(true); setError("");
    try {
      let value: BrowserSession | undefined;
      if (operation === "open") value = await automation("browser.open", { profileId, conversationId });
      else if (operation === "clear") {
        if (!window.confirm("Close browser sessions and permanently clear private browser data for this profile conversation?")) return;
        await automation("browser.clearData", { profileId, conversationId, confirm: true }); setSelected(""); setSession(null); setFrame("");
      } else if (session) {
        if (operation === "input") {
          const input: BrowserAction = { action };
          if (action === "navigate" || action === "open") input.url = url;
          if (["click", "type", "upload"].includes(action)) input.selector = selector;
          if (action === "type") input.text = text;
          if (action === "key") input.key = text;
          if (action === "upload") input.attachmentId = text;
          if (action === "switchTab") input.tab = Number(text);
          if (action === "scroll") { input.direction = "down"; input.amount = 400; }
          await automation("browser.input", { id: session.id, epoch: session.epoch, frameSeq: session.frameSeq, action: input });
          value = await automation("browser.state", { id: session.id });
        } else if (operation === "takeover") { value = await automation("browser.takeover", { id: session.id, expectedEpoch: session.epoch }); lease.current = value; }
        else if (operation === "release") { value = await automation("browser.release", { id: session.id, expectedEpoch: session.epoch }); lease.current = null; }
        else value = await automation("browser.close", { id: session.id });
      }
      if (value && !alive.current) return;
      if (value) { setSession(value); setSelected(value.id); client.setQueryData(["automation", "browser-state", value.id], value); }
      await sessions.refetch();
    } catch (e) { if (alive.current) setError((e as Error).message); }
    finally { if (alive.current) setBusy(false); }
  }
  return <section className="automation-card"><strong>Isolated browser / computer</strong><p className="muted">Only the isolated session is visible. This is not access to your desktop. Take over explicitly to send human input; release returns control to the profile.</p>
    {error && <Notice>{error}</Notice>}{sessions.isError && <Notice>{sessions.error.message}</Notice>}{current.isError && <Notice>{current.error.message}</Notice>}
    <div className="automation-actions"><Button type="button" disabled={busy || !enabled} onClick={() => void run("open")}>Open isolated browser</Button><Button type="button" variant="outline" disabled={busy} onClick={() => void run("clear")}>Clear private browser data</Button></div>
    {!enabled && <p className="muted">Browser capability is disabled for this profile.</p>}
    <label>Session<select aria-label="Browser session" value={selected} onChange={(e) => { lease.current = null; setSession(null); setFrame(""); setSelected(e.target.value); }}><option value="">Select a session</option>{sessions.data?.items.map((item) => <option key={item.id} value={item.id}>{item.id} · {item.state}</option>)}</select></label>
    {session && <><p role="status">{session.state} · epoch {session.epoch} · frame {session.frameSeq}{session.leaseExpiresAt ? " · control expires " + new Date(session.leaseExpiresAt).toLocaleTimeString() : ""}</p>{session.lastError && <Notice>{session.lastError}</Notice>}
      <div className="automation-actions"><Button type="button" disabled={busy || session.state === "closed"} onClick={() => void run("takeover")}>Take over / renew</Button><Button type="button" variant="outline" disabled={busy || !lease.current} onClick={() => void run("release")}>Release control</Button><Button type="button" variant="ghost" disabled={busy || session.state === "closed"} onClick={() => void run("close")}>Close session</Button></div>
      {frame && <img className="automation-browser-frame" src={frame} alt="Current isolated browser frame" />}
      <form className="form-stack" onSubmit={(e) => void run("input", e)}><label>Action<select value={action} onChange={(e) => setAction(e.target.value)}>{["navigate", "open", "inspect", "screenshot", "click", "type", "key", "scroll", "upload", "switchTab"].map((name) => <option key={name}>{name}</option>)}</select></label>{["navigate", "open"].includes(action) && <label>Reviewed URL<input type="url" required value={url} onChange={(e) => setURL(e.target.value)} /></label>}{["click", "type", "upload"].includes(action) && <label>Element selector<input required maxLength={1024} value={selector} onChange={(e) => setSelector(e.target.value)} /></label>}{["type", "key", "upload", "switchTab"].includes(action) && <label>{action === "upload" ? "Conversation attachment ID" : action === "switchTab" ? "Tab index (zero based)" : "Text / key"}<input required maxLength={4096} value={text} onChange={(e) => setText(e.target.value)} /></label>}<Button type="submit" disabled={busy || !lease.current || session.state !== "taking_over"}>Send human input</Button></form>
      <details><summary>Accessible snapshot</summary><pre>{session.snapshotText || "No snapshot available."}</pre></details>
    </>}
  </section>;
}
