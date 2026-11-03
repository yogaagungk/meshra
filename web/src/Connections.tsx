import { useEffect, useState, type FormEvent } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Globe,
  KeyRound,
  Mic,
  Plus,
  RefreshCw,
  Server,
  Trash2,
} from "lucide-react";
import {
  automation,
  type ConnectionList,
  type NetworkGrant,
  type ProviderEndpoint,
  type RuntimeReadiness,
  type VoiceReadiness,
  type AutomationResponse,
  type MCPServer,
} from "./automation-api";
import { Empty, Notice, Status } from "./App";
import { api } from "./api";
import type { Workspace } from "./types";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";
import { MessagingConnections } from "./MessagingConnections";

const SECRET_STATUS_LABELS: Record<string, string> = {
  missing: "No key stored",
  saved: "Key stored",
  unreadable: "Stored key unreadable",
};

const READINESS_LABELS: Record<string, string> = {
  ready: "Ready",
  missing: "Missing dependency",
  unsupported: "Unsupported",
  failed: "Failed",
};

function originError(origin: string) {
  const value = origin.trim();
  if (!value) return "Enter the exact origin.";
  if (value.includes("*")) return "Wildcards are not allowed.";
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    return "Enter a full origin such as https://api.example.com.";
  }
  if (parsed.protocol !== "https:" && parsed.protocol !== "http:")
    return "Only http and https origins are allowed.";
  if (parsed.username || parsed.password)
    return "Credentials are not allowed in an origin.";
  if (parsed.pathname && parsed.pathname !== "/")
    return "Use the exact origin without a path.";
  if (parsed.search || parsed.hash)
    return "Use the exact origin without a query or fragment.";
  if (!parsed.hostname) return "Enter the host name.";
  return "";
}

function privateHost(origin: string) {
  let parsed: URL;
  try {
    parsed = new URL(origin.trim());
  } catch {
    return false;
  }
  const host = parsed.hostname.toLowerCase();
  if (host === "localhost" || host.endsWith(".local")) return true;
  if (/^127\./.test(host) || /^10\./.test(host) || /^192\.168\./.test(host))
    return true;
  const private172 = /^172\.(\d+)\./.exec(host);
  if (private172) {
    const block = Number(private172[1]);
    if (block >= 16 && block <= 31) return true;
  }
  if (host === "::1" || host.startsWith("fd")) return true;
  return false;
}

/**
 * Connections is the operator surface for what may leave the machine: exact
 * network origins, the confined runtime, and reviewed provider connections.
 */
export function Connections() {
  return (
    <div className="automation-page">
      <NetworkGrants />
      <ProviderConnections />
      <MCPConnections />
      <MessagingConnections />
      <VoiceReadiness />
      <RuntimeReadiness />
    </div>
  );
}

function RuntimeReadiness() {
  const client = useQueryClient();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState(false);
  const [executables, setExecutables] = useState<[string, string][]>([]);
  const [runtimePaths, setRuntimePaths] = useState("");
  const [snapshotBytes, setSnapshotBytes] = useState("536870912");
  const [snapshotFiles, setSnapshotFiles] = useState("20000");
  const [storeBytes, setStoreBytes] = useState("2147483648");
  const [cgroupRoot, setCgroupRoot] = useState("");
  const runtime = useQuery({
    queryKey: ["runtime-status"],
    queryFn: () => automation("runtime.status", {}),
  });
  const status: RuntimeReadiness | undefined = runtime.data;

  useEffect(() => {
    if (!status) return;
    setExecutables(Object.entries(status.config?.executables || {}));
    setRuntimePaths((status.config?.runtimePaths || []).join("\n"));
    setSnapshotBytes(String(status.config?.limits?.snapshotBytes || 0));
    setSnapshotFiles(String(status.config?.limits?.snapshotFiles || 0));
    setStoreBytes(String(status.config?.limits?.storeBytes || 0));
    setCgroupRoot(status.config?.limits?.cgroupRoot || "");
  }, [status]);

  async function configure(e: FormEvent) {
    e.preventDefault();
    if (!status) return;
    setBusy(true);
    setError("");
    try {
      await automation("runtime.configure", {
        expectedRevision: status.revision,
        executables: Object.fromEntries(
          executables.filter(([name]) => name.trim()),
        ),
        runtimePaths: runtimePaths
          .split("\n")
          .map((line) => line.trim())
          .filter(Boolean),
        limits: {
          snapshotBytes: Number(snapshotBytes),
          snapshotFiles: Number(snapshotFiles),
          storeBytes: Number(storeBytes),
          ...(cgroupRoot.trim() ? { cgroupRoot: cgroupRoot.trim() } : {}),
        },
      });
      await client.invalidateQueries({ queryKey: ["runtime-status"] });
      setEditing(false);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="automation-card">
      <header className="automation-card-head">
        <div>
          <strong>Confined runtime</strong>
          <small className="muted">
            Commands, browsers and agent runtimes execute here, never directly
            on your machine.
          </small>
        </div>
        <div className="automation-actions">
          <Status state={status?.status.state === "ready" ? "idle" : "error"} />
          <Button
            variant="outline"
            size="sm"
            onClick={() => setEditing(true)}
            disabled={!status}
          >
            Configure
          </Button>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Refresh runtime readiness"
            onClick={() => void runtime.refetch()}
          >
            <RefreshCw size={14} />
          </Button>
        </div>
      </header>
      {error && <Notice>{error}</Notice>}
      {runtime.isError && <Notice>{runtime.error.message}</Notice>}
      {runtime.isPending ? (
        <p role="status">Checking runtime readiness…</p>
      ) : status ? (
        <>
          <div className="agent-card-details">
            <span>
              {READINESS_LABELS[status.status.state] || status.status.state}
            </span>
            <span>Revision {status.revision}</span>
            {Object.entries(status.config?.executables || {}).map(([name, path]) => (
              <span key={name}>
                {name}: {path}
              </span>
            ))}
          </div>
          {status.status.prerequisites?.length ? (
            <ul className="automation-prerequisites">
              {status.status.prerequisites.map((item) => (
                <li key={item}>{item}</li>
              ))}
            </ul>
          ) : (
            <p className="muted">No missing prerequisites were reported.</p>
          )}
        </>
      ) : null}
      <Dialog
        open={editing}
        onOpenChange={setEditing}
        title="Configure the confined runtime"
        description="Only the operator changes this manifest. Configuring it grants no execution rights."
      >
        <form className="form-stack" onSubmit={configure}>
          <label htmlFor="runtime-executables">Executables</label>
          {executables.map(([name, path], index) => (
            <div className="automation-inline" key={index}>
              <input
                aria-label={"Executable " + (index + 1) + " name"}
                value={name}
                onChange={(e) =>
                  setExecutables((v) =>
                    v.map((row, i) =>
                      i === index
                        ? ([e.target.value, row[1]] as [string, string])
                        : row,
                    ),
                  )
                }
              />
              <input
                aria-label={"Executable " + (index + 1) + " path"}
                value={path}
                onChange={(e) =>
                  setExecutables((v) =>
                    v.map((row, i) =>
                      i === index
                        ? ([row[0], e.target.value] as [string, string])
                        : row,
                    ),
                  )
                }
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                aria-label={"Remove executable " + (index + 1)}
                onClick={() =>
                  setExecutables((v) => v.filter((_, i) => i !== index))
                }
              >
                <Trash2 size={14} />
              </Button>
            </div>
          ))}
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => setExecutables((v) => [...v, ["bwrap", ""]])}
          >
            <Plus size={13} />
            Add executable
          </Button>
          <label htmlFor="runtime-paths">Runtime paths</label>
          <textarea
            id="runtime-paths"
            rows={3}
            value={runtimePaths}
            onChange={(e) => setRuntimePaths(e.target.value)}
            placeholder="One absolute path per line"
          />
          <label htmlFor="runtime-cgroup-root">Cgroup root (optional)</label>
          <input id="runtime-cgroup-root" value={cgroupRoot} onChange={(e) => setCgroupRoot(e.target.value)} placeholder="/sys/fs/cgroup/meshra" />
          <div className="automation-inline">
            <label htmlFor="runtime-snapshot-bytes">Snapshot bytes</label>
            <input
              id="runtime-snapshot-bytes"
              type="number"
              value={snapshotBytes}
              onChange={(e) => setSnapshotBytes(e.target.value)}
            />
            <label htmlFor="runtime-snapshot-files">Snapshot files</label>
            <input
              id="runtime-snapshot-files"
              type="number"
              value={snapshotFiles}
              onChange={(e) => setSnapshotFiles(e.target.value)}
            />
            <label htmlFor="runtime-store-bytes">Store bytes</label>
            <input
              id="runtime-store-bytes"
              type="number"
              value={storeBytes}
              onChange={(e) => setStoreBytes(e.target.value)}
            />
          </div>
          {error && <Notice>{error}</Notice>}
          <div className="dialog-footer">
            <Button
              type="button"
              variant="outline"
              onClick={() => setEditing(false)}
              disabled={busy}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={busy}>
              {busy ? "Saving…" : "Save runtime"}
            </Button>
          </div>
        </form>
      </Dialog>
    </section>
  );
}

function NetworkGrants() {
  const client = useQueryClient();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [scope, setScope] = useState<"profile" | "connection">("profile");
  const [scopeId, setScopeId] = useState("");
  const [origin, setOrigin] = useState("");
  const [allowPrivate, setAllowPrivate] = useState(false);
  const grants = useQuery({
    queryKey: ["network-grants"],
    queryFn: () => automation("network.list", {}),
  });
  const items: NetworkGrant[] = grants.data || [];

  async function grant(e: FormEvent) {
    e.preventDefault();
    const problem = originError(origin);
    if (problem) {
      setError(problem);
      return;
    }
    if (!scopeId.trim()) {
      setError(
        scope === "profile"
          ? "Choose the profile this origin belongs to."
          : "Choose the connection this origin belongs to.",
      );
      return;
    }
    setBusy("grant");
    setError("");
    const scopeValue = scopeId.trim();
    const existing = items.find(
      (item) =>
        item.origin === origin.trim() &&
        (scope === "profile"
          ? item.profileId === scopeValue && !item.connectionId
          : item.connectionId === scopeValue && !item.profileId),
    );
    try {
      await automation("network.grant", {
        ...(scope === "profile"
          ? { profileId: scopeValue }
          : { connectionId: scopeValue }),
        origin: origin.trim(),
        allowPrivate,
        ...(existing ? { expectedRevision: existing.revision } : {}),
      });
      setOrigin("");
      setAllowPrivate(false);
      await client.invalidateQueries({ queryKey: ["network-grants"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  async function revoke(item: NetworkGrant) {
    setBusy(item.id);
    setError("");
    try {
      await automation("network.revoke", {
        id: item.id,
        expectedRevision: item.revision,
      });
      await client.invalidateQueries({ queryKey: ["network-grants"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  return (
    <section className="automation-card">
      <header className="automation-card-head">
        <div>
          <strong>Network grants</strong>
          <small className="muted">
            Exact origins only. A grant never covers a subnet or a wildcard.
          </small>
        </div>
        <Button
          variant="ghost"
          size="icon"
          aria-label="Refresh network grants"
          onClick={() => void grants.refetch()}
        >
          <RefreshCw size={14} />
        </Button>
      </header>
      {error && <Notice>{error}</Notice>}
      {grants.isError && <Notice>{grants.error.message}</Notice>}
      {privateHost(origin) && (
        <p className="picker-hint">
          This host is private or loopback. Private access has to be confirmed
          explicitly before the grant is accepted.
        </p>
      )}
      {items.length ? (
        <ul className="automation-grant-list">
          {items.map((item) => (
            <li key={item.id}>
              <span className="mono">{item.origin}</span>
              <span className="muted">
                {item.profileId
                  ? "Profile " + item.profileId
                  : item.connectionId
                    ? "Connection " + item.connectionId
                    : "Unscoped"}
              </span>
              {item.allowPrivate && (
                <span className="automation-tag">Private allowed</span>
              )}
              <span className="automation-tag">
                {item.enabled ? "Enabled" : "Revoked"}
              </span>
              <Button
                variant="outline"
                size="sm"
                disabled={busy === item.id || !item.enabled}
                onClick={() => void revoke(item)}
              >
                Revoke
              </Button>
            </li>
          ))}
        </ul>
      ) : (
        !grants.isPending && (
          <p className="muted">
            No origins are allowed yet. Nothing can reach the network until you
            review one.
          </p>
        )
      )}
      <form className="form-stack automation-grant-form" onSubmit={grant}>
        <div className="automation-inline">
          <label htmlFor="grant-scope">Scope</label>
          <select
            id="grant-scope"
            value={scope}
            onChange={(e) => setScope(e.target.value as "profile" | "connection")}
          >
            <option value="profile">Profile</option>
            <option value="connection">Connection</option>
          </select>
          <label htmlFor="grant-scope-id">
            {scope === "profile" ? "Profile id" : "Connection id"}
          </label>
          <input
            id="grant-scope-id"
            required
            value={scopeId}
            onChange={(e) => setScopeId(e.target.value)}
            placeholder={scope === "profile" ? "profile id" : "connection id"}
          />
        </div>
        <div className="automation-inline">
          <label htmlFor="grant-origin">Origin</label>
          <input
            id="grant-origin"
            required
            value={origin}
            onChange={(e) => setOrigin(e.target.value)}
            placeholder="https://api.example.com"
          />
        </div>
        <label className="automation-inline">
          <input
            type="checkbox"
            checked={allowPrivate}
            onChange={(e) => setAllowPrivate(e.target.checked)}
          />
          This origin is a private or loopback address. I confirm private
          access explicitly.
        </label>
        <Button type="submit" disabled={busy === "grant"}>
          <Globe size={15} />
          {busy === "grant" ? "Granting…" : "Grant this origin"}
        </Button>
      </form>
    </section>
  );
}

function ProviderConnections() {
  const client = useQueryClient();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [supportsTools, setSupportsTools] = useState(true);
  const [supportsImages, setSupportsImages] = useState(false);
  const [allowInsecureHttp, setAllowInsecureHttp] = useState(false);
  const [enabled, setEnabled] = useState(false);
  const [editingEndpoint, setEditingEndpoint] = useState<ProviderEndpoint>();
  const [contextWindow, setContextWindow] = useState("0");

  function editEndpoint(endpoint?: ProviderEndpoint) {
    setEditingEndpoint(endpoint);
    setName(endpoint?.name || "");
    setBaseUrl(endpoint?.baseUrl || "");
    setApiKey("");
    setSupportsTools(endpoint?.supportsTools ?? true);
    setSupportsImages(endpoint?.supportsImages ?? false);
    setAllowInsecureHttp(endpoint?.allowInsecureHttp ?? false);
    setEnabled(endpoint?.enabled ?? false);
    setContextWindow(String(endpoint?.contextWindowTokens || 0));
    setError("");
    setAdding(true);
  }
  const connections = useQuery({
    queryKey: ["connections"],
    queryFn: () => automation("connections.list", {}),
  });
  const list: ConnectionList | undefined = connections.data;

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy("save");
    setError("");
    try {
      await automation("connections.saveEndpoint", {
        ...(editingEndpoint ? { expectedRevision: editingEndpoint.revision } : {}),
        endpoint: {
          ...(editingEndpoint ? { id: editingEndpoint.id } : {}),
          name: name.trim(),
          baseUrl: baseUrl.trim(),
          supportsTools,
          supportsImages,
          allowInsecureHttp,
          enabled,
          contextWindowTokens: Number(contextWindow),
        },
        ...(apiKey.trim() ? { apiKey: apiKey.trim() } : {}),
      });
      setName("");
      setBaseUrl("");
      setApiKey("");
      setAdding(false);
      await client.invalidateQueries({ queryKey: ["connections"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  async function disable(endpoint: ProviderEndpoint) {
    setBusy(endpoint.id);
    setError("");
    try {
      await automation("connections.disable", {
        id: endpoint.id,
        expectedRevision: endpoint.revision,
      });
      await client.invalidateQueries({ queryKey: ["connections"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  return (
    <section className="automation-card">
      <header className="automation-card-head">
        <div>
          <strong>Provider connections</strong>
          <small className="muted">
            OpenAI-compatible endpoints. Credentials
            are stored encrypted and never shown again.
          </small>
        </div>
        <div className="automation-actions">
          <Button variant="outline" size="sm" onClick={() => editEndpoint()}>
            <Plus size={13} />
            Add endpoint
          </Button>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Refresh provider connections"
            onClick={() => void connections.refetch()}
          >
            <RefreshCw size={14} />
          </Button>
        </div>
      </header>
      {error && <Notice>{error}</Notice>}
      {connections.isError && (
        <Notice>{connections.error.message}</Notice>
      )}
      {connections.isPending ? (
        <p role="status">Loading connections…</p>
      ) : !list?.endpoints?.length ? (
        <Empty icon={Server} title="No provider connections">
          Add an OpenAI-compatible endpoint to reach a local or remote model
          service with reviewed origins.
        </Empty>
      ) : (
        <ul className="automation-connection-list">
          {list?.endpoints?.map((endpoint) => (
            <li key={endpoint.id}>
              <div>
                <strong>{endpoint.name}</strong>
                <small className="muted mono">{endpoint.baseUrl}</small>
              </div>
              <span className="automation-tag">
                {endpoint.enabled ? "Enabled" : "Disabled"}
              </span>
              <span className="automation-tag">
                <KeyRound size={12} />
                {SECRET_STATUS_LABELS[endpoint.secretStatus] ||
                  endpoint.secretStatus}
              </span>
              <Button variant="outline" size="sm" disabled={!!busy} onClick={() => editEndpoint(endpoint)}>Edit / enable</Button>
              <Button
                variant="outline"
                size="sm"
                disabled={busy === endpoint.id || !endpoint.enabled}
                onClick={() => void disable(endpoint)}
              >
                Disable
              </Button>
            </li>
          ))}
        </ul>
      )}
      <Dialog
        open={adding}
        onOpenChange={(open) => { setAdding(open); if (!open) setApiKey(""); }}
        title={editingEndpoint ? "Edit provider endpoint" : "Add a provider endpoint"}
        description="The key is write-only. It is stored encrypted and never returned."
      >
        <form className="form-stack" onSubmit={save}>
          <label htmlFor="endpoint-name">Name</label>
          <input
            id="endpoint-name"
            required
            maxLength={128}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <label htmlFor="endpoint-url">Base URL</label>
          <input
            id="endpoint-url"
            required
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
            placeholder="https://models.example.com"
          />
          <label htmlFor="endpoint-key">API key</label>
          <input
            id="endpoint-key"
            type="password"
            autoComplete="off"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
          />
          <label className="automation-inline">
            <input
              type="checkbox"
              checked={supportsTools}
              onChange={(e) => setSupportsTools(e.target.checked)}
            />
            The endpoint supports tool calls
          </label>
          <label className="automation-inline">
            <input
              type="checkbox"
              checked={supportsImages}
              onChange={(e) => setSupportsImages(e.target.checked)}
            />
            The endpoint supports images
          </label>
          <label htmlFor="endpoint-context-window">Context window tokens (0 = unknown)</label>
          <input id="endpoint-context-window" type="number" min={0} value={contextWindow} onChange={(e) => setContextWindow(e.target.value)} />
          <label className="automation-inline">
            <input
              type="checkbox"
              checked={allowInsecureHttp}
              onChange={(e) => setAllowInsecureHttp(e.target.checked)}
            />
            This endpoint uses plain HTTP on a local or private host. I confirm
            that explicitly.
          </label>
          <label className="automation-inline">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
            />
            Enable this endpoint now
          </label>
          {error && <Notice>{error}</Notice>}
          <div className="dialog-footer">
            <Button
              type="button"
              variant="outline"
              onClick={() => { setAdding(false); setApiKey(""); }}
              disabled={busy === "save"}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={busy === "save"}>
              {busy === "save" ? "Saving…" : "Save endpoint"}
            </Button>
          </div>
        </form>
      </Dialog>
    </section>
  );
}

function VoiceReadiness() {
  const voice = useQuery({
    queryKey: ["voice-status"],
    queryFn: () => automation("voice.status", {}),
    retry: false,
  });
  const status: VoiceReadiness | undefined = voice.data;
  return (
    <section className="automation-card">
      <header className="automation-card-head">
        <div>
          <strong>Local voice</strong>
          <small className="muted">
            Speech to text and read-aloud run on this machine. Nothing is
            recorded unless you ask for it.
          </small>
        </div>
        <div className="automation-actions">
          <Mic size={16} />
          <Status state={status?.state === "ready" ? "idle" : "error"} />
        </div>
      </header>
      {voice.isError ? (
        <Notice>{voice.error.message}</Notice>
      ) : voice.isPending ? (
        <p role="status">Checking voice readiness…</p>
      ) : status ? (
        <>
          <div className="agent-card-details">
            <span>{READINESS_LABELS[status.state] || status.state}</span>
            <span>
              {status.stt?.configured ? "Speech to text ready" : "No speech model"}
            </span>
            <span>
              {status.tts?.configured ? "Read-aloud ready" : "No voice model"}
            </span>
            {status.autoReadReplies && <span>Reads replies aloud</span>}
          </div>
          {status.prerequisites?.length ? (
            <ul className="automation-prerequisites">
              {status.prerequisites.map((item) => (
                <li key={item}>{item}</li>
              ))}
            </ul>
          ) : null}
        </>
      ) : null}
    </section>
  );
}

function lines(value: string) {
  return value.split("\n").map((line) => line.trim()).filter(Boolean);
}

function Field({ label, value, onChange, required = false, type = "text" }: {
  label: string; value: string; onChange: (value: string) => void; required?: boolean; type?: string;
}) {
  return <label className="form-stack">{label}<input type={type} required={required} autoComplete={type === "password" ? "off" : undefined} value={value} onChange={(e) => onChange(e.target.value)} /></label>;
}

function LinesField({ label, value, onChange }: { label: string; value: string; onChange: (value: string) => void }) {
  return <label className="form-stack">{label}<textarea rows={3} value={value} onChange={(e) => onChange(e.target.value)} /></label>;
}

function Check({ label, checked, onChange }: { label: string; checked: boolean; onChange: (value: boolean) => void }) {
  return <label className="automation-inline"><input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} />{label}</label>;
}

function useConnectionProfiles() {
  return useInfiniteQuery({
    queryKey: ["automation", "profiles", "connection-options"],
    initialPageParam: "",
    queryFn: ({ pageParam }) => automation("profiles.list", { limit: 100, ...(pageParam ? { after: pageParam } : {}) }),
    getNextPageParam: (last) => last.hasMore ? last.nextAfter : undefined,
  });
}

function ProfileSelect({ value, onChange, workspaceId }: { value: string; onChange: (value: string) => void; workspaceId?: string }) {
  const profiles = useConnectionProfiles();
  const items = profiles.data?.pages.flatMap((page) => page.items).filter((profile) => !profile.archived && (!workspaceId || profile.workspaceId === workspaceId)) || [];
  return <>
    <label className="form-stack">Profile<select required value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">Choose a profile</option>
      {items.map((profile) => <option key={profile.id} value={profile.id}>{profile.name} ({profile.id})</option>)}
    </select></label>
    {profiles.isPending && <p role="status">Loading profiles…</p>}
    {profiles.isError && <Notice>{profiles.error.message}</Notice>}
    {!profiles.isPending && !profiles.isError && !items.length && <p className="muted">No eligible profiles. Create a profile with the matching workspace first.</p>}
    {profiles.hasNextPage && <Button type="button" variant="outline" disabled={profiles.isFetchingNextPage} onClick={() => void profiles.fetchNextPage()}>Load more profiles</Button>}
  </>;
}

function MCPConnections() {
  const client = useQueryClient();
  const servers = useInfiniteQuery({
    queryKey: ["automation", "mcp"],
    initialPageParam: "",
    queryFn: ({ pageParam }) => automation("mcp.list", { limit: 100, ...(pageParam ? { after: pageParam } : {}) }),
    getNextPageParam: (last) => last.hasMore ? last.nextAfter : undefined,
  });
  const [editing, setEditing] = useState<{ server?: MCPServer }>();
  return <section className="automation-card">
    <header className="automation-card-head"><div><strong>External MCP servers</strong><small className="muted">Save disabled, explicitly test discovery, then review exact tools for each profile. This does not replace Meshra's own MCP server.</small></div><div className="automation-actions"><Button variant="outline" onClick={() => setEditing({})}>Add MCP server</Button><Button variant="ghost" aria-label="Refresh MCP servers" onClick={() => void servers.refetch()}><RefreshCw size={14} /></Button></div></header>
    {servers.isError && <Notice>{servers.error.message}</Notice>}
    {servers.isPending && <p role="status">Loading MCP servers…</p>}
    {servers.data?.pages.every((page) => !page.items.length) && <p className="muted">No external MCP servers configured.</p>}
    {servers.data?.pages.flatMap((page) => page.items).map((server) => <MCPEntry key={server.id} server={server} onEdit={() => setEditing({ server })} />)}
    {servers.hasNextPage && <Button variant="outline" disabled={servers.isFetchingNextPage} onClick={() => void servers.fetchNextPage()}>Load more servers</Button>}
    <Dialog open={!!editing} onOpenChange={(open) => { if (!open) setEditing(undefined); }} title={editing?.server ? "Edit MCP server" : "Add MCP server"} description="Saving always disables the server, clears discovered tools and requires fresh review. Only existing encrypted secret references are accepted here.">
      {editing && <MCPForm server={editing.server} onSaved={() => { setEditing(undefined); void client.invalidateQueries({ queryKey: ["automation", "mcp"] }); void client.invalidateQueries({ queryKey: ["network-grants"] }); }} />}
    </Dialog>
  </section>;
}

function MCPForm({ server, onSaved }: { server?: MCPServer; onSaved: () => void }) {
  const workspaces = useQuery({ queryKey: ["workspaces"], queryFn: () => api<Workspace[]>("workspaces") });
  const [name, setName] = useState(server?.name || "");
  const [transport, setTransport] = useState<"http" | "stdio">(server?.transport === "stdio" ? "stdio" : "http");
  const [url, setUrl] = useState(server?.url || "");
  const [executable, setExecutable] = useState(server?.executable || "");
  const [args, setArgs] = useState((server?.args || []).join("\n"));
  const [workspaceId, setWorkspaceId] = useState(server?.workspaceId || "");
  const [origins, setOrigins] = useState((server?.allowedOrigins || []).join("\n"));
  const [headers, setHeaders] = useState((server?.secretHeaderRefs || []).join("\n"));
  const [env, setEnv] = useState((server?.secretEnvRefs || []).join("\n"));
  const [allowPrivate, setAllowPrivate] = useState(server?.allowPrivate || false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function save(e: FormEvent) {
    e.preventDefault(); setBusy(true); setError("");
    try {
      await automation("mcp.save", { ...(server ? { expectedRevision: server.revision } : {}), server: { ...(server ? { id: server.id } : {}), name, workspaceId, transport, ...(transport === "http" ? { url } : { executable, args: lines(args) }), allowedOrigins: lines(origins), secretHeaderRefs: lines(headers), secretEnvRefs: lines(env), allowPrivate, enabled: false } });
      onSaved();
    } catch (e) { setError((e as Error).message); } finally { setBusy(false); }
  }
  return <form className="form-stack" onSubmit={save}>
    <Field label="Server name" value={name} onChange={setName} required />
    <label className="form-stack">Workspace<select required value={workspaceId} onChange={(e) => setWorkspaceId(e.target.value)}><option value="">Choose a workspace</option>{workspaces.data?.map((workspace) => <option key={workspace.id} value={workspace.id}>{workspace.name}</option>)}</select></label>
    {workspaces.isError && <Notice>{workspaces.error.message}</Notice>}
    {!workspaces.isPending && !workspaces.data?.length && <p className="muted">Create a workspace before configuring an MCP server.</p>}
    <label className="form-stack">Transport<select value={transport} onChange={(e) => setTransport(e.target.value as "http" | "stdio")}><option value="http">Streamable HTTP</option><option value="stdio">Confined stdio executable</option></select></label>
    {transport === "http" ? <Field label="MCP URL" value={url} onChange={setUrl} required /> : <><Field label="Absolute executable path" value={executable} onChange={setExecutable} required /><LinesField label="Arguments (one per line; no shell command)" value={args} onChange={setArgs} /></>}
    <LinesField label="Allowed exact origins, including OAuth issuer when needed (one per line)" value={origins} onChange={setOrigins} />
    <Check label="I explicitly allow the listed private / loopback origins" checked={allowPrivate} onChange={setAllowPrivate} />
    <LinesField label="Existing secret header references (one identifier per line, not secret values)" value={headers} onChange={setHeaders} />
    <LinesField label="Existing secret environment references (one identifier per line, not secret values)" value={env} onChange={setEnv} />
    {error && <Notice>{error}</Notice>}
    <Button type="submit" disabled={busy || !workspaceId}>{busy ? "Saving…" : "Save disabled configuration"}</Button>
  </form>;
}

function MCPEntry({ server, onEdit }: { server: MCPServer; onEdit: () => void }) {
  const client = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [review, setReview] = useState<"enable" | "remove">();
  const [bindingOpen, setBindingOpen] = useState(false);
  const [authOpen, setAuthOpen] = useState(false);
  const [authorization, setAuthorization] = useState<AutomationResponse<"mcp.authStart">>();
  async function act(action: "test" | "enable" | "disable" | "remove" | "auth" | "authStatus" | "signOut") {
    setBusy(true); setError(""); setMessage("");
    try {
      const request = { id: server.id, expectedRevision: server.revision };
      if (action === "test") { const result = await automation("mcp.test", { id: server.id }); setMessage(`Discovery completed: ${result.tools?.length || 0} tools advertised. Review bindings before use.`); }
      else if (action === "enable" || action === "disable") await automation("mcp.setEnabled", { ...request, enabled: action === "enable" });
      else if (action === "remove") await automation("mcp.remove", request);
      else if (action === "auth") { setAuthorization(await automation("mcp.authStart", { id: server.id })); setAuthOpen(true); }
      else if (action === "authStatus") { const result = await automation("mcp.authStatus", { id: server.id }); setMessage(`Authorization: ${result.auth}${result.expiresAt ? ` · expires ${result.expiresAt}` : ""}`); }
      else { const result = await automation("mcp.signOut", { id: server.id }); setAuthorization(undefined); setMessage(`Signed out locally. Upstream revocation ${result.upstreamRevoked ? "confirmed" : "not confirmed"}.`); }
      setReview(undefined);
      await client.invalidateQueries({ queryKey: ["automation", "mcp"] });
      await client.invalidateQueries({ queryKey: ["network-grants"] });
    } catch (e) { setError((e as Error).message); } finally { setBusy(false); }
  }
  return <article className="automation-card">
    <strong>{server.name}</strong><p className="mono">{server.url || server.executable}</p><p>{server.transport} · {server.enabled ? "Enabled" : "Disabled"} · {server.auth} · revision {server.revision} · workspace {server.workspaceId}</p>
    {Object.entries(server.secretStatus || {}).map(([ref, status]) => <p key={ref}>{ref}: {status}</p>)}
    <div className="automation-actions"><Button size="sm" variant="outline" disabled={busy} onClick={onEdit}>Edit</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => void act("test")}>Test / discover tools</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => server.enabled ? void act("disable") : setReview("enable")}>{server.enabled ? "Disable" : "Review enable"}</Button><Button size="sm" variant="outline" disabled={busy || !server.tools?.length} onClick={() => setBindingOpen(true)}>Review profile tools</Button>
      {server.transport === "http" && <><Button size="sm" variant="outline" disabled={busy} onClick={() => void act("auth")}>Start OAuth sign-in</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => void act("authStatus")}>Check authorization</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => void act("signOut")}>Sign out</Button></>}
      <Button size="sm" variant="ghost" disabled={busy} onClick={() => setReview("remove")}>Remove</Button></div>
    {!server.tools?.length && <p className="muted">No discovered tools. Test explicitly; a missing dependency or authentication is reported, never treated as ready.</p>}
    {server.tools?.map((tool) => <details key={tool.name}><summary>{tool.name} · {tool.digest || "No digest reported"}</summary><p>{tool.description}</p><pre>{JSON.stringify(tool.inputSchema, null, 2)}</pre></details>)}
    {server.bindings?.map((binding) => <p key={binding.profileId}>Profile {binding.profileId} · {binding.enabled ? "Enabled" : "Disabled"} · reviewed server revision {binding.revision}{binding.revision !== server.revision ? " (stale; review again)" : ""} · {binding.tools.map((tool) => tool.name).join(", ")}</p>)}
    {busy && <p role="status">Working…</p>}{error && <Notice>{error}</Notice>}{message && <p role="status">{message}</p>}
    <Dialog open={!!review} onOpenChange={(open) => { if (!open) setReview(undefined); }} title={review === "enable" ? "Review MCP enablement" : "Remove MCP server?"} description={review === "enable" ? "Enabling allows this reviewed server to be used by explicit profile bindings. Stdio launches code; HTTP discloses tool arguments to the listed origins." : "This removes stored server configuration, tools, bindings and local authorization."}>
      {review && <ReviewAction key={review + server.revision} busy={busy} error={error} label={review === "enable" ? "Enable reviewed server" : "Remove server"} onConfirm={() => void act(review)}><p>{server.name} · revision {server.revision} · workspace {server.workspaceId}</p><p className="mono">{server.url || server.executable} {server.args?.join(" ")}</p><p>Exact origins: {server.allowedOrigins?.join(", ") || "None"} · private access: {server.allowPrivate ? "Allowed" : "Not allowed"}</p></ReviewAction>}
    </Dialog>
    <Dialog open={bindingOpen} onOpenChange={setBindingOpen} title="Review exact profile tools" description="Only discovered tool names and their current digests are bound. The server must share the profile workspace.">
      {bindingOpen && <MCPBindingForm key={server.revision} server={server} onSaved={() => { setBindingOpen(false); void client.invalidateQueries({ queryKey: ["automation", "mcp"] }); }} />}
    </Dialog>
    <Dialog open={authOpen} onOpenChange={(open) => { setAuthOpen(open); if (!open) setAuthorization(undefined); }} title="MCP authorization" description="Complete the provider's native OAuth page. No password is entered in Meshra chat. Close this dialog to discard the local authorization link.">
      {authorization && <><p>Expires {authorization.expiresAt}</p><a href={authorization.authorizationUrl} target="_blank" rel="noopener noreferrer" referrerPolicy="no-referrer">Open provider authorization page</a><Button disabled={busy} onClick={() => void act("authStatus")}>Check authorization</Button></>}{error && <Notice>{error}</Notice>}{message && <p role="status">{message}</p>}
    </Dialog>
  </article>;
}

function ReviewAction({ children, busy, error, label, onConfirm }: { children: React.ReactNode; busy: boolean; error: string; label: string; onConfirm: () => void }) {
  const [reviewed, setReviewed] = useState(false);
  return <div className="form-stack">{children}<Check label="I reviewed this exact configuration and its effects" checked={reviewed} onChange={setReviewed} />{error && <Notice>{error}</Notice>}<Button disabled={busy || !reviewed} onClick={onConfirm}>{busy ? "Working…" : label}</Button></div>;
}

function MCPBindingForm({ server, onSaved }: { server: MCPServer; onSaved: () => void }) {
  const [profileId, setProfileId] = useState("");
  const [toolNames, setToolNames] = useState<string[]>([]);
  const [enabled, setEnabled] = useState(false);
  const [reviewed, setReviewed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  function chooseProfile(id: string) {
    setProfileId(id); setReviewed(false);
    const binding = server.bindings?.find((item) => item.profileId === id);
    setToolNames(binding?.tools.map((tool) => tool.name).filter((name) => server.tools?.some((tool) => tool.name === name)) || []);
    setEnabled(binding?.enabled || false);
  }
  async function save(e: FormEvent) {
    e.preventDefault(); setBusy(true); setError("");
    try { await automation("mcp.bind", { serverId: server.id, expectedRevision: server.revision, profileId, toolNames, enabled }); onSaved(); }
    catch (e) { setError((e as Error).message); } finally { setBusy(false); }
  }
  return <form className="form-stack" onSubmit={save}>
    <ProfileSelect value={profileId} onChange={chooseProfile} workspaceId={server.workspaceId} />
    {server.tools?.map((tool) => <Check key={tool.name} label={`${tool.name} · ${tool.digest || "No digest"}`} checked={toolNames.includes(tool.name)} onChange={(checked) => { setToolNames((names) => checked ? [...names, tool.name] : names.filter((name) => name !== tool.name)); setReviewed(false); }} />)}
    <Check label="Enable this profile's selected tools" checked={enabled} onChange={(value) => { setEnabled(value); setReviewed(false); }} />
    <Check label="I reviewed the selected tool schemas, digests and provider disclosure" checked={reviewed} onChange={setReviewed} />
    {error && <Notice>{error}</Notice>}
    <Button type="submit" disabled={busy || !profileId || !toolNames.length || !reviewed}>{busy ? "Saving…" : "Save reviewed tool binding"}</Button>
  </form>;
}
