import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowLeft,
  ArrowUpRight,
  BookOpen,
  Check,
  ChevronDown,
  ChevronRight,
  Clock,
  Copy,
  FileText,
  Info,
  MessageSquare,
  MoreHorizontal,
  PanelRight,
  Paperclip,
  Plus,
  Send,
  ShieldCheck,
  Square,
  RotateCcw,
  Route,
  Users,
  UserRound,
  X,
} from "lucide-react";
import { ChatAIStatus } from "./AIStatus";
import { api, collab, draftKey, localRead, localWrite } from "./api";
import { Harness, Status, Empty, Notice, go } from "./App";
import { AgentAvatar } from "./components/AgentAvatar";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";
import { Markdown } from "./Markdown";
import { ContextBrowser } from "./Operations";
import { harnessName, deptOf } from "./HarnessLogo";
import {
  mergeTasks,
  stateLabel,
  terminal,
  type Agent,
  type Conversation,
  type Page,
  type Ref,
  type SharedContext,
  type Task,
} from "./types";
import { ProfileChat } from "./ProfileChat";
import { ChatQueue, RetryWith, RoutingControl } from "./ChatCoordination";
import {
  readRouteNotes,
  readRouting,
  routeWithLead,
  writeRouteNote,
  type RoutedNote,
} from "./leadRouting";
import {
  applyMention,
  handle,
  mentionOptions,
  mentionQuery,
  reasonText,
  resolveRoute,
} from "./routing";
export function Chat(props: { id: string; agents: Agent[]; onNew: () => void }) {
  const conversation = useQuery({
    queryKey: ["conversation", props.id],
    queryFn: () => collab<Conversation>("conversation.get", { id: props.id }),
    gcTime: 0,
  });
  if (conversation.data?.profileIds?.length)
    return <ProfileChat key={props.id} conversation={conversation.data} agents={props.agents} onNew={props.onNew} />;
  return <LegacyChat {...props} />;
}
function LegacyChat({
  id,
  agents,
  onNew,
}: {
  id: string;
  agents: Agent[];
  onNew: () => void;
}) {
  const client = useQueryClient();
  const [draft, setDraft] = useState(() => localRead(draftKey(id), ""));
  const [targets, setTargets] = useState<string[]>(() =>
    localRead("targets:" + id, []),
  );
  const [refs, setRefs] = useState<Ref[]>(() =>
    localRead("draft-refs:" + id, []),
  );
  const [inspector, setInspector] = useState(false);
  const [attach, setAttach] = useState(false);
  const [participants, setParticipants] = useState(false);
  const [sending, setSending] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [regenerating, setRegenerating] = useState(false);
  const [error, setError] = useState("");
  const [connection, setConnection] = useState("Connecting");
  const [jump, setJump] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [title, setTitle] = useState("");
  const [sendKey] = useState(() => localRead("send-key", "enter"));
  const [seen, setSeen] = useState(false);
  const pendingKey = useRef<string>(localRead("pending-send:" + id, ""));
  const scroller = useRef<HTMLDivElement>(null);
  const nearBottom = useRef(true);
  const restoredScroll = useRef(false);
  const input = useRef<HTMLTextAreaElement>(null);
  const conversation = useQuery({
    queryKey: ["conversation", id],
    gcTime: 0,
    queryFn: () => collab<Conversation>("conversation.get", { id }),
  });
  const isChat = conversation.data?.mode === "chat";
  const chatState = useQuery({
    queryKey: ["chat-state", id],
    queryFn: () =>
      api<{ taskId: string; state: string; canRegenerate: boolean }>(
        "chat-state?conversationId=" + encodeURIComponent(id),
      ),
    enabled: isChat,
    gcTime: 0,
  });
  const timeline = useInfiniteQuery({
    queryKey: ["timeline", id],
    initialPageParam: 0,
    queryFn: ({ pageParam }) =>
      api<Page>(
        "timeline?conversationId=" +
          encodeURIComponent(id) +
          "&limit=20&before=" +
          pageParam,
      ),
    getNextPageParam: (last) => (last.hasMore ? last.nextBefore : undefined),
    maxPages: 1,
    gcTime: 0,
  });
  const tasks = mergeTasks(timeline.data?.pages || []);
  const active = isChat
    ? !!chatState.data?.state && !terminal(chatState.data.state)
    : tasks.some((t) => !terminal(t.state));
  const members = agents.filter((a) =>
    conversation.data?.participants.includes(a.id),
  );
  const [routing, setRouting] = useState(() => readRouting(id));
  const [routeNotes, setRouteNotes] = useState(() => readRouteNotes(id));
  const [routingBusy, setRoutingBusy] = useState(false);
  const [caret, setCaret] = useState(0);
  const [mentionIndex, setMentionIndex] = useState(0);
  const route = resolveRoute({ draft, members, selected: targets, tasks });
  const sendIds = isChat ? targets : route.ids;
  const leadRoutes =
    !isChat &&
    !!routing.lead &&
    members.length > 1 &&
    members.some((m) => m.id === routing.lead && m.capabilities?.toolFreeDraft);
  const canLeadRoute = leadRoutes && !sendIds.length && !!draft.trim();
  const open = !isChat ? mentionQuery(draft, caret) : null;
  const options = open ? mentionOptions(open.query, members) : [];
  function pickMention(member: { id: string; name: string }) {
    if (!open) return;
    const next = applyMention(draft, caret, open.start, member);
    setDraft(next.text);
    pendingKey.current = "";
    localWrite("pending-send:" + id, "");
    requestAnimationFrame(() => {
      input.current?.focus();
      input.current?.setSelectionRange(next.caret, next.caret);
      setCaret(next.caret);
    });
  }
  useEffect(() => {
    if (!conversation.data) return;
    setTargets((previous) => {
      const valid = previous.filter((value) =>
        conversation.data!.participants.includes(value),
      );
      if (valid.length !== previous.length) {
        pendingKey.current = "";
        localWrite("pending-send:" + id, "");
        return valid;
      }
      if (
        !seen &&
        !previous.length &&
        conversation.data!.participants.length === 1
      )
        return [conversation.data!.participants[0]];
      return previous;
    });
    setSeen(true);
  }, [conversation.data, seen, id]);
  useEffect(() => {
    localWrite(draftKey(id), draft);
  }, [draft, id]);
  useEffect(() => {
    localWrite("targets:" + id, targets);
    localWrite("draft-refs:" + id, refs);
  }, [targets, refs, id]);
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    let expired = false;
    const key = "event-cursor:" + id;
    const saved = localRead<number>(key, 0);
    const source = new EventSource(
      "/console/api/events?conversationId=" +
        encodeURIComponent(id) +
        "&after=" +
        saved,
    );
    const refresh = () => {
      if (timer) return;
      timer = setTimeout(() => {
        timer = undefined;
        // Live refresh must not cancel a page the reader is opening.
        client.invalidateQueries(
          { queryKey: ["timeline", id] },
          { cancelRefetch: false },
        );
        client.invalidateQueries({ queryKey: ["chat-state", id] });
        client.invalidateQueries({ queryKey: ["inbox"] });
        client.invalidateQueries({ queryKey: ["task-detail"] });
        if (!nearBottom.current) setJump(true);
      }, 500);
    };
    source.onopen = () => setConnection("Live");
    source.onmessage = (e) => {
      const value = JSON.parse(e.data);
      localWrite(key, value.id);
      refresh();
    };
    source.addEventListener("resync", () => {
      setConnection("Live");
      refresh();
    });
    source.addEventListener("expired", () => {
      expired = true;
      source.close();
      window.dispatchEvent(new Event("meshra:expired"));
    });
    source.onerror = () => {
      if (!expired)
        setConnection(navigator.onLine ? "Reconnecting" : "Offline");
    };
    return () => {
      source.close();
      if (timer) clearTimeout(timer);
    };
  }, [id, client]);
  useEffect(() => {
    const element = scroller.current;
    if (!element || !timeline.data) return;
    localWrite("last-viewed:" + id, Date.now());
    if (!restoredScroll.current) {
      const saved = localRead<number>("scroll:" + id, -1);
      element.scrollTop =
        saved >= 0
          ? Math.min(
              saved,
              Math.max(0, element.scrollHeight - element.clientHeight),
            )
          : element.scrollHeight;
      nearBottom.current =
        element.scrollHeight - element.scrollTop - element.clientHeight < 100;
      restoredScroll.current = true;
      return;
    }
    if (nearBottom.current) element.scrollTop = element.scrollHeight;
    else if (active) setJump(true);
  }, [timeline.data, active, id]);
  async function send(e?: FormEvent) {
    e?.preventDefault();
    if (
      sending ||
      !draft.trim() ||
      (isChat && active) ||
      (!sendIds.length && !canLeadRoute) ||
      new TextEncoder().encode(draft).length > 65536
    )
      return;
    setSending(true);
    setError("");
    pendingKey.current ||= crypto.randomUUID();
    localWrite("pending-send:" + id, pendingKey.current);
    try {
      if (isChat && members[0]) {
        const ai = await api<Agent>(
          "agents/" + encodeURIComponent(members[0].id) + "/start",
          {},
        );
        if (ai.state === "error")
          throw new Error("Reconnect the AI before sending another message.");
        client.invalidateQueries({ queryKey: ["agents"] });
      }
      let recipients = sendIds;
      let note: RoutedNote | undefined;
      if (!recipients.length) {
        setRoutingBusy(true);
        try {
          const picked = await routeWithLead({
            conversationId: id,
            lead: routing.lead,
            text: draft,
            purpose: routing.purpose,
          });
          if (!picked.ids.length)
            throw new Error(
              "The lead did not pick a recipient. Type @ to address an agent, or choose one below.",
            );
          recipients = picked.ids;
          note = { by: routing.lead, reason: picked.reason, ids: picked.ids };
        } finally {
          setRoutingBusy(false);
        }
      }
      const result = await api<{
        tasks: Task[];
        errors: Record<string, string>;
      }>("send", {
        conversationId: id,
        text: draft,
        recipients,
        contextRefs: refs,
        idempotencyKey: pendingKey.current,
      });
      if (note) {
        for (const sent of result.tasks) writeRouteNote(id, sent.messageId, note);
        setRouteNotes(readRouteNotes(id));
      }
      const failures = Object.entries(result.errors);
      if (failures.length) {
        setError(failures.map(([id, error]) => id + ": " + error).join("\n"));
        setTargets(failures.map(([id]) => id));
      } else {
        setDraft("");
        setRefs([]);
        pendingKey.current = "";
        localWrite("pending-send:" + id, "");
        nearBottom.current = true;
        input.current?.focus();
      }
      if (timeline.data?.pageParams[0] !== 0) {
        await client.resetQueries({ queryKey: ["timeline", id] });
      } else {
        await client.invalidateQueries({ queryKey: ["timeline", id] });
      }
      await client.invalidateQueries({ queryKey: ["chat-state", id] });
      await client.invalidateQueries({ queryKey: ["inbox"] });
    } catch (e) {
      setError(
        (e as Error).message +
          " · Retry send keeps the same message key to avoid duplicate admission.",
      );
    } finally {
      setSending(false);
    }
  }
  async function stopReply() {
    setStopping(true);
    setError("");
    try {
      await Promise.all(
        (isChat && chatState.data
          ? [chatState.data]
          : tasks.filter((t) => !terminal(t.state))
        ).map((t) =>
          api("tasks/" + encodeURIComponent(t.taskId) + "/cancel", {}),
        ),
      );
      await client.invalidateQueries({ queryKey: ["timeline", id] });
      await client.invalidateQueries({ queryKey: ["chat-state", id] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setStopping(false);
    }
  }
  async function regenerateReply(taskId: string) {
    setRegenerating(true);
    setError("");
    try {
      await api("regenerate", { taskId });
      nearBottom.current = true;
      await Promise.all([
        client.invalidateQueries({ queryKey: ["timeline", id] }),
        client.invalidateQueries({ queryKey: ["chat-state", id] }),
      ]);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setRegenerating(false);
    }
  }
  async function loadOlder() {
    const element = scroller.current;
    const oldHeight = element?.scrollHeight || 0;
    await timeline.fetchNextPage();
    requestAnimationFrame(() => {
      if (element) element.scrollTop += element.scrollHeight - oldHeight;
    });
  }
  async function rename(e: FormEvent) {
    e.preventDefault();
    try {
      await collab("conversation.rename", { id, title });
      setRenaming(false);
      client.invalidateQueries({ queryKey: ["conversation", id] });
      client.invalidateQueries({ queryKey: ["inbox"] });
    } catch (e) {
      setError((e as Error).message);
    }
  }
  function showConversation() {
    setInspector(true);
  }
  if (conversation.isError)
    return (
      <Empty
        icon={MessageSquare}
        title="Conversation unavailable"
        action={
          <Button onClick={() => go("chat")}>Back to conversations</Button>
        }
      >
        {conversation.error.message}
      </Empty>
    );
  return (
    <div
      className={
        "chat-layout " +
        (isChat ? "direct-chat " : "") +
        (inspector ? "inspector-open" : "")
      }
    >
      <section className="chat-main">
        <header className="chat-header">
          <div className="chat-title-row">
            <Button
              variant="ghost"
              size="icon"
              className="mobile-back"
              aria-label="Back to conversations"
              onClick={() => go("chat")}
            >
              <ArrowLeft size={20} />
            </Button>
            <span className="chat-header-icon">
              <MessageSquare size={20} />
            </span>
            <div>
              <button
                className="title-button"
                onClick={() => {
                  setTitle(conversation.data?.title || "");
                  setRenaming(true);
                }}
              >
                <h1>{conversation.data?.title || "Loading conversation…"}</h1>
                <ChevronDown size={14} />
              </button>
              <div className="chat-subtitle">
                <span
                  className={
                    "status-dot " + (connection === "Live" ? "" : "offline-dot")
                  }
                  role="img"
                  aria-label={
                    connection === "Live" ? "Connected to local relay" : connection
                  }
                  title={
                    connection === "Live" ? "Connected to local relay" : connection
                  }
                />
                <span>
                  {isChat
                    ? harnessName(
                        members[0]?.id ||
                          conversation.data?.participants[0] ||
                          "AI",
                      )
                    : `${conversation.data?.participants.length ?? 0} participants`}
                </span>
              </div>
            </div>
          </div>
          <div className="chat-header-actions">
            {isChat && conversation.data && (
              <ChatAIStatus
                conversationId={id}
                harness={
                  conversation.data.participants[0]?.split(":")[0] || "codex"
                }
                revision={chatState.data?.state || ""}
              />
            )}
            <Button
              variant="ghost"
              size="sm"
              aria-label="New chat"
              onClick={onNew}
            >
              <Plus size={16} />{" "}
              <span className="new-chat-label">New chat</span>
            </Button>
            {!isChat && (
              <Button
                variant="ghost"
                size="icon"
                aria-label="Manage participants"
                onClick={() => setParticipants(true)}
              >
                <Users size={18} />
              </Button>
            )}
            <Button
              variant={inspector ? "outline" : "ghost"}
              size="icon"
              aria-label="Toggle conversation inspector"
              aria-pressed={inspector}
              onClick={() => setInspector(!inspector)}
            >
              <PanelRight size={18} />
            </Button>
          </div>
        </header>
        {!isChat && (
          <div className="participant-strip">
            <span className="muted">Participants</span>
            {members.map((a) => (
              <button
                key={a.id}
                onClick={() => setParticipants(true)}
                title={a.id + " · " + a.cwd + " · " + a.state}
              >
                <AgentAvatar icon={a.avatarIcon} color={a.avatarColor} size={28} />
                <span>{a.id}</span>
                <span className={"status-dot state-" + a.state} />
                <span className="sr-only">{a.state}</span>
              </button>
            ))}
            <Button
              variant="ghost"
              size="icon"
              aria-label="Add participant"
              onClick={() => setParticipants(true)}
            >
              <Plus size={15} />
            </Button>
          </div>
        )}
        {connection !== "Live" && (
          <div className="connection-banner" role="status">
            <Clock size={14} />
            {connection === "Connecting"
              ? "Opening the event stream…"
              : connection === "Offline"
                ? "You are offline. Your draft is saved locally."
                : "Reconnecting to the relay. Saved messages remain available; replay resumes automatically."}
          </div>
        )}
        <div
          className="message-scroll"
          role="log"
          aria-label="Conversation messages"
          aria-live="polite"
          aria-relevant="additions text"
          tabIndex={0}
          ref={scroller}
          onScroll={(e) => {
            const el = e.currentTarget;
            nearBottom.current =
              el.scrollHeight - el.scrollTop - el.clientHeight < 100;
            localWrite("scroll:" + id, el.scrollTop);
            if (nearBottom.current) setJump(false);
          }}
        >
          <div className="timeline-inner">
            {timeline.data && timeline.data.pageParams[0] !== 0 && (
              <div className="older-messages">
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    nearBottom.current = true;
                    client.resetQueries({ queryKey: ["timeline", id] });
                  }}
                >
                  Back to latest messages
                </Button>
              </div>
            )}
            {timeline.hasNextPage && (
              <div className="older-messages">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={timeline.isFetchingNextPage}
                  onClick={() => void loadOlder()}
                >
                  {timeline.isFetchingNextPage
                    ? "Loading…"
                    : "Load older messages"}
                </Button>
              </div>
            )}
            {timeline.isLoading ? (
              <div className="timeline-loading" role="status">
                <span className="skeleton" />
                <span className="skeleton short" />
                <p>Loading saved messages…</p>
              </div>
            ) : timeline.isError ? (
              <Notice>
                {timeline.error.message}
                <Button variant="outline" onClick={() => timeline.refetch()}>
                  Try again
                </Button>
              </Notice>
            ) : tasks.length === 0 ? (
              <div className="conversation-start">
                <span className="empty-icon">
                  <MessageSquare size={28} strokeWidth={1.4} />
                </span>
                <h2>
                  {isChat
                    ? "What can I help with?"
                    : "A little context goes a long way."}
                </h2>
                <p>
                  {isChat ? (
                    "Ask a question, explore an idea, or work through a problem together."
                  ) : (
                    <>
                      Tell your agents what you’re working on.
                      <br />
                      Type @ to address an agent, or just write. Meshra shows who
                      receives it.
                    </>
                  )}
                </p>
                <div className="conversation-start-meta">
                  <ShieldCheck size={14} />
                  {isChat
                    ? "Your conversation is saved locally."
                    : "Nothing is broadcast automatically."}
                </div>
              </div>
            ) : (
              tasks.map((t) => (
                <div
                  className="message-group"
                  key={t.taskId}
                  data-task-id={t.taskId}
                >
                  <div className="message message-request">
                    <div className="message-avatar human-avatar">
                      {t.from === "human:operator" ? (
                        <UserRound size={14} />
                      ) : (
                        <ChatAgentAvatar id={t.from} agents={agents} />
                      )}
                    </div>
                    <div className="message-body">
                      <div className="message-heading">
                        {t.from !== "human:operator" && <strong>{t.from}</strong>}
                        {!isChat && (
                          <span className="message-target">
                            → <code>{t.to}</code>
                          </span>
                        )}
                        <time dateTime={t.createdAt}>
                          {formatTime(t.createdAt)}
                        </time>
                      </div>
                      <Markdown text={t.text} />
                      {routeNotes[t.messageId] && (
                        <p className="route-note">
                          <Route size={12} />
                          Routed by <code>{routeNotes[t.messageId].by}</code>
                          {routeNotes[t.messageId].reason
                            ? ": " + routeNotes[t.messageId].reason
                            : ""}
                        </p>
                      )}
                      {!!t.contextRefs?.length && (
                        <div className="message-refs">
                          {t.contextRefs.map((ref) => (
                            <button
                              key={ref.contextId + ref.version}
                              onClick={() => {
                                showConversation();
                              }}
                            >
                              <Paperclip size={12} />
                              {ref.contextId.slice(0, 10)} · v{ref.version}
                            </button>
                          ))}
                        </div>
                      )}
                      <div className="message-meta">
                        <Delivery task={t} />
                      </div>
                    </div>
                  </div>
                  <div className="message message-response" data-harness={deptOf(t.to)}>
                    <div className="message-avatar">
                      <ChatAgentAvatar id={t.to} agents={agents} />
                    </div>
                    <div className="message-body">
                      <div className="message-heading">
                        <strong className={isChat ? "" : "mono"}>
                          {isChat ? harnessName(t.to) : t.to}
                        </strong>
                        <span className="harness-label">
                          {t.to.split(":")[0]}
                        </span>
                        <time dateTime={t.updatedAt}>
                          {formatTime(t.updatedAt)}
                        </time>
                      </div>
                      {t.result ? (
                        <Markdown text={t.result} />
                      ) : !terminal(t.state) ? (
                        <Thinking state={t.state} />
                      ) : (
                        <div
                          className={
                            "execution-state " +
                            (t.ambiguous ? "uncertain" : "failure")
                          }
                        >
                          <Info size={17} />
                          <div>
                            <strong>{stateLabel(t)}</strong>
                            <p>{t.error || "No response text was returned."}</p>
                            {t.ambiguous && (
                              <p>
                                The response was interrupted. You can send
                                another message.
                              </p>
                            )}
                            {!isChat && (
                              <RetryWith
                                task={t}
                                members={members}
                                onSent={async () => {
                                  nearBottom.current = true;
                                  await client.invalidateQueries({
                                    queryKey: ["timeline", id],
                                  });
                                }}
                              />
                            )}
                          </div>
                        </div>
                      )}
                      <div className="response-tools">
                        {t.result && <CopyButton text={t.result} />}
                        {isChat &&
                          chatState.data?.taskId === t.taskId &&
                          chatState.data.canRegenerate && (
                            <button
                              disabled={regenerating || sending}
                              onClick={() => void regenerateReply(t.taskId)}
                            >
                              <RotateCcw size={13} />{" "}
                              {regenerating
                                ? "Regenerating…"
                                : "Regenerate response"}
                            </button>
                          )}
                      </div>
                    </div>
                  </div>
                </div>
              ))
            )}
          </div>
        </div>
        {jump && (
          <Button
            className="jump-button"
            variant="outline"
            size="sm"
            onClick={() => {
              nearBottom.current = true;
              setJump(false);
              client.resetQueries({ queryKey: ["timeline", id] });
              scroller.current?.scrollTo({
                top: scroller.current.scrollHeight,
                behavior: matchMedia("(prefers-reduced-motion: reduce)").matches
                  ? "instant"
                  : "smooth",
              });
            }}
          >
            <ArrowDown size={14} />
            New messages · jump to latest
          </Button>
        )}
        {!isChat && <ChatQueue conversationId={id} members={members} />}
        <div className="composer-area">
          {error && (
            <Notice>
              {error}
              <button
                className="dismiss-error"
                aria-label="Dismiss send error"
                onClick={() => setError("")}
              >
                <X size={15} />
              </button>
            </Notice>
          )}
          {isChat && members[0]?.state === "error" && (
            <Notice>
              This AI needs to reconnect.
              <Button
                variant="outline"
                onClick={async () => {
                  try {
                    await api(
                      "agents/" +
                        encodeURIComponent(members[0].id) +
                        "/restart",
                      {},
                    );
                    client.invalidateQueries({ queryKey: ["agents"] });
                  } catch (e) {
                    setError((e as Error).message);
                  }
                }}
              >
                Reconnect AI
              </Button>
            </Notice>
          )}
          <form className="composer" onSubmit={send}>
            {!isChat && (
              <div className="recipient-row">
                <span>To</span>
                <details className="recipient-picker">
                  <summary>
                    {sendIds.length === 0
                      ? leadRoutes
                        ? "Lead chooses"
                        : "Choose recipient"
                      : sendIds.length === 1
                        ? sendIds[0]
                        : sendIds.length + " recipients"}
                    <ChevronDown size={13} />
                  </summary>
                  <div className="recipient-options">
                    {members.map((a) => (
                      <label key={a.id}>
                        <input
                          type="checkbox"
                          disabled={sending}
                          checked={targets.includes(a.id)}
                          onChange={() => {
                            setTargets((v) =>
                              v.includes(a.id)
                                ? v.filter((x) => x !== a.id)
                                : [...v, a.id],
                            );
                            pendingKey.current = "";
                            localWrite("pending-send:" + id, "");
                          }}
                        />
                        <span>
                          <code>{a.id}</code>
                          <small>
                            {a.state === "stopped"
                              ? "Offline · queue supported"
                              : a.state}
                          </small>
                        </span>
                      </label>
                    ))}
                    {!members.length && (
                      <p>Add an agent to this conversation first.</p>
                    )}
                  </div>
                </details>
                <RoutingControl
                  conversationId={id}
                  members={members}
                  value={routing}
                  onChange={setRouting}
                />
                <span className="composer-private">
                  <ShieldCheck size={12} />
                  {route.reason === "none" && leadRoutes
                    ? "Lead " + routing.lead + " will choose"
                    : reasonText[route.reason]}
                </span>
              </div>
            )}
            {refs.length > 0 && (
              <div className="composer-refs">
                {refs.map((ref) => (
                  <span key={ref.contextId + ref.version}>
                    <Paperclip size={12} />
                    {ref.contextId.slice(0, 10)} · v{ref.version}
                    <button
                      type="button"
                      aria-label="Remove context reference"
                      disabled={sending}
                      onClick={() => {
                        pendingKey.current = "";
                        localWrite("pending-send:" + id, "");
                        setRefs((v) => v.filter((x) => x !== ref));
                      }}
                    >
                      <X size={12} />
                    </button>
                  </span>
                ))}
              </div>
            )}
            {open && options.length > 0 && (
              <ul className="mention-list" id="mention-list" role="listbox" aria-label="Address an agent">
                {options.map((m, i) => (
                  <li
                    key={m.id}
                    role="option"
                    aria-selected={i === Math.min(mentionIndex, options.length - 1)}
                    onMouseDown={(e) => {
                      e.preventDefault();
                      pickMention(m);
                    }}
                  >
                    <strong>@{m.id === "everyone" ? "everyone" : handle(m)}</strong>
                    <small>
                      {m.id === "everyone"
                        ? "Every member of this chat"
                        : m.id}
                    </small>
                  </li>
                ))}
              </ul>
            )}
            <label className="sr-only" htmlFor="message-draft">
              Message selected agents
            </label>
            <textarea
              ref={input}
              id="message-draft"
              value={draft}
              disabled={sending}
              maxLength={60000}
              onChange={(e) => {
                setDraft(e.target.value);
                setCaret(e.target.selectionStart);
                setMentionIndex(0);
                pendingKey.current = "";
                localWrite("pending-send:" + id, "");
              }}
              onSelect={(e) => setCaret(e.currentTarget.selectionStart)}
              role={open && options.length ? "combobox" : undefined}
              aria-expanded={open && options.length ? true : undefined}
              aria-controls={open && options.length ? "mention-list" : undefined}
              placeholder={
                isChat
                  ? "Message your AI…"
                  : sendIds.length || leadRoutes
                    ? "Give your agent a clear task or ask a question…"
                    : "Type @ to address an agent, or choose a recipient…"
              }
              rows={2}
              onKeyDown={(e) => {
                if (e.nativeEvent.isComposing) return;
                if (open && options.length) {
                  if (e.key === "ArrowDown" || e.key === "ArrowUp") {
                    e.preventDefault();
                    setMentionIndex(
                      (i) =>
                        (i + (e.key === "ArrowDown" ? 1 : -1) + options.length) %
                        options.length,
                    );
                    return;
                  }
                  if (e.key === "Enter" || e.key === "Tab") {
                    e.preventDefault();
                    pickMention(options[Math.min(mentionIndex, options.length - 1)]);
                    return;
                  }
                  if (e.key === "Escape") {
                    e.preventDefault();
                    setCaret(0);
                    return;
                  }
                }
                if (
                  e.key === "Enter" &&
                  !e.shiftKey &&
                  (sendKey === "enter" || e.ctrlKey || e.metaKey)
                ) {
                  e.preventDefault();
                  void send();
                }
              }}
            />
            <div className="composer-toolbar">
              <div>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => setAttach(true)}
                  disabled={sending || refs.length >= 16}
                >
                  <Paperclip size={16} />
                  Attach context
                </Button>
                <span className="composer-shortcut">
                  {sendKey === "enter" ? "↵ to send" : "⌘ ↵ to send"} · ⇧ ↵ new
                  line
                </span>
              </div>
              {active ? (
                <Button
                  type="button"
                  variant="outline"
                  disabled={stopping}
                  onClick={() => void stopReply()}
                >
                  <Square size={14} />{" "}
                  {stopping ? "Stopping…" : "Stop response"}
                </Button>
              ) : (
                <Button
                  type="submit"
                  disabled={
                    sending ||
                    !draft.trim() ||
                    new TextEncoder().encode(draft).length > 65536 ||
                    (!sendIds.length && !canLeadRoute) ||
                    !navigator.onLine
                  }
                  size="sm"
                >
                  {routingBusy
                    ? "Lead is choosing…"
                    : sending
                    ? "Sending…"
                    : sendIds.length > 1
                      ? "Send to " + sendIds.length + " agents"
                      : "Send message"}
                  <Send size={14} />
                </Button>
              )}
            </div>
          </form>
          <div className="composer-footnote">
            <ShieldCheck size={12} />
            {new TextEncoder().encode(draft).length > 65536
              ? "Message exceeds 64 KiB. Shorten it to send."
              : isChat
                ? "AI can make mistakes. Recent messages provide context; full history is saved locally."
                : "Context is shared by immutable reference. Agent sessions stay independent."}
          </div>
        </div>
      </section>
      {inspector && (
        <aside className="chat-inspector">
          <div className="inspector-heading">
            <strong>Conversation details</strong>
            <Button
              variant="ghost"
              size="icon"
              aria-label="Close inspector"
              onClick={() => setInspector(false)}
            >
              <X size={17} />
            </Button>
          </div>
          <div className="inspector-content">
            <h2>{conversation.data?.title}</h2>
            {!isChat && <code className="break-id">{id}</code>}
            <div className="inspector-section">
              <h3>
                <Users size={15} />
                {isChat ? "AI" : "Participants"}
              </h3>
              {members.map((a) => (
                <div className="inspector-agent" key={a.id}>
                  <ChatAgentAvatar id={a.id} agents={agents} />
                  <div>
                    <strong>{isChat ? harnessName(a.id) : a.id}</strong>
                    {!isChat && <small>{a.cwd}</small>}
                  </div>
                  <Status state={a.state} />
                </div>
              ))}
              {!isChat && (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setParticipants(true)}
                >
                  <Plus size={14} />
                  Manage participants
                </Button>
              )}
            </div>
            {isChat && conversation.data?.chatSettings && (
              <div className="inspector-section">
                <h3>Chat configuration</h3>
                <p className="muted">
                  Model:{" "}
                  {conversation.data.chatSettings.config.model ||
                    "Native default"}
                </p>
                {conversation.data.chatSettings.config.provider && (
                  <p className="muted">
                    Provider: {conversation.data.chatSettings.config.provider}
                  </p>
                )}
                <p className="muted">
                  {conversation.data.chatSettings.workspaceId
                    ? "Uses the project directory selected when this chat was created."
                    : "General chat workspace"}
                </p>
              </div>
            )}
            <div className="inspector-section">
              <h3>
                <BookOpen size={15} />
                Shared context
              </h3>
              {!conversation.data?.contextRefs?.length ? (
                <p className="muted">No context references attached.</p>
              ) : (
                conversation.data.contextRefs.map((ref) => (
                  <code className="ref-line" key={ref.contextId + ref.version}>
                    {ref.contextId} · v{ref.version}
                  </code>
                ))
              )}
              <Button
                variant="outline"
                size="sm"
                onClick={() => setAttach(true)}
                disabled={sending || refs.length >= 16}
              >
                <Paperclip size={14} />
                Browse references
              </Button>
            </div>
            <div className="policy-note">
              <ShieldCheck size={18} />
              <strong>Independent by design</strong>
              <p>
                Membership doesn’t share agent history or grant access to
                private context.
              </p>
            </div>
          </div>
        </aside>
      )}
      <Dialog
        open={renaming}
        onOpenChange={setRenaming}
        title="Rename conversation"
        description="Update the title for this persistent conversation."
      >
        <form onSubmit={rename}>
          <label htmlFor="rename-title">Title</label>
          <input
            id="rename-title"
            value={title}
            maxLength={180}
            onChange={(e) => setTitle(e.target.value)}
          />
          <div className="dialog-footer">
            <Button type="submit" disabled={!title.trim()}>
              Save title
            </Button>
          </div>
        </form>
      </Dialog>
      <Dialog
        open={attach}
        onOpenChange={setAttach}
        title="Attach shared context"
        description="Choose an exact immutable version. Meshra checks access as every receiving agent."
        wide
      >
        <ContextBrowser
          compact
          recipients={sendIds}
          onAttach={(ref) => {
            pendingKey.current = "";
            localWrite("pending-send:" + id, "");
            setRefs((v) =>
              v.some(
                (x) =>
                  x.contextId === ref.contextId && x.version === ref.version,
              )
                ? v
                : [...v, ref],
            );
            setAttach(false);
          }}
        />
      </Dialog>
      <Dialog
        open={participants}
        onOpenChange={setParticipants}
        title="Conversation participants"
        description="Exact identities, independent sessions. Changes never grant context access."
        wide
      >
        <ParticipantManager
          conversation={conversation.data}
          agents={agents}
          onChange={() => {
            client.invalidateQueries({ queryKey: ["conversation", id] });
            client.invalidateQueries({ queryKey: ["inbox"] });
          }}
        />
      </Dialog>
    </div>
  );
}
function ChatAgentAvatar({ id, agents }: { id: string; agents: Agent[] }) {
  const agent = agents.find((item) => item.id === id);
  if (!agent || agent.managedChat || agent.profileManaged) return <Harness id={id} />;
  return <AgentAvatar icon={agent.avatarIcon} color={agent.avatarColor} size={28} />;
}
export function Delivery({ task }: { task: Task }) {
  return (
    <span
      className={
        "delivery delivery-" +
        task.state +
        (task.ambiguous ? " delivery-uncertain" : "")
      }
    >
      {task.state === "completed" ? <Check size={12} /> : <Clock size={12} />}{" "}
      {stateLabel(task)}
    </span>
  );
}
function formatTime(s: string) {
  return new Intl.DateTimeFormat(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  }).format(new Date(s));
}
function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      onClick={() =>
        navigator.clipboard.writeText(text).then(() => {
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        })
      }
    >
      {copied ? <Check size={13} /> : <Copy size={13} />}{" "}
      {copied ? "Copied" : "Copy"}
    </button>
  );
}
function ParticipantManager({
  conversation,
  agents,
  onChange,
}: {
  conversation?: Conversation;
  agents: Agent[];
  onChange: () => void;
}) {
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  if (!conversation) return null;
  async function change(agentId: string, remove: boolean) {
    setBusy(agentId);
    setError("");
    try {
      await collab(
        remove
          ? "conversation.removeParticipant"
          : "conversation.addParticipant",
        { id: conversation!.conversationId, agentId },
      );
      onChange();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }
  return (
    <div className="participant-manager">
      {error && <Notice>{error}</Notice>}
      {agents.map((a) => {
        const member = conversation.participants.includes(a.id);
        return (
          <div className="managed-agent" key={a.id}>
            <ChatAgentAvatar id={a.id} agents={agents} />
            <div>
              <strong>{a.id}</strong>
              <small>{a.cwd}</small>
              <small>
                {a.policy.delegate
                  ? "Delegation permitted"
                  : "Delegation disabled"}{" "}
                ·{" "}
                {(a.policy.operations ?? []).join(", ") ||
                  "No agent-initiated operations"}
              </small>
            </div>
            <Status state={a.state} />
            <Button
              variant={member ? "outline" : "default"}
              size="sm"
              disabled={!!busy}
              onClick={() => void change(a.id, member)}
            >
              {busy === a.id ? "Updating…" : member ? "Remove" : "Add"}
            </Button>
          </div>
        );
      })}
    </div>
  );
}

function Thinking({ state }: { state: string }) {
  return (
    <div
      className="thinking-indicator"
      role="status"
      aria-label={state === "queued" ? "Waiting for AI" : "AI is thinking"}
    >
      <span className="thinking-orbit" aria-hidden="true">
        <span />
        <span />
        <span />
      </span>
      <span className="thinking-label">
        {state === "queued"
          ? "Getting ready"
          : state === "dispatching"
            ? "Connecting"
            : "Thinking"}
        <span className="thinking-ellipsis" aria-hidden="true">
          …
        </span>
      </span>
    </div>
  );
}
