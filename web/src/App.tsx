import { useEffect, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Activity,
  ArrowLeft,
  ArrowUpRight,
  BookOpen,
  Check,
  ChevronDown,
  Command,
  Layers3,
  FolderOpen,
  LogOut,
  MessageSquare,
  PanelLeftClose,
  Plus,
  Search,
  Settings,
  ShieldCheck,
  SquareTerminal,
  Users,
  UserRound,
  X,
  type LucideIcon,
} from "lucide-react";
import { api, collab, localRead, localWrite, setCSRF } from "./api";
import type {
  Agent,
  Conversation,
  InboxItem,
  ModelOption,
  Workspace,
  PersonalPreferences,
} from "./types";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";
import { Chat } from "./Chat";
import { Operations } from "./Operations";
import { ConversationList, type ConversationFilter } from "./ConversationList";
import { HarnessLogo, harnessName } from "./HarnessLogo";
export const navigation = [
  ["chat", "Chat", MessageSquare],
  ["agents", "Agents", Users],
  ["workspaces", "Workspaces", FolderOpen],
  ["contexts", "Contexts", BookOpen],
  ["activity", "Activity", Activity],
  ["settings", "Settings", Settings],
] as const;
export type View = (typeof navigation)[number][0];
function route() {
  const [view, id] = location.hash.slice(1).split("/");
  return {
    view: (navigation.some((x) => x[0] === view) ? view : "chat") as View,
    id: id || "",
  };
}
export function go(view: View, id = "") {
  location.hash = view + (id ? "/" + id : "");
}
export function Brand({ small = false }: { small?: boolean }) {
  return (
    <div className={"brand " + (small ? "brand-small" : "")}>
      <span className="brand-mark">
        <Layers3 size={small ? 19 : 25} strokeWidth={1.7} />
      </span>
      {!small && (
        <span>
          meshra<span className="brand-dot">.</span>
        </span>
      )}
    </div>
  );
}
export function Status({ state }: { state: string }) {
  return (
    <span className={"status status-" + state}>
      <span className="status-dot" />
      {(
        {
          idle: "Ready",
          busy: "Busy",
          stopped: "Offline",
          error: "Error",
          starting: "Starting",
        } as Record<string, string>
      )[state] || state}
    </span>
  );
}
export function Harness({ id }: { id: string }) {
  return <HarnessLogo id={id} />;
}
export function Notice({ children }: { children: React.ReactNode }) {
  return (
    <div className="notice" role="alert">
      {children}
    </div>
  );
}
export function Empty({
  icon: Icon,
  title,
  children,
  action,
}: {
  icon: LucideIcon;
  title: string;
  children: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <div className="empty-state">
      <span className="empty-icon">
        <Icon size={27} strokeWidth={1.5} />
      </span>
      <h2>{title}</h2>
      <p>{children}</p>
      {action}
    </div>
  );
}
export function App() {
  const client = useQueryClient();
  const [connected, setConnected] = useState(false);
  const [loading, setLoading] = useState(true);
  const [theme, setTheme] = useState(() => localRead("theme", "system"));
  const [dense, setDense] = useState(() => localRead("dense", false));
  useEffect(() => {
    api<{ csrf: string }>("session")
      .then((v) => {
        setCSRF(v.csrf);
        setConnected(true);
      })
      .catch(() => {})
      .finally(() => setLoading(false));
    const expired = () => {
      setConnected(false);
      client.clear();
    };
    window.addEventListener("meshra:expired", expired);
    return () => window.removeEventListener("meshra:expired", expired);
  }, [client]);
  useEffect(() => {
    localWrite("theme", theme);
    const mq = matchMedia("(prefers-color-scheme: dark)");
    const update = () =>
      (document.documentElement.dataset.theme =
        theme === "system" ? (mq.matches ? "dark" : "light") : theme);
    update();
    mq.addEventListener("change", update);
    return () => mq.removeEventListener("change", update);
  }, [theme]);
  useEffect(() => {
    document.documentElement.dataset.density = dense
      ? "compact"
      : "comfortable";
    localWrite("dense", dense);
  }, [dense]);
  if (loading)
    return (
      <main className="login">
        <Brand />
        <p role="status">Connecting to the local relay…</p>
      </main>
    );
  if (!connected) return <Login onConnect={() => setConnected(true)} />;
  return (
    <Shell
      theme={theme}
      setTheme={setTheme}
      dense={dense}
      setDense={setDense}
      onLogout={async () => {
        await api("logout", {});
        setCSRF("");
        client.clear();
        setConnected(false);
      }}
    />
  );
}
function Login({ onConnect }: { onConnect: () => void }) {
  const [token, setToken] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [recovery, setRecovery] = useState(false);
  const auth = useQuery({
    queryKey: ["auth"],
    queryFn: () => api<{ configured: boolean }>("auth"),
    retry: false,
  });
  const usePassword = !!auth.data?.configured && !recovery;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const v = await api<{ csrf: string }>(
        "login",
        usePassword ? { username, password } : { token },
        { "X-Meshra-Console": "1" },
      );
      setToken("");
      setPassword("");
      setCSRF(v.csrf);
      onConnect();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <main className="login">
      <div className="login-card">
        <Brand />
        <h1>
          Your agents.
          <br />
          One conversation.
        </h1>
        <p>
          A focused workspace to talk to named agents, share context, and follow
          their work.
        </p>
        <form onSubmit={submit}>
          {auth.isPending ? (
            <p role="status">Loading sign-in…</p>
          ) : auth.isError ? (
            <>
              <Notice>{auth.error.message}</Notice>
              <Button type="button" onClick={() => auth.refetch()}>
                Retry
              </Button>
            </>
          ) : (
            <>
              {usePassword ? (
                <>
                  <label htmlFor="username">Username</label>
                  <input
                    id="username"
                    name="username"
                    autoComplete="username"
                    value={username}
                    onChange={(e) => setUsername(e.target.value)}
                    maxLength={64}
                    required
                    autoCapitalize="none"
                    spellCheck={false}
                  />
                  <label htmlFor="password">Password</label>
                  <input
                    id="password"
                    name="password"
                    type="password"
                    autoComplete="current-password"
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    maxLength={1024}
                    required
                  />
                </>
              ) : (
                <>
                  <label htmlFor="token">Local administrator token</label>
                  <input
                    id="token"
                    type="password"
                    value={token}
                    onChange={(e) => setToken(e.target.value)}
                    autoComplete="off"
                    placeholder="Paste your local admin token"
                    required
                  />
                  <small>
                    {auth.data?.configured
                      ? "Use your administrator token to recover access. You can reset your password in Settings → Personal access."
                      : "Sign in with your admin.token once, then create your username and password in Settings → Personal access."}
                  </small>
                </>
              )}
              {error && <Notice>{error}</Notice>}
              <Button
                type="submit"
                disabled={
                  busy || (usePassword ? !username || !password : !token.trim())
                }
              >
                {busy
                  ? "Signing in…"
                  : usePassword
                    ? "Sign in"
                    : "Connect to Meshra"}
                <ArrowUpRight size={16} />
              </Button>
              {auth.data?.configured && (
                <Button
                  type="button"
                  variant="ghost"
                  disabled={busy}
                  onClick={() => {
                    setRecovery(!recovery);
                    setToken("");
                    setPassword("");
                    setError("");
                  }}
                >
                  {recovery
                    ? "Use username and password"
                    : "Recover with administrator token"}
                </Button>
              )}
            </>
          )}
        </form>
        <div className="secure-note">
          <ShieldCheck size={16} /> Private workspace · Secure session
        </div>
      </div>
      <div className="login-footer">
        MESHRA <span>Communication infrastructure for independent agents.</span>
      </div>
    </main>
  );
}
function Shell({
  theme,
  setTheme,
  dense,
  setDense,
  onLogout,
}: {
  theme: string;
  setTheme: (v: string) => void;
  dense: boolean;
  setDense: (v: boolean) => void;
  onLogout: () => Promise<void>;
}) {
  const client = useQueryClient();
  const [current, setCurrent] = useState(route);
  const [newChat, setNewChat] = useState(false);
  const [projectChat, setProjectChat] = useState("");
  const preferences = useQuery({
    queryKey: ["preferences"],
    queryFn: () => api<PersonalPreferences>("preferences"),
  });
  useEffect(() => {
    const fn = (event: Event) => {
      setProjectChat((event as CustomEvent<string>).detail);
      setNewChat(true);
    };
    window.addEventListener("meshra:project-chat", fn);
    return () => window.removeEventListener("meshra:project-chat", fn);
  }, []);
  const [palette, setPalette] = useState(false);
  const [search, setSearch] = useState("");
  const [conversationSearch, setConversationSearch] = useState("");
  useEffect(() => {
    const timer = setTimeout(() => setConversationSearch(search.trim()), 200);
    return () => clearTimeout(timer);
  }, [search]);
  const [conversationFilter, setConversationFilter] =
    useState<ConversationFilter>("all");
  const [collapsed, setCollapsed] = useState(() =>
    localRead("rail-collapsed", false),
  );
  const [pins, setPins] = useState<string[]>(() => localRead("pins", []));
  const [inboxWidth, setInboxWidth] = useState(() =>
    localRead("inbox-width", 320),
  );
  const agents = useQuery({
    queryKey: ["agents"],
    queryFn: () => api<Agent[]>("agents"),
    refetchInterval: 20000,
    refetchIntervalInBackground: false,
  });
  const inbox = useQuery({
    queryKey: ["inbox", conversationSearch],
    gcTime: 0,
    queryFn: () =>
      api<InboxItem[]>(
        "conversations?limit=100&keyword=" +
          encodeURIComponent(conversationSearch),
      ),
    refetchInterval: 30000,
    refetchIntervalInBackground: false,
  });
  useEffect(() => {
    const fn = () => setCurrent(route());
    window.addEventListener("hashchange", fn);
    return () => window.removeEventListener("hashchange", fn);
  }, []);
  useEffect(() => {
    const fn = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault();
        setPalette((v) => !v);
      }
      if (
        (e.metaKey || e.ctrlKey) &&
        e.shiftKey &&
        e.key.toLowerCase() === "o"
      ) {
        e.preventDefault();
        setNewChat(true);
      }
    };
    window.addEventListener("keydown", fn);
    return () => window.removeEventListener("keydown", fn);
  }, []);
  const all = agents.data || [];
  const conversations = [...(inbox.data || [])].sort(
    (a, b) =>
      Number(pins.includes(b.conversation.conversationId)) -
      Number(pins.includes(a.conversation.conversationId)),
  );
  function pin(id: string) {
    const next = pins.includes(id)
      ? pins.filter((x) => x !== id)
      : [...pins, id];
    setPins(next);
    localWrite("pins", next);
  }
  return (
    <div
      className={
        "app-shell " +
        (collapsed ? "rail-collapsed " : "") +
        (current.view === "chat" ? "is-chat " : "") +
        (current.id ? "conversation-open" : "")
      }
      style={{ "--inbox-width": inboxWidth + "px" } as React.CSSProperties}
    >
      <a href="#main-content" className="skip-link">
        Skip to content
      </a>
      <aside className="navigation">
        <a href="#chat" aria-label="Meshra home" className="brand-link">
          <Brand small={collapsed} />
        </a>
        <div className="workspace-label">
          <span className="workspace-avatar">M</span>
          <div>
            <strong>Local workspace</strong>
            <small>Developer console</small>
          </div>
        </div>
        <Button
          variant="outline"
          className="search-trigger"
          onClick={() => setPalette(true)}
          title="Search (Ctrl+K)"
        >
          <Search size={16} />
          <span>Quick search</span>
          <kbd>⌘ K</kbd>
        </Button>
        <span className="nav-label">WORKSPACE</span>
        <nav aria-label="Primary navigation">
          {navigation.map(([key, label, Icon]) => (
            <a
              key={key}
              href={"#" + key}
              aria-label={label}
              aria-current={current.view === key ? "page" : undefined}
              title={collapsed ? label : undefined}
            >
              <Icon size={18} />
              <span>{label}</span>
              {key === "chat" && (
                <span className="nav-shortcut" aria-hidden="true">
                  ↗
                </span>
              )}
            </a>
          ))}
        </nav>
        <div className="nav-bottom">
          <div className="local-connection">
            <span
              className={"status-dot " + (agents.isError ? "offline-dot" : "")}
            />
            <span>
              {agents.isError ? "Relay unavailable" : "Local relay connected"}
            </span>
          </div>
          <Button
            variant="ghost"
            onClick={() => {
              setCollapsed(!collapsed);
              localWrite("rail-collapsed", !collapsed);
            }}
            aria-label={collapsed ? "Expand navigation" : "Collapse navigation"}
          >
            <PanelLeftClose size={17} />
            <span>Collapse sidebar</span>
          </Button>
          <div className="operator">
            <span className="operator-avatar">
              <UserRound size={14} />
            </span>
            <div>
              <strong>
                {preferences.data?.displayName || "Local operator"}
              </strong>
              <small>Administrator</small>
            </div>
            <Button
              variant="ghost"
              size="icon"
              aria-label="Disconnect session"
              onClick={() => {
                void onLogout();
              }}
            >
              <LogOut size={16} />
            </Button>
          </div>
        </div>
      </aside>
      {current.view === "chat" && (
        <aside className="inbox">
          <ConversationList
            items={conversations}
            currentId={current.id}
            search={search}
            onSearch={setSearch}
            filter={conversationFilter}
            onFilter={setConversationFilter}
            pins={pins}
            onPin={pin}
            onNew={() => setNewChat(true)}
            loading={inbox.isLoading || search.trim() !== conversationSearch}
            error={inbox.isError}
            onRetry={() => void inbox.refetch()}
          />
          <div className="inbox-footer">
            <ShieldCheck size={14} />
            <span>Private by default. Shared by choice.</span>
          </div>
          <div
            className="inbox-resizer"
            role="separator"
            aria-orientation="vertical"
            aria-label="Resize conversation sidebar"
            aria-valuenow={inboxWidth}
            aria-valuemin={240}
            aria-valuemax={420}
            tabIndex={0}
            onKeyDown={(e) => {
              if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
                const value = Math.min(
                  420,
                  Math.max(
                    240,
                    inboxWidth + (e.key === "ArrowRight" ? 16 : -16),
                  ),
                );
                setInboxWidth(value);
                localWrite("inbox-width", value);
              }
            }}
            onPointerDown={(e) => {
              e.currentTarget.setPointerCapture(e.pointerId);
              const start = e.clientX;
              const width = inboxWidth;
              const element = e.currentTarget;
              const move = (event: PointerEvent) =>
                setInboxWidth(
                  Math.min(420, Math.max(240, width + event.clientX - start)),
                );
              const up = (event: PointerEvent) => {
                localWrite(
                  "inbox-width",
                  Math.min(420, Math.max(240, width + event.clientX - start)),
                );
                element.removeEventListener("pointermove", move);
                element.removeEventListener("pointerup", up);
              };
              element.addEventListener("pointermove", move);
              element.addEventListener("pointerup", up);
            }}
          />
        </aside>
      )}
      <main id="main-content" className="main-panel" tabIndex={-1}>
        <div className="topbar">
          <div className="breadcrumbs">
            <SquareTerminal size={15} />
            <span>Local workspace</span>
            <span>/</span>
            <strong>
              {navigation.find((x) => x[0] === current.view)?.[1]}
            </strong>
          </div>
          <div className="topbar-actions">
            <span className="version-tag">v0.4</span>
            <Button
              variant="ghost"
              size="icon"
              aria-label="Open command palette"
              onClick={() => setPalette(true)}
            >
              <Command size={16} />
            </Button>
          </div>
        </div>
        {current.view === "chat" ? (
          current.id ? (
            <Chat
              key={current.id}
              id={current.id}
              agents={all}
              onNew={() => setNewChat(true)}
            />
          ) : (
            <div className="welcome">
              <div className="welcome-content">
                <span className="welcome-brand">
                  <Layers3 size={35} strokeWidth={1.3} />
                </span>
                <h1>
                  Good work starts
                  <br />
                  with a conversation.
                </h1>
                <p>
                  Bring your coding agents together. Give each one a clear task,
                  <br className="desktop-break" /> share only the context it
                  needs, and follow the work.
                </p>
                <Button onClick={() => setNewChat(true)}>
                  <Plus size={17} />
                  New conversation<span className="button-key">⌘ ⇧ O</span>
                </Button>
                <div className="welcome-actions">
                  <button onClick={() => go("agents")}>
                    <Users size={20} />
                    <strong>Meet your agents</strong>
                    <span>Named instances, clear boundaries.</span>
                    <ArrowUpRight size={16} />
                  </button>
                  <button onClick={() => go("contexts")}>
                    <BookOpen size={20} />
                    <strong>Share useful context</strong>
                    <span>Immutable references. Explicit access.</span>
                    <ArrowUpRight size={16} />
                  </button>
                </div>
                {all.length > 0 && (
                  <div className="available-agents">
                    <div>
                      {all.slice(0, 4).map((a) => (
                        <button key={a.id} onClick={() => setNewChat(true)}>
                          <Harness id={a.id} />
                          <span>{a.id}</span>
                          <Status state={a.state} />
                        </button>
                      ))}
                    </div>
                  </div>
                )}
                <div className="welcome-note">
                  <ShieldCheck size={14} />
                  Messages go only to the recipients you select.
                </div>
              </div>
              <div className="welcome-bottom">
                <span>
                  <kbd>⌘ K</kbd> Quick search
                </span>
                <span>
                  <kbd>⇧ Enter</kbd> New line
                </span>
                <span>Built for independent agents.</span>
              </div>
            </div>
          )
        ) : (
          <Operations
            view={current.view}
            resourceId={current.id}
            agents={all.filter((a) => !a.managedChat)}
            loading={agents.isLoading}
            error={agents.error}
            theme={theme}
            setTheme={setTheme}
            dense={dense}
            setDense={setDense}
          />
        )}
      </main>
      <NewConversation
        initialWorkspace={projectChat}
        open={newChat}
        onOpenChange={setNewChat}
        agents={all}
        onCreated={(c) => {
          setSearch("");
          setConversationFilter("all");
          client.invalidateQueries({ queryKey: ["inbox"] });
          go("chat", c.conversationId);
        }}
      />
      <Dialog
        open={palette}
        onOpenChange={setPalette}
        title="Go anywhere"
        description="Search conversations, agents, or console sections."
      >
        <CommandSearch
          agents={all.filter((a) => !a.managedChat)}
          inbox={inbox.data || []}
          onClose={() => setPalette(false)}
          onNew={() => {
            setPalette(false);
            setNewChat(true);
          }}
        />
      </Dialog>
      <nav className="mobile-nav" aria-label="Mobile navigation">
        {navigation.slice(0, 4).map(([key, label, Icon]) => (
          <a
            href={"#" + key}
            key={key}
            aria-current={current.view === key ? "page" : undefined}
          >
            <Icon size={19} />
            <span>{label}</span>
          </a>
        ))}
        <button aria-label="More navigation" onClick={() => setPalette(true)}>
          <Command size={19} />
          <span>More</span>
        </button>
      </nav>
    </div>
  );
}
function CommandSearch({
  agents,
  inbox,
  onClose,
  onNew,
}: {
  agents: Agent[];
  inbox: InboxItem[];
  onClose: () => void;
  onNew: () => void;
}) {
  const [q, setQ] = useState("");
  return (
    <div className="command-search">
      <div className="search-field">
        <Search size={18} />
        <input
          autoFocus
          aria-label="Search console"
          placeholder="Search or jump to…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
      </div>
      <div className="command-results">
        <button onClick={onNew}>
          <Plus size={17} />
          New conversation<kbd>⌘ ⇧ O</kbd>
        </button>
        {navigation
          .filter((x) => x[1].toLowerCase().includes(q.toLowerCase()))
          .map(([key, label, Icon]) => (
            <button
              key={key}
              onClick={() => {
                go(key);
                onClose();
              }}
            >
              <Icon size={17} />
              {label}
              <ArrowUpRight size={14} />
            </button>
          ))}
        {inbox
          .filter((x) =>
            x.conversation.title.toLowerCase().includes(q.toLowerCase()),
          )
          .slice(0, 8)
          .map((x) => (
            <button
              key={x.conversation.conversationId}
              onClick={() => {
                go("chat", x.conversation.conversationId);
                onClose();
              }}
            >
              <MessageSquare size={17} />
              {x.conversation.title}
            </button>
          ))}
        {q &&
          agents
            .filter((x) => x.id.includes(q))
            .slice(0, 5)
            .map((x) => (
              <button
                key={x.id}
                onClick={() => {
                  go("agents");
                  onClose();
                }}
              >
                <Harness id={x.id} />
                {x.id}
              </button>
            ))}
      </div>
    </div>
  );
}
function NewConversation({
  initialWorkspace,
  open,
  onOpenChange,
  agents,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agents: Agent[];
  onCreated: (c: Conversation) => void;
  initialWorkspace: string;
}) {
  const client = useQueryClient();
  const [title, setTitle] = useState("");
  const [mode, setMode] = useState<"chat" | "collaboration">("chat");
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [harness, setHarness] = useState("codex");
  const [workspace, setWorkspace] = useState("");
  const [selectedModel, setSelectedModel] = useState("");
  useEffect(() => {
    if (open) {
      setWorkspace(initialWorkspace);
      setSelectedModel("");
    }
  }, [open, initialWorkspace]);
  const workspaces = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => api<Workspace[]>("workspaces"),
    enabled: open,
  });
  const models = useQuery({
    queryKey: ["models", harness],
    queryFn: () => api<{ items: ModelOption[] }>("models", { harness }),
    enabled: open && mode === "chat",
    retry: false,
    gcTime: 0,
    staleTime: 60000,
  });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const harnesses = useQuery({
    queryKey: ["harnesses"],
    queryFn: () => api<{ id: string }[]>("harnesses"),
    enabled: open,
    gcTime: 0,
  });
  async function create(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const c =
        mode === "chat"
          ? await api<Conversation>("chat-create", {
              harness,
              workspaceId: workspace,
              config: selectedModel ? JSON.parse(selectedModel) : {},
              title: title.trim() || "New chat",
            })
          : await collab<Conversation>("conversation.create", {
              title:
                title.trim() ||
                "Conversation with " +
                  selected.map((id) => id.split(":")[1]).join(", "),
              participants: selected,
            });
      await client.invalidateQueries({ queryKey: ["agents"] });
      onCreated(c);
      onOpenChange(false);
      setTitle("");
      setSelected([]);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="Start a conversation"
      description={
        mode === "chat"
          ? "Choose your AI. Every conversation has its own history."
          : "Choose the named agents you want to work with."
      }
      wide
    >
      <form onSubmit={create} className="new-conversation-form">
        <label htmlFor="conversation-mode">Conversation type</label>
        <select
          id="conversation-mode"
          value={mode}
          onChange={(e) => setMode(e.target.value as "chat" | "collaboration")}
        >
          <option value="chat">Chat with an AI</option>
          <option value="collaboration">Agent collaboration</option>
        </select>
        <label htmlFor="conversation-title">
          Conversation title <span className="muted">optional</span>
        </label>
        <input
          id="conversation-title"
          autoFocus
          placeholder="What’s on your mind?"
          maxLength={180}
          value={title}
          onChange={(e) => setTitle(e.target.value)}
        />
        {mode === "chat" ? (
          <>
            <fieldset className="harness-picker">
              <legend>Choose your AI</legend>
              <div className="harness-grid">
                {harnesses.data?.map(({ id }) => (
                  <label
                    key={id}
                    className={
                      "harness-option " + (harness === id ? "checked" : "")
                    }
                  >
                    <input
                      type="radio"
                      name="chat-harness"
                      value={id}
                      checked={harness === id}
                      onChange={() => {
                        setHarness(id);
                        setSelectedModel("");
                      }}
                    />
                    <Harness id={id} />
                    <span>
                      <strong>{harnessName(id)}</strong>
                      <small>
                        {id === "claude"
                          ? "Anthropic"
                          : id === "codex"
                            ? "OpenAI"
                            : id === "opencode"
                              ? "Open-source assistant"
                              : "Oh My Pi"}
                      </small>
                    </span>
                    <Check className="harness-check" size={17} />
                  </label>
                ))}
              </div>
            </fieldset>
            {harnesses.isPending && <p role="status">Loading AI options…</p>}
            {harnesses.isError && <Notice>{harnesses.error.message}</Notice>}
            <label htmlFor="chat-workspace">Workspace</label>
            <select
              id="chat-workspace"
              value={workspace}
              onChange={(e) => setWorkspace(e.target.value)}
            >
              <option value="">General chat</option>
              {workspaces.data?.map((w) => (
                <option value={w.id} key={w.id}>
                  {w.name}
                </option>
              ))}
            </select>
            {workspaces.isError && <Notice>{workspaces.error.message}</Notice>}
            <label htmlFor="chat-model">Model</label>
            <select
              id="chat-model"
              value={selectedModel}
              onChange={(e) => setSelectedModel(e.target.value)}
            >
              <option value="">Native default</option>
              {models.data?.items.map((m) => (
                <option
                  key={(m.provider || "") + "/" + m.id}
                  value={JSON.stringify({
                    model: m.id,
                    provider: m.provider || "",
                  })}
                >
                  {m.provider ? m.provider + " · " : ""}
                  {m.name}
                </option>
              ))}
            </select>
            {models.isFetching && <p role="status">Discovering models…</p>}
            {models.isError && (
              <p className="picker-hint">
                Model discovery unavailable. Native default is still available.{" "}
                <button
                  type="button"
                  className="text-button"
                  onClick={() => void models.refetch()}
                >
                  Try again
                </button>
              </p>
            )}
            <p className="picker-hint">
              Uses your local harness and its authentication. No agent setup
              needed.
            </p>
          </>
        ) : (
          <>
            <div className="picker-heading">
              <label htmlFor="agent-search">Add participants</label>
              <span>{selected.length} selected</span>
            </div>
            <div className="search-field">
              <Search size={16} />
              <input
                id="agent-search"
                placeholder="Search your agents…"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
            </div>
            <div className="agent-picker">
              {agents
                .filter(
                  (a) =>
                    !a.managedChat &&
                    (a.id + " " + a.cwd)
                      .toLowerCase()
                      .includes(query.toLowerCase()),
                )
                .map((a) => (
                  <label
                    key={a.id}
                    className={
                      "agent-option " +
                      (selected.includes(a.id) ? "checked" : "")
                    }
                  >
                    <input
                      type="checkbox"
                      checked={selected.includes(a.id)}
                      onChange={() =>
                        setSelected((v) =>
                          v.includes(a.id)
                            ? v.filter((id) => id !== a.id)
                            : [...v, a.id],
                        )
                      }
                    />
                    <Harness id={a.id} />
                    <span className="agent-option-info">
                      <strong>{a.id}</strong>
                      <small title={a.cwd}>{a.cwd}</small>
                    </span>
                    <Status state={a.state} />
                  </label>
                ))}
              {!agents.some((a) => !a.managedChat) && (
                <p>No registered agents yet. Open Agents to create one.</p>
              )}
            </div>
          </>
        )}
        <div className="secure-note">
          <ShieldCheck size={15} />
          {mode === "chat"
            ? "Your history stays local. Recent messages provide context."
            : "Membership never grants access to private context."}
        </div>
        {error && <Notice>{error}</Notice>}
        <div className="dialog-footer">
          <Button
            type="button"
            variant="outline"
            onClick={() => onOpenChange(false)}
          >
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={
              busy ||
              (mode === "chat"
                ? !harnesses.data?.some((h) => h.id === harness)
                : !selected.length)
            }
          >
            {busy ? "Creating…" : "Create conversation"}
            <ArrowUpRight size={16} />
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
