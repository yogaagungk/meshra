import { useEffect, useState, type FormEvent } from "react";
import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  Activity,
  ArrowUpRight,
  BookOpen,
  Check,
  ChevronRight,
  Clock,
  Copy,
  FileText,
  FolderOpen,
  Info,
  Layers3,
  Monitor,
  Moon,
  Play,
  Plus,
  RefreshCw,
  Search,
  Settings,
  ShieldAlert,
  ShieldCheck,
  Square,
  Sun,
  Users,
  X,
} from "lucide-react";
import { api, collab, localRead, localWrite } from "./api";
import { Empty, Harness, Notice, Status, go, type View } from "./App";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";
import { Markdown } from "./Markdown";
import { Delivery } from "./Chat";
import { DirectoryPicker } from "./DirectoryPicker";
import { harnessName } from "./HarnessLogo";
import {
  mergeTasks,
  terminal,
  type Agent,
  type Page,
  type Ref,
  type SharedContext,
  type Task,
  type TraceEvent,
} from "./types";
import { SettingsPanels } from "./Personal";
import { Workspaces } from "./Workspaces";
export function Operations({
  view,
  resourceId,
  agents,
  loading,
  error,
  theme,
  setTheme,
  dense,
  setDense,
}: {
  view: View;
  resourceId?: string;
  agents: Agent[];
  loading: boolean;
  error: Error | null;
  theme: string;
  setTheme: (v: string) => void;
  dense: boolean;
  setDense: (v: boolean) => void;
}) {
  return (
    <section
      className={
        "operations" + (view === "workspaces" ? " operations-workspace" : "")
      }
      key={view}
    >
      <div className="page-heading">
        <div>
          <h1>
            {
              (
                {
                  workspaces: "Coding workspaces",
                  agents: "Your agents",
                  contexts: "Shared context",
                  activity: "Activity & provenance",
                  settings: "Make yourself at home",
                } as Record<string, string>
              )[view]
            }
          </h1>
          <p>
            {
              (
                {
                  workspaces:
                    "Browse project files, review changes, and give your AI the right working directory.",
                  agents:
                    "Independent instances. Exact identities. Clear boundaries.",
                  contexts:
                    "Small, immutable references. Explicit access for every agent.",
                  activity:
                    "A traceable record of what happened, where, and when.",
                  settings:
                    "Appearance, personal access, resource limits, and AI models.",
                } as Record<string, string>
              )[view]
            }
          </p>
        </div>
      </div>
      {view === "workspaces" ? (
        <Workspaces />
      ) : view === "agents" ? (
        <Agents agents={agents} loading={loading} error={error} />
      ) : view === "contexts" ? (
        <ContextBrowser />
      ) : view === "settings" ? (
        <SettingsPanels>
          <Preferences
            theme={theme}
            setTheme={setTheme}
            dense={dense}
            setDense={setDense}
          />
        </SettingsPanels>
      ) : (
        <RecentActivity />
      )}
    </section>
  );
}
function Agents({
  agents,
  loading,
  error,
}: {
  agents: Agent[];
  loading: boolean;
  error: Error | null;
}) {
  const client = useQueryClient();
  const [q, setQ] = useState("");
  const [filter, setFilter] = useState("all");
  const [create, setCreate] = useState(false);
  const [selected, setSelected] = useState<Agent | null>(null);
  const [action, setAction] = useState("");
  const [failure, setFailure] = useState("");
  const [stopping, setStopping] = useState<Agent | null>(null);
  async function lifecycle(a: Agent, op: string) {
    setAction(a.id);
    setFailure("");
    try {
      await api("agents/" + encodeURIComponent(a.id) + "/" + op, {});
      client.invalidateQueries({ queryKey: ["agents"] });
      setSelected(null);
    } catch (e) {
      setFailure((e as Error).message);
    } finally {
      setAction("");
      setStopping(null);
    }
  }
  const rows = agents.filter(
    (a) =>
      (filter === "all" || a.harness === filter) &&
      (a.id + " " + a.cwd).toLowerCase().includes(q.toLowerCase()),
  );
  return (
    <>
      <div className="section-toolbar">
        <div className="search-field">
          <Search size={17} />
          <input
            aria-label="Search agents"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Search instances or workspaces…"
          />
        </div>
        <select
          aria-label="Filter by harness"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        >
          <option value="all">All harnesses</option>
          {["claude", "codex", "omp", "opencode"].map((h) => (
            <option key={h}>{h}</option>
          ))}
        </select>
        <Button onClick={() => setCreate(true)}>
          <Plus size={16} />
          Register agent
        </Button>
      </div>
      {failure && <Notice>{failure}</Notice>}
      {error && <Notice>{error.message}</Notice>}
      {loading ? (
        <p role="status">Loading registered agents…</p>
      ) : !rows.length ? (
        <Empty
          icon={Users}
          title={
            agents.length ? "No matching agents" : "Your workspace starts here"
          }
          action={
            !agents.length && (
              <Button onClick={() => setCreate(true)}>
                <Plus size={16} />
                Register your first agent
              </Button>
            )
          }
        >
          Register an installed harness with a unique name and an existing
          workspace directory.
        </Empty>
      ) : (
        <div className="agent-grid">
          {rows.map((a) => (
            <article className="agent-card" key={a.id}>
              <div className="agent-card-top">
                <Harness id={a.id} />
                <Status state={a.state} />
              </div>
              <button
                className="agent-card-title"
                onClick={() => setSelected(a)}
              >
                <h2>{a.id}</h2>
                <ArrowUpRight size={16} />
              </button>
              <p className="agent-workspace" title={a.cwd}>
                {a.cwd}
              </p>
              <div className="agent-card-details">
                <span>{a.harness}</span>
                <span>
                  {a.policy.delegate ? "Delegation enabled" : "Direct requests"}
                </span>
              </div>
              {a.error && <p className="text-error">{a.error}</p>}
              <div className="agent-card-footer">
                <span>
                  <span className="status-dot" />
                  {a.sessionId ? "Owned session" : "No session allocated"}
                </span>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!!action || a.state === "starting"}
                  onClick={() =>
                    a.state === "idle" || a.state === "busy"
                      ? setStopping(a)
                      : void lifecycle(
                          a,
                          a.state === "error" ? "restart" : "start",
                        )
                  }
                >
                  {action === a.id ? (
                    "Working…"
                  ) : a.state === "idle" || a.state === "busy" ? (
                    <>
                      <Square size={13} />
                      Stop
                    </>
                  ) : (
                    <>
                      <Play size={13} />
                      {a.state === "error" ? "Restart" : "Start"}
                    </>
                  )}
                </Button>
              </div>
            </article>
          ))}
        </div>
      )}
      <Dialog
        open={!!selected}
        onOpenChange={() => setSelected(null)}
        title={selected?.id || "Agent details"}
        description="The live registry entry and session mapping."
      >
        {selected && (
          <div className="agent-detail">
            <dl>
              <dt>Harness</dt>
              <dd>{selected.harness}</dd>
              <dt>Workspace</dt>
              <dd>
                <code>{selected.cwd}</code>
              </dd>
              <dt>Native session</dt>
              <dd>
                <code>
                  {selected.sessionId || "Allocated on start or first turn"}
                </code>
              </dd>
              <dt>Outbound operations</dt>
              <dd>{(selected.policy.operations ?? []).join(", ") || "None"}</dd>
              <dt>Destinations</dt>
              <dd>
                {(selected.policy.destinations ?? []).join(", ") || "None"}
              </dd>
              <dt>Capabilities</dt>
              <dd>
                {Object.entries(selected.capabilities || {})
                  .filter(([, v]) => v)
                  .map(([k]) => k)
                  .join(", ") || "Not reported"}
              </dd>
            </dl>
            <div className="policy-note">
              Human operator requests use your console session. Agent-to-agent
              requests retain their own policies.
            </div>
          </div>
        )}
      </Dialog>
      <Dialog
        open={!!stopping}
        onOpenChange={() => setStopping(null)}
        title="Stop this agent?"
        description="Stopping may interrupt a running turn. Cancellation does not roll back external effects."
      >
        <p className="mono">{stopping?.id}</p>
        <div className="dialog-footer">
          <Button variant="outline" onClick={() => setStopping(null)}>
            Keep running
          </Button>
          <Button
            variant="destructive"
            disabled={!!action}
            onClick={() => stopping && void lifecycle(stopping, "stop")}
          >
            <Square size={14} />
            Stop agent
          </Button>
        </div>
      </Dialog>
      <RegisterAgent open={create} onOpenChange={setCreate} />
    </>
  );
}
function RegisterAgent({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
}) {
  const client = useQueryClient();
  const [harness, setHarness] = useState("codex");
  const [name, setName] = useState("");
  const [cwd, setCwd] = useState("");
  const [browsing, setBrowsing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api("agents", {
        harness,
        name,
        cwd,
        config: {},
        policy: { destinations: [], operations: [], delegate: false },
      });
      client.invalidateQueries({ queryKey: ["agents"] });
      onOpenChange(false);
      setName("");
      setCwd("");
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
      title="Register a named agent"
      description="Use an installed harness and an existing local directory. Registration won’t start a model turn."
    >
      <form onSubmit={submit} className="form-stack">
        <label htmlFor="agent-harness">Harness</label>
        <select
          id="agent-harness"
          value={harness}
          onChange={(e) => setHarness(e.target.value)}
        >
          {["codex", "omp", "claude", "opencode"].map((h) => (
            <option key={h}>{h}</option>
          ))}
        </select>
        <label htmlFor="agent-name">Instance name</label>
        <input
          id="agent-name"
          value={name}
          pattern="[a-z][a-z0-9_-]{0,62}"
          placeholder="reviewer"
          required
          onChange={(e) => setName(e.target.value)}
        />
        <small className="mono">
          Identity: {harness}:{name || "instance-name"}
        </small>
        <label htmlFor="agent-cwd">Existing workspace directory</label>
        <div className="workspace-picker">
          <input
            id="agent-cwd"
            value={cwd}
            placeholder="Choose a folder…"
            readOnly
            required
            onClick={() => setBrowsing(true)}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                setBrowsing(true);
              }
            }}
            aria-haspopup="dialog"
          />
          <Button
            type="button"
            variant="outline"
            onClick={() => setBrowsing(true)}
          >
            <FolderOpen size={17} />
            Browse…
          </Button>
        </div>
        <DirectoryPicker
          open={browsing}
          onOpenChange={setBrowsing}
          onSelect={setCwd}
        />

        <div className="policy-note">
          <ShieldCheck size={16} />
          Read-oriented tools. Agent-initiated delegation is disabled by
          default. Set outbound policies when creating agents with the CLI.
        </div>
        {error && <Notice>{error}</Notice>}
        <div className="dialog-footer">
          <Button type="submit" disabled={busy || !name || !cwd}>
            {busy ? "Registering…" : "Register agent"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
function RecentActivity() {
  const [before, setBefore] = useState(0);
  const data = useQuery({
    queryKey: ["recent-activity", before],
    queryFn: () => api<Page>("tasks?limit=20&before=" + before),
    refetchInterval: 15000,
    gcTime: 0,
  });
  return (
    <>
      {data.isPending ? (
        <p role="status">Loading recent activity…</p>
      ) : data.isError ? (
        <Notice>{data.error.message}</Notice>
      ) : !data.data.items.length ? (
        <Empty icon={Activity} title="Nothing here yet">
          Your conversations will appear here.
        </Empty>
      ) : (
        <div className="recent-activity">
          {data.data.items.map((t) => (
            <button
              key={t.taskId}
              className="activity-entry"
              disabled={!t.conversationId}
              onClick={() => t.conversationId && go("chat", t.conversationId)}
            >
              <Harness id={t.to} />
              <span>
                <strong>{t.text.slice(0, 100)}</strong>
                <small>
                  {harnessName(t.to)} · {new Date(t.createdAt).toLocaleString()}
                </small>
              </span>
              <Delivery task={t} />
              <ChevronRight size={16} />
            </button>
          ))}
        </div>
      )}
      <div className="directory-paging">
        {before > 0 && (
          <Button variant="ghost" onClick={() => setBefore(0)}>
            Latest activity
          </Button>
        )}
        {data.data?.hasMore && (
          <Button
            variant="outline"
            onClick={() => setBefore(data.data!.nextBefore)}
          >
            Older activity
          </Button>
        )}
      </div>
    </>
  );
}
export function ContextBrowser({
  compact = false,
  onAttach,
  recipients = [],
}: {
  compact?: boolean;
  onAttach?: (ref: Ref) => void;
  recipients?: string[];
}) {
  const [q, setQ] = useState("");
  const [type, setType] = useState("");
  const [selected, setSelected] = useState<SharedContext | null>(null);
  const [version, setVersion] = useState(1);
  const [after, setAfter] = useState("");
  const [error, setError] = useState("");
  const [checking, setChecking] = useState(false);
  const [publish, setPublish] = useState(false);
  const data = useQuery({
    queryKey: ["contexts", q, type, after],
    queryFn: () =>
      collab<SharedContext[]>("context.search", {
        keyword: q,
        type,
        after,
        limit: 40,
      }),
  });
  const content = useQuery({
    queryKey: ["context", selected?.contextId, version],
    queryFn: () =>
      collab<SharedContext>("context.get", {
        contextId: selected!.contextId,
        version,
      }),
    enabled: !!selected,
  });
  async function attach() {
    if (!selected || !onAttach) return;
    setChecking(true);
    setError("");
    try {
      if (recipients.length) {
        const checked = await api<{ denied: string[] }>("context-check", {
          recipients,
          contextRefs: [{ contextId: selected.contextId, version }],
        });
        if (checked.denied.length) {
          setError(
            "Access denied for " +
              checked.denied.join(", ") +
              ". The owner must explicitly grant this context.",
          );
          return;
        }
      }
      onAttach({ contextId: selected.contextId, version });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setChecking(false);
    }
  }
  return (
    <div className={"context-browser " + (compact ? "compact" : "")}>
      <PublishContext open={publish} onOpenChange={setPublish} />
      <div className="section-toolbar">
        <div className="search-field">
          <Search size={16} />
          <input
            aria-label="Search contexts"
            value={q}
            onChange={(e) => {
              setQ(e.target.value);
              setAfter("");
            }}
            placeholder="Search context titles…"
          />
        </div>
        <select
          aria-label="Filter context type"
          value={type}
          onChange={(e) => {
            setType(e.target.value);
            setAfter("");
          }}
        >
          <option value="">All types</option>
          {["contract", "decision", "findings", "result", "test-report"].map(
            (s) => (
              <option key={s}>{s}</option>
            ),
          )}
        </select>
        {!compact && (
          <Button onClick={() => setPublish(true)}>
            <Plus size={15} />
            Publish context
          </Button>
        )}
      </div>
      {data.isError ? (
        <Notice>{data.error.message}</Notice>
      ) : data.isLoading ? (
        <p role="status">Loading authorized context metadata…</p>
      ) : !data.data?.length ? (
        <Empty icon={BookOpen} title="No matching context references">
          Publish a focused contract, decision, or finding. References retain
          their exact version, owner, and access permissions.
        </Empty>
      ) : (
        <div className="context-list">
          {data.data.map((c) => (
            <button
              key={c.contextId}
              className={
                "context-row " +
                (selected?.contextId === c.contextId ? "selected" : "")
              }
              onClick={() => {
                setSelected(c);
                setVersion(c.version);
                setError("");
              }}
            >
              <span className="context-type-icon">
                <FileText size={21} />
              </span>
              <div>
                <strong>{c.title || "Untitled context"}</strong>
                <code>{c.contextId}</code>
                <small>
                  {c.owner} · {c.type || "context"} ·{" "}
                  {(c.size / 1024).toFixed(1)} KB
                </small>
              </div>
              <span className="version-chip">v{c.version}</span>
              <ChevronRight size={17} />
            </button>
          ))}
        </div>
      )}
      {data.data?.length === 40 && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => setAfter(data.data![39].contextId)}
        >
          Next 40 references
        </Button>
      )}
      {after && (
        <Button variant="ghost" size="sm" onClick={() => setAfter("")}>
          Back to newest search
        </Button>
      )}
      {selected && (
        <section className="context-preview">
          <div className="context-preview-heading">
            <div>
              <h3>{selected.title || "Context preview"}</h3>
            </div>
            <Button
              variant="ghost"
              size="icon"
              aria-label="Close context preview"
              onClick={() => setSelected(null)}
            >
              <X size={16} />
            </Button>
          </div>
          <label className="version-selector">
            Version
            <input
              aria-label="Context version"
              type="number"
              min={1}
              max={selected.version}
              value={version}
              onChange={(e) => setVersion(Math.max(1, Number(e.target.value)))}
            />
          </label>
          <dl className="context-metadata">
            <dt>Owner</dt>
            <dd>
              <code>{selected.owner}</code>
            </dd>
            <dt>SHA-256</dt>
            <dd>
              <code>{content.data?.sha256 || "Loading…"}</code>
            </dd>
          </dl>
          <div className="untrusted-label">
            <ShieldCheck size={14} />
            Untrusted shared data · Access checked server-side
          </div>
          {content.isError ? (
            <Notice>{content.error.message}</Notice>
          ) : content.isLoading ? (
            <p role="status">Loading version {version}…</p>
          ) : (
            <div className="context-content">
              <Markdown text={content.data?.content || ""} />
            </div>
          )}
          {error && <Notice>{error}</Notice>}
          {onAttach && (
            <div className="dialog-footer">
              <span className="muted">
                {recipients.length
                  ? "Check access for " +
                    recipients.length +
                    " selected recipient(s)"
                  : "Select recipients before sending; access is checked at admission."}
              </span>
              <Button
                onClick={() => void attach()}
                disabled={checking || content.isError || content.isLoading}
              >
                <BookOpen size={15} />
                {checking ? "Checking access…" : "Attach version " + version}
              </Button>
            </div>
          )}
        </section>
      )}
    </div>
  );
}
function Preferences({
  theme,
  setTheme,
  dense,
  setDense,
}: {
  theme: string;
  setTheme: (v: string) => void;
  dense: boolean;
  setDense: (v: boolean) => void;
}) {
  const [sendKey, setSendKey] = useState(() => localRead("send-key", "enter"));
  return (
    <div className="preferences">
      <section>
        <h2>Appearance</h2>
        <p>Choose a theme for long coding sessions.</p>
        <div className="theme-options">
          {[
            ["system", "System", Monitor],
            ["dark", "Dark", Moon],
            ["light", "Light", Sun],
          ].map(([key, label, Icon]) => {
            const I = Icon as typeof Monitor;
            return (
              <button
                key={String(key)}
                className={theme === key ? "selected" : ""}
                aria-pressed={theme === key}
                onClick={() => setTheme(String(key))}
              >
                <I size={22} />
                <span>{String(label)}</span>
                {theme === key && <Check size={15} />}
              </button>
            );
          })}
        </div>
        <label className="settings-toggle">
          <div>
            <strong>Compact density</strong>
            <small>Reduce spacing in lists and message groups.</small>
          </div>
          <input
            type="checkbox"
            role="switch"
            checked={dense}
            onChange={(e) => setDense(e.target.checked)}
          />
        </label>
      </section>
      <section>
        <h2>Composer</h2>
        <label className="settings-toggle">
          <div>
            <strong>Send messages with</strong>
            <small>
              Shift+Enter always inserts a new line. IME composition never
              sends.
            </small>
          </div>
          <select
            aria-label="Send keyboard shortcut"
            value={sendKey}
            onChange={(e) => {
              setSendKey(e.target.value);
              localWrite("send-key", e.target.value);
            }}
          >
            <option value="enter">Enter</option>
            <option value="modifier">Ctrl / ⌘ + Enter</option>
          </select>
        </label>
      </section>
      <section>
        <h2>Keyboard shortcuts</h2>
        <dl className="shortcut-list">
          <dt>Quick search</dt>
          <dd>
            <kbd>Ctrl / ⌘ K</kbd>
          </dd>
          <dt>New conversation</dt>
          <dd>
            <kbd>Ctrl / ⌘ Shift O</kbd>
          </dd>
          <dt>New line</dt>
          <dd>
            <kbd>Shift Enter</kbd>
          </dd>
          <dt>Close dialog</dt>
          <dd>
            <kbd>Esc</kbd>
          </dd>
        </dl>
      </section>
      <section>
        <h2>Connection & permissions</h2>
        <div className="policy-note">
          <ShieldCheck size={19} />
          <div>
            <strong>Local administrator session</strong>
            <p>
              Same-origin, HttpOnly session; 8-hour absolute expiry. Agent
              credentials and native sessions remain separate. No terminal or
              approval-bypass endpoint is exposed.
            </p>
          </div>
        </div>
        <p className="muted">
          Preferences and drafts stay on this browser. Authoritative messages,
          task state, and context stay in SQLite.
        </p>
      </section>
    </div>
  );
}

function PublishContext({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
}) {
  const client = useQueryClient();
  const [title, setTitle] = useState("");
  const [type, setType] = useState("contract");
  const [text, setText] = useState("");
  const [readers, setReaders] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [key, setKey] = useState(() => crypto.randomUUID());
  const agents = useQuery({
    queryKey: ["agents"],
    queryFn: () => api<Agent[]>("agents"),
    enabled: open,
  });
  const size = new TextEncoder().encode(text).length;
  async function publish(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const c = await collab<SharedContext>("context.publish", {
        title,
        type,
        content: text,
        idempotencyKey: key,
      });
      for (const agentId of readers)
        await collab("context.grant", { contextId: c.contextId, agentId });
      client.invalidateQueries({ queryKey: ["contexts"] });
      onOpenChange(false);
      setTitle("");
      setText("");
      setReaders([]);
      setKey(crypto.randomUUID());
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
      title="Publish shared context"
      description="Create a small, immutable text reference. Keep credentials, tokens, and private keys out of shared content."
      wide
    >
      <form onSubmit={publish} className="form-stack">
        <label htmlFor="context-title">Title</label>
        <input
          id="context-title"
          value={title}
          onChange={(e) => {
            setTitle(e.target.value);
            setKey(crypto.randomUUID());
          }}
          placeholder="Payment API contract"
          required
          maxLength={180}
        />
        <label htmlFor="context-type">Type</label>
        <select
          id="context-type"
          value={type}
          onChange={(e) => {
            setType(e.target.value);
            setKey(crypto.randomUUID());
          }}
        >
          {["contract", "decision", "findings", "test-report"].map((t) => (
            <option key={t}>{t}</option>
          ))}
        </select>
        <label htmlFor="context-content">
          Content · UTF-8 text or Markdown
        </label>
        <textarea
          id="context-content"
          value={text}
          rows={7}
          required
          onChange={(e) => {
            setText(e.target.value);
            setKey(crypto.randomUUID());
          }}
          placeholder="Publish the focused contract or finding your agents need."
          className="publish-content"
        />
        <small>
          {size.toLocaleString()} / 65,536 bytes · the relay may enforce a lower
          configured limit
        </small>
        <label>Explicitly grant readers</label>
        <div className="grant-readers">
          {agents.data?.map((a) => (
            <label key={a.id}>
              <input
                type="checkbox"
                checked={readers.includes(a.id)}
                onChange={() =>
                  setReaders((v) =>
                    v.includes(a.id)
                      ? v.filter((x) => x !== a.id)
                      : [...v, a.id],
                  )
                }
              />
              <Harness id={a.id} />
              <code>{a.id}</code>
            </label>
          ))}
        </div>
        <small>
          Private to the operator by default. Conversation membership is not a
          content grant.
        </small>
        {error && <Notice>{error}</Notice>}
        <div className="dialog-footer">
          <Button
            type="submit"
            disabled={busy || !text.trim() || !title.trim() || size > 65536}
          >
            <BookOpen size={15} />
            {busy ? "Publishing…" : "Publish immutable reference"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
