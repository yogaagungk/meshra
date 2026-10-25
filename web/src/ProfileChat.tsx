import { useEffect, useRef, useState, type FormEvent } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, PanelRight, Plus } from "lucide-react";
import { api, collab } from "./api";
import { automation, type Memory } from "./automation-api";
import { go, Notice, Status } from "./App";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";
import { Markdown } from "./Markdown";
import { Approvals } from "./Approvals";
import { Attachments } from "./Attachments";
import { BrowserPanel } from "./BrowserPanel";
import { Voice } from "./Voice";
import { ContextBrowser } from "./Operations";
import { mergeTasks, stateLabel, terminal, type Agent, type Conversation, type Page } from "./types";

export function ProfileChat({ conversation, agents, onNew }: { conversation: Conversation; agents: Agent[]; onNew: () => void }) {
  const client = useQueryClient();
  const id = conversation.conversationId;
  const [recipient, setRecipient] = useState(conversation.profileIds?.length === 1 ? conversation.profileIds[0] : "");
  const [draft, setDraft] = useState("");
  const [attachmentIds, setAttachmentIds] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [side, setSide] = useState(false);
  const [context, setContext] = useState(false);
  const [tab, setTab] = useState("memory");
  const [connection, setConnection] = useState("Connecting");
  const key = useRef("");
  const profiles = useQuery({ queryKey: ["automation", "participants", id, conversation.profileIds], queryFn: () => Promise.all((conversation.profileIds || []).map((profileId) => automation("profiles.get", { id: profileId }))), gcTime: 0 });
  const selected = profiles.data?.find((profile) => profile.id === recipient);
  const binding = useQuery({ queryKey: ["automation", "binding", recipient, id], queryFn: () => automation("profiles.binding", { profileId: recipient, conversationId: id }), enabled: !!recipient, gcTime: 0 });
  const agent = agents.find((item) => item.id === binding.data?.agentId);
  const timeline = useInfiniteQuery({ queryKey: ["timeline", id], queryFn: ({ pageParam }) => api<Page>("timeline?conversationId=" + encodeURIComponent(id) + "&limit=20&before=" + pageParam), initialPageParam: 0, getNextPageParam: (page) => page.hasMore ? page.nextBefore : undefined, maxPages: 1, gcTime: 0 });
  const tasks = mergeTasks(timeline.data?.pages || []);
  useEffect(() => {
    let timer: number | undefined;
    const source = new EventSource("/console/api/events?conversationId=" + encodeURIComponent(id) + "&after=0");
    const refresh = () => {
      if (timer !== undefined) return;
      timer = window.setTimeout(() => { timer = undefined; void client.invalidateQueries({ queryKey: ["timeline", id] }, { cancelRefetch: false }); void client.invalidateQueries({ queryKey: ["inbox"] }); void client.invalidateQueries({ queryKey: ["automation", "binding"] }); }, 400);
    };
    source.onopen = () => setConnection("Live"); source.onmessage = refresh;
    source.addEventListener("resync", refresh);
    source.addEventListener("expired", () => { source.close(); window.dispatchEvent(new Event("meshra:expired")); });
    source.onerror = () => setConnection(navigator.onLine ? "Reconnecting" : "Offline");
    return () => { source.close(); window.clearTimeout(timer); };
  }, [id, client]);
  async function send(event: FormEvent) {
    event.preventDefault(); if (!recipient || !draft.trim() || busy) return;
    setBusy(true); setError(""); key.current ||= crypto.randomUUID();
    try {
      await automation("profiles.send", { id: recipient, conversationId: id, text: draft, attachmentIds, idempotencyKey: key.current });
      key.current = ""; setDraft(""); setAttachmentIds([]);
      await client.resetQueries({ queryKey: ["timeline", id] }); await client.invalidateQueries({ queryKey: ["inbox"] });
    } catch (e) { setError((e as Error).message + " Retry preserves the admission key."); }
    finally { setBusy(false); }
  }
  async function bindingAction(action: "start" | "stop") {
    setBusy(true); setError("");
    try { await automation("profiles.bindingAction", { profileId: recipient, conversationId: id, action }); await client.invalidateQueries({ queryKey: ["agents"] }); await binding.refetch(); }
    catch (e) { setError((e as Error).message); } finally { setBusy(false); }
  }
  const latestReply = [...tasks].reverse().find((task) => task.state === "completed" && task.result)?.result || "";
  return <div className={"chat-layout profile-chat " + (side ? "inspector-open" : "")}><section className="chat-main">
    <header className="chat-header"><div className="chat-title-row"><Button variant="ghost" size="icon" className="mobile-back" aria-label="Back to conversations" onClick={() => go("chat")}><ArrowLeft size={20} /></Button><div><h1>{conversation.title}</h1><div className="chat-subtitle">Profile {conversation.profileKind || "conversation"} · {connection} · queued sends</div></div></div><div className="chat-header-actions"><Button variant="ghost" onClick={onNew}><Plus size={16} />New conversation</Button><Button variant={side ? "outline" : "ghost"} aria-label="Profile conversation tools" onClick={() => setSide(!side)}><PanelRight size={18} /></Button></div></header>
    <div className="profile-binding-bar"><label htmlFor="profile-recipient">Recipient / channel lead</label><select id="profile-recipient" value={recipient} disabled={busy} onChange={(e) => { setRecipient(e.target.value); setAttachmentIds([]); key.current = ""; }}><option value="">Select one recipient</option>{profiles.data?.map((profile) => <option key={profile.id} value={profile.id}>{profile.name}</option>)}</select>
      {selected && <><span>{binding.data?.harness || selected.binding.harness} · {binding.data?.config.model || selected.binding.config.model || "Configured default"}</span><Status state={agent?.state || "unavailable"} /><Button variant="outline" size="sm" disabled={busy} onClick={() => void bindingAction("start")}>Start binding</Button><Button variant="outline" size="sm" disabled={busy} onClick={() => void bindingAction("stop")}>Stop binding</Button>{binding.data && <small className="muted">Binding {binding.data.id} · generation {binding.data.generation}</small>}</>}
    </div>
    {error && <Notice>{error}</Notice>}{profiles.isError && <Notice>{profiles.error.message}</Notice>}{binding.isError && <Notice>{binding.error.message}</Notice>}
    <div className="chat-timeline"><div className="timeline-inner">
      {timeline.isError && <Notice>{timeline.error.message}</Notice>}{timeline.hasNextPage && <Button variant="outline" disabled={timeline.isFetchingNextPage} onClick={() => void timeline.fetchNextPage()}>Older messages</Button>}{timeline.data?.pageParams[0] !== 0 && <Button variant="outline" onClick={() => void client.resetQueries({ queryKey: ["timeline", id] })}>Latest messages</Button>}
      {tasks.map((task) => <article className="automation-card" key={task.taskId}><header className="automation-card-head"><strong>{profiles.data?.find((profile) => profile.id === task.to)?.name || task.to}</strong><span>{stateLabel(task)}</span></header><Markdown text={task.text} />{task.result && <Markdown text={task.result} />}{task.error && <Notice>{task.error}</Notice>}{!terminal(task.state) && <Button variant="outline" onClick={() => { void api("tasks/" + encodeURIComponent(task.taskId) + "/cancel", {}).then(() => client.invalidateQueries({ queryKey: ["timeline", id] })).catch((e: Error) => setError(e.message)); }}>Cancel task</Button>}{!!task.attachmentIds?.length && <small className="muted">Files: {task.attachmentIds.join(", ")}</small>}</article>)}
      {!timeline.isPending && !tasks.length && <p className="muted">Choose a recipient and queue your first message. Starting a binding is explicit; stopped automatic work stays queued.</p>}
    </div></div>
    <form className="profile-composer form-stack" onSubmit={send}><label htmlFor="profile-message">Queue a message{selected ? " to " + selected.name : ""}</label><textarea id="profile-message" value={draft} disabled={busy} placeholder="Messages can queue while a task is running…" onChange={(e) => { setDraft(e.target.value); key.current = ""; }} /><div className="automation-actions"><Button type="button" variant="outline" onClick={() => { setTab("files"); setSide(true); }}>Files ({attachmentIds.length})</Button><Button type="button" variant="outline" onClick={() => { setTab("voice"); setSide(true); }}>Voice draft</Button><Button type="submit" disabled={busy || !recipient || !draft.trim() || new TextEncoder().encode(draft).length > 65536}>{busy ? "Queuing…" : "Queue message"}</Button></div></form>
  </section>{side && <aside className="chat-inspector profile-inspector"><div className="automation-tabs" role="group" aria-label="Profile conversation tools">{["memory", "history", "context", "files", "attention", "browser", "voice"].map((name) => <button key={name} aria-pressed={tab === name} onClick={() => setTab(name)}>{name}</button>)}</div>
    {!recipient ? <p>Select a recipient to inspect its private tools and memory.</p>
      : tab === "memory" ? <ProfileMemory key={recipient} profileId={recipient} conversationId={id} />
      : tab === "history" ? <ProfileHistory key={recipient} profileId={recipient} conversationId={id} />
      : tab === "context" ? <section className="automation-card">
        <strong>Context and reset</strong>
        <p>Only explicitly attached shared contexts enter this conversation. Membership does not grant access.</p>
        <ul>{conversation.contextRefs.map((ref) => <li key={ref.contextId + ":" + ref.version}>{ref.contextId} · v{ref.version}</li>)}</ul>
        <Button variant="outline" onClick={() => setContext(true)}>Browse shared context</Button>
        <Button variant="outline" disabled={!selected || busy} onClick={() => {
          if (!selected || !window.confirm("Reset future context for this binding? Existing history and saved memory remain.")) return;
          setBusy(true);
          void automation("profiles.resetContext", { id: recipient, expectedRevision: selected.revision, conversationId: id }).then(() => binding.refetch()).catch((e: Error) => setError(e.message)).finally(() => setBusy(false));
        }}>Reset future context</Button>
      </section>
      : tab === "files" ? <Attachments key={recipient} conversationId={id} selected={attachmentIds} enabled={!!selected?.capabilities.attachments} onSelect={(ids) => { setAttachmentIds(ids); key.current = ""; }} />
      : tab === "attention" ? <Approvals key={recipient} profileId={recipient} />
      : tab === "browser" ? <BrowserPanel key={recipient} profileId={recipient} conversationId={id} enabled={!!selected?.capabilities.browser} />
      : <Voice key={recipient} conversationId={id} reply={latestReply} onDraft={(text) => { setDraft((value) => value + (value ? "\n" : "") + text); key.current = ""; }} />}
  </aside>}
  <Dialog open={context} onOpenChange={setContext} title="Attach shared context" description="Select an authorized immutable context version to share with this profile conversation." wide>
    <ContextBrowser recipients={binding.data ? [binding.data.agentId] : []} onAttach={(ref) => {
      void collab("context.attach", { conversationId: id, ...ref }).then(() => { setContext(false); return client.invalidateQueries({ queryKey: ["conversation", id] }); }).catch((e: Error) => setError(e.message));
    }} />
  </Dialog></div>;
}

function ProfileMemory({ profileId, conversationId }: { profileId: string; conversationId: string }) {
  const [editing, setEditing] = useState<Memory | null>(null);
  const [content, setContent] = useState("");
  const [scope, setScope] = useState<"operator" | "conversation">("conversation");
  const [pinned, setPinned] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const memories = useQuery({ queryKey: ["automation", "memory", profileId, conversationId], queryFn: () => automation("memory.list", { profileId, conversationId }), gcTime: 0 });
  async function save(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError("");
    try { await automation("memory.save", { profileId, conversationId: scope === "conversation" ? conversationId : undefined, scope, content, pinned, id: editing?.id, expectedRevision: editing?.revision }); setEditing(null); setContent(""); await memories.refetch(); }
    catch (e) { setError((e as Error).message); } finally { setBusy(false); }
  }
  return <section className="automation-card"><strong>Profile memory</strong><p className="muted">Quoted data, never policy. Operator-only memories are not inherited by external conversations.</p>{error && <Notice>{error}</Notice>}{memories.isError && <Notice>{memories.error.message}</Notice>}<ul className="automation-task-list">{memories.data?.items.map((memory) => <li key={memory.id}><div><small>{memory.scope} · revision {memory.revision}{memory.pinned ? " · pinned" : ""}</small><Markdown text={memory.content} />{memory.sourceTaskId && <small>Source: {memory.sourceAvailable === false ? "unavailable" : memory.sourceTaskId}</small>}<div className="automation-actions"><Button variant="outline" onClick={() => { setEditing(memory); setContent(memory.content); setScope(memory.scope); setPinned(memory.pinned); }}>Edit</Button><Button variant="ghost" onClick={() => { if (!window.confirm("Delete this memory?")) return; void automation("memory.remove", { profileId, id: memory.id, expectedRevision: memory.revision }).then(() => memories.refetch()).catch((e: Error) => setError(e.message)); }}>Delete</Button></div></div></li>)}</ul><form className="form-stack" onSubmit={save}><label>Memory<textarea required value={content} onChange={(e) => setContent(e.target.value)} /></label><label>Scope<select value={scope} onChange={(e) => setScope(e.target.value as "operator" | "conversation")}><option value="conversation">This conversation</option><option value="operator">Operator only</option></select></label><label><input type="checkbox" checked={pinned} onChange={(e) => setPinned(e.target.checked)} /> Pinned</label><div className="automation-actions"><Button disabled={busy || !content.trim()}>{editing ? "Save memory revision" : "Add memory"}</Button>{editing && <Button type="button" variant="ghost" onClick={() => { setEditing(null); setContent(""); }}>Cancel edit</Button>}</div></form></section>;
}
function ProfileHistory({ profileId, conversationId }: { profileId: string; conversationId: string }) {
  const [query, setQuery] = useState(""); const [search, setSearch] = useState(""); const [taskId, setTaskId] = useState(""); const [before, setBefore] = useState<number | undefined>();
  const hits = useQuery({ queryKey: ["automation", "history-search", profileId, conversationId, search], queryFn: () => automation("history.search", { profileId, conversationId, query: search, limit: 50 }), enabled: !!search, gcTime: 0 });
  const history = useQuery({ queryKey: ["automation", "history", profileId, conversationId, taskId, before], queryFn: () => automation("history.read", { profileId, conversationId, taskId: taskId || undefined, before, limit: 20 }), gcTime: 0 });
  return <section className="automation-card"><strong>Profile history</strong><form className="form-stack" onSubmit={(e) => { e.preventDefault(); setSearch(query); }}><input aria-label="Search profile history" value={query} onChange={(e) => setQuery(e.target.value)} /><Button type="submit">Search</Button></form>{hits.isError && <Notice>{hits.error.message}</Notice>}{hits.data?.items.map((hit) => <button className="text-button" key={hit.taskId} onClick={() => { setTaskId(hit.taskId); setBefore(undefined); }}>{hit.excerpt}</button>)}{history.isError && <Notice>{history.error.message}</Notice>}{history.data?.items.map((exchange) => <article key={exchange.taskId}><small>{exchange.taskId}</small><Markdown text={exchange.text} />{exchange.result && <Markdown text={exchange.result} />}</article>)}<div className="automation-actions"><Button variant="outline" disabled={!history.data?.hasMore} onClick={() => setBefore(history.data?.nextBefore)}>Older history</Button><Button variant="ghost" onClick={() => { setTaskId(""); setBefore(undefined); setSearch(""); }}>Latest history</Button></div></section>;
}
