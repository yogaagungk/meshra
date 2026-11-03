import { useState } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./api";
import type { Task } from "./types";
import { automation, type PendingMemory, type SkillProposal } from "./automation-api";
import { goAutomation } from "./automationShared";
import { Notice } from "./App";
import { Button } from "./components/ui/button";
import { Markdown } from "./Markdown";

function Timestamp({ value }: { value: string }) {
  const date = new Date(value);
  return <time dateTime={value}>{Number.isNaN(date.getTime()) ? value : date.toLocaleString()}</time>;
}

function SourceTask({ conversationId, taskId, available = true }: { conversationId?: string; taskId?: string; available?: boolean }) {
  const source = useQuery({ queryKey: ["journey-source", taskId], queryFn: () => api<Task>("tasks/" + encodeURIComponent(taskId!)), enabled: !!taskId && !conversationId && available, retry: false });
  if (!taskId) return null;
  const sourceConversationId = conversationId || source.data?.conversationId;
  return sourceConversationId && available
    ? <a href={`#chat/${encodeURIComponent(sourceConversationId)}/${encodeURIComponent(taskId)}`}>Source task: {taskId}</a>
    : <span className="muted">Source task: {taskId}{!available || source.isError ? " (no longer available)" : ""}</span>;
}

function MemoryReview({ item }: { item: PendingMemory }) {
  const client = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [content, setContent] = useState(item.content);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function review(decision: "approve" | "reject") {
    setBusy(true); setError("");
    try {
      await automation("memory.pending.review", { profileId: item.profileId, id: item.id, decision, ...(decision === "approve" && content !== item.content ? { content } : {}) });
      await Promise.all([
        client.invalidateQueries({ queryKey: ["pending-memory", item.profileId] }),
        client.invalidateQueries({ queryKey: ["journey-memory", item.profileId] }),
        client.invalidateQueries({ queryKey: ["automation", "memory", item.profileId] }),
      ]);
    } catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  }
  return <li className="automation-card">
    <div className="automation-card-head"><strong>Pending memory{item.memoryId ? " update" : ""}</strong><Timestamp value={item.createdAt} /></div>
    <p className="muted">{item.pinned ? "Pinned · " : ""}<SourceTask conversationId={item.conversationId} taskId={item.sourceTaskId} /></p>
    {error && <Notice>{error}</Notice>}
    {editing ? <><label htmlFor={`memory-review-${item.id}`}>Memory content</label><textarea id={`memory-review-${item.id}`} rows={5} value={content} onChange={(e) => setContent(e.target.value)} /></> : <Markdown text={item.content} />}
    <div className="automation-actions">
      <Button size="sm" disabled={busy || !content.trim()} onClick={() => void review("approve")}>{editing ? "Save and approve" : "Approve"}</Button>
      {editing ? <Button size="sm" variant="outline" disabled={busy} onClick={() => { setContent(item.content); setEditing(false); }}>Cancel edit</Button> : <Button size="sm" variant="outline" disabled={busy} onClick={() => setEditing(true)}>Edit</Button>}
      <Button size="sm" variant="outline" disabled={busy} onClick={() => void review("reject")}>Reject</Button>
    </div>
  </li>;
}

function ProposalReview({ proposal }: { proposal: SkillProposal }) {
  const client = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function review(decision: "approve" | "reject") {
    setBusy(true); setError("");
    try {
      await automation("skills.proposals.review", { id: proposal.id, decision });
      await Promise.all([client.invalidateQueries({ queryKey: ["skill-proposals"] }), client.invalidateQueries({ queryKey: ["skills"] })]);
    } catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  }
  return <li className="automation-card">
    <div className="automation-card-head"><strong>{proposal.name}</strong><span>{proposal.status} · <Timestamp value={proposal.createdAt} /></span></div>
    <p>{proposal.description}</p>
    <p className="muted"><SourceTask conversationId={proposal.conversationId} taskId={proposal.taskId} />{proposal.basedOn && <> · Based on {proposal.basedOn.id} v{proposal.basedOn.version}</>}</p>
    {error && <Notice>{error}</Notice>}
    <details><summary>Instructions</summary><Markdown text={proposal.instructions} /></details>
    {proposal.status === "pending" && <div className="automation-actions"><Button size="sm" disabled={busy} onClick={() => void review("approve")}>Approve draft</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => void review("reject")}>Reject</Button></div>}
    {proposal.skillId && <Button size="sm" variant="outline" onClick={() => goAutomation("skills", proposal.skillId)}>Open skill</Button>}
  </li>;
}

export function SkillProposals({ pendingOnly = false, profileId }: { pendingOnly?: boolean; profileId?: string }) {
  const proposals = useInfiniteQuery({
    queryKey: ["skill-proposals"], initialPageParam: "",
    queryFn: ({ pageParam }) => automation("skills.proposals.list", { limit: 100, ...(pageParam ? { after: pageParam } : {}) }),
    getNextPageParam: (page) => page.hasMore ? page.nextAfter : undefined,
  });
  const items = (proposals.data?.pages.flatMap((page) => page.items) || []).filter((item) => (!pendingOnly || item.status === "pending") && (!profileId || item.profileId === profileId));
  return <section aria-label={pendingOnly ? "Skill proposals awaiting review" : "Skill proposals"}>
    <h2>{pendingOnly ? "Skill proposals awaiting review" : "Skill proposals"}</h2>
    <p className="muted">Proposals are inert until approved; approving creates an unbound skill that you can review and pin.</p>
    {proposals.isError && <Notice>{proposals.error.message}</Notice>}
    {proposals.isPending ? <p role="status">Loading proposals…</p> : !proposals.isError && !items.length && <p className="muted">No {pendingOnly ? "pending " : ""}skill proposals{profileId ? " for this profile" : ""}{proposals.hasNextPage ? " in loaded results" : ""}.</p>}
    <ul className="automation-node-list">{items.map((proposal) => <ProposalReview key={proposal.id} proposal={proposal} />)}</ul>
    {proposals.hasNextPage && <Button variant="outline" disabled={proposals.isFetchingNextPage} onClick={() => void proposals.fetchNextPage()}>{proposals.isFetchingNextPage ? "Loading…" : "Load more proposals"}</Button>}
  </section>;
}

export function JourneyPanel() {
  const profiles = useInfiniteQuery({
    queryKey: ["journey-profiles"], initialPageParam: "",
    queryFn: ({ pageParam }) => automation("profiles.list", { limit: 100, ...(pageParam ? { after: pageParam } : {}) }),
    getNextPageParam: (page) => page.hasMore ? page.nextAfter : undefined,
  });
  const profileItems = profiles.data?.pages.flatMap((page) => page.items) || [];
  const [selected, setSelected] = useState("");
  const profileId = selected || profileItems[0]?.id || "";
  const pending = useQuery({ queryKey: ["pending-memory", profileId], queryFn: () => automation("memory.pending.list", { profileId }), enabled: !!profileId });
  const memories = useQuery({ queryKey: ["journey-memory", profileId], queryFn: () => automation("memory.list", { profileId }), enabled: !!profileId });
  const learned = (memories.data?.items || []).filter((item) => item.sourceTaskId).sort((a, b) => b.updatedAt.localeCompare(a.updatedAt));
  return <div className="form-stack">
    <div><h1>Journey</h1><p className="muted">Review proposed memories and follow what this profile has learned from its tasks.</p></div>
    {profiles.isError && <Notice>{profiles.error.message}</Notice>}
    {profiles.isPending ? <p role="status">Loading profiles…</p> : !profileItems.length ? !profiles.isError && <p className="muted">Create a profile to see its journey.</p> : <>
      <label htmlFor="journey-profile">Profile</label>
      <select id="journey-profile" value={profileId} onChange={(e) => setSelected(e.target.value)}>{profileItems.map((profile) => <option key={profile.id} value={profile.id}>{profile.name}{profile.archived ? " (archived)" : ""}</option>)}</select>
      {profiles.hasNextPage && <Button variant="outline" disabled={profiles.isFetchingNextPage} onClick={() => void profiles.fetchNextPage()}>Load more profiles</Button>}
      <section aria-label="Pending memories"><h2>Pending memories</h2>
        {pending.isError && <Notice>{pending.error.message}</Notice>}
        {pending.isPending ? <p role="status">Loading pending memories…</p> : !pending.isError && !pending.data?.items.length && <p className="muted">No memories awaiting review.</p>}
        <ul className="automation-node-list">{pending.data?.items.map((item) => <MemoryReview key={item.id} item={item} />)}</ul>
      </section>
      <section aria-label="Learned memories"><h2>Learned memories</h2>
        {memories.isError && <Notice>{memories.error.message}</Notice>}
        {memories.isPending ? <p role="status">Loading memories…</p> : !memories.isError && !learned.length && <p className="muted">No task-sourced memories yet.</p>}
        <ul className="automation-node-list">{learned.map((item) => <li className="automation-card" key={item.id}><div className="automation-card-head"><strong>{item.scope} memory</strong><Timestamp value={item.updatedAt} /></div><Markdown text={item.content} /><SourceTask conversationId={item.conversationId} taskId={item.sourceTaskId} available={item.sourceAvailable !== false} /></li>)}</ul>
      </section>
      <SkillProposals profileId={profileId} />
    </>}
  </div>;
}
