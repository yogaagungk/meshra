import { useEffect, useRef, useState, type FormEvent } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Archive,
  ArrowUpRight,
  FolderOpen,
  Plus,
  Search,
  ShieldCheck,
  Users,
} from "lucide-react";
import { api, APIError } from "./api";
import {
  automation,
  type Profile,
  type ProfileCapabilities,
  type ProfileConfig,
  type ProfileDraft,
  type ProfileHarness,
  type ProfileProvider,
  type ProfileSwitchPreview,
} from "./automation-api";
import { Empty, Harness, Notice, Status } from "./App";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";
import { DirectoryPicker } from "./DirectoryPicker";
import { harnessName } from "./HarnessLogo";
import { terminal, type ModelOption, type Task, type TraceEvent, type Workspace } from "./types";

type CapabilityKey = keyof ProfileCapabilities;

const capabilityLabels: { key: CapabilityKey; label: string; hint: string }[] = [
  {
    key: "workspaceRead",
    label: "Read workspace files",
    hint: "Read files inside the selected workspace only.",
  },
  {
    key: "workspaceWrite",
    label: "Write workspace files",
    hint: "Staged edits that still need your approval.",
  },
  {
    key: "command",
    label: "Run commands",
    hint: "Commands require a configured confined runtime and the applicable action approvals.",
  },
  {
    key: "browser",
    label: "Browser",
    hint: "Requires an available isolated browser backend and reviewed network access; never your personal browser.",
  },
  {
    key: "computer",
    label: "Computer use",
    hint: "Requires an available private virtual display; never your desktop.",
  },
  {
    key: "memoryWrite",
    label: "Write memory",
    hint: "Save durable notes for this profile.",
  },
  {
    key: "externalMcp",
    label: "External MCP tools",
    hint: "Only operator-reviewed server/tool bindings; enabling this does not connect a server.",
  },
  {
    key: "attachments",
    label: "Attachments",
    hint: "Receive files in this profile's conversations.",
  },
];

const EXECUTION_CAPABILITIES: Record<string, true> = {
  workspaceWrite: true,
  command: true,
  browser: true,
  computer: true,
  externalMcp: true,
};

const EMPTY_CAPABILITIES: ProfileCapabilities = {
  workspaceRead: false,
  workspaceWrite: false,
  command: false,
  browser: false,
  computer: false,
  memoryWrite: false,
  externalMcp: false,
  attachments: false,
};

/**
 * Profiles are portable coworkers: an identity, a workspace, an explicit
 * capability set and the providers it may talk to. Every control here is
 * operator authority; nothing is granted by loading a profile.
 */
export function Profiles() {
  const client = useQueryClient();
  const [search, setSearch] = useState("");
  const [editing, setEditing] = useState<Profile | null>(null);
  const [creating, setCreating] = useState(false);
  const [failure, setFailure] = useState("");
  const profiles = useInfiniteQuery({
    queryKey: ["profiles", "directory"],
    initialPageParam: "",
    queryFn: ({ pageParam }) => automation("profiles.list", { limit: 50, ...(pageParam ? { after: pageParam } : {}) }),
    getNextPageParam: (page) => page.hasMore ? page.nextAfter : undefined,
  });
  const workspaces = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => api<Workspace[]>("workspaces"),
  });
  const items = profiles.data?.pages.flatMap((page) => page.items) || [];
  const rows = items.filter((p) =>
    (p.name + " " + p.id + " " + p.binding.harness)
      .toLowerCase()
      .includes(search.toLowerCase()),
  );
  async function archive(profile: Profile) {
    setFailure("");
    try {
      await automation("profiles.archive", {
        id: profile.id,
        expectedRevision: profile.revision,
        archived: !profile.archived,
      });
      await client.invalidateQueries({ queryKey: ["profiles"] });
    } catch (e) {
      setFailure((e as Error).message);
    }
  }
  return (
    <>
      <div className="section-toolbar">
        <div className="search-field">
          <Search size={17} />
          <input
            aria-label="Search profiles"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search profiles…"
          />
        </div>
        <Button
          onClick={() => {
            setFailure("");
            setCreating(true);
          }}
        >
          <Plus size={16} />
          New profile
        </Button>
      </div>
      {failure && <Notice>{failure}</Notice>}
      {profiles.isError && <Notice>{profiles.error.message}</Notice>}
      {profiles.isPending ? (
        <p role="status">Loading profiles…</p>
      ) : !rows.length ? (
        <Empty
          icon={Users}
          title={items.length ? "No matching profiles" : "No profiles yet"}
          action={
            !items.length && (
              <Button onClick={() => setCreating(true)}>
                <Plus size={16} />
                Create your first profile
              </Button>
            )
          }
        >
          A profile is a portable coworker: its own identity, one workspace,
          and an explicit set of capabilities.
        </Empty>
      ) : (
        <div className="agent-grid">
          {rows.map((profile) => (
            <article className="agent-card" key={profile.id}>
              <div className="agent-card-top">
                <Harness id={profile.binding.harness} />
                <Status state={profile.archived ? "stopped" : "idle"} />
              </div>
              <button
                className="agent-card-title"
                onClick={() => {
                  setFailure("");
                  setEditing(profile);
                }}
              >
                <h2>{profile.name}</h2>
                <ArrowUpRight size={16} />
              </button>
              <p className="agent-workspace">
                {workspaces.data?.find((w) => w.id === profile.workspaceId)
                  ?.name || profile.workspaceId || "No workspace"}
              </p>
              <div className="agent-card-details">
                <span>{profile.binding.harness}</span>
                {profile.binding.config.model && (
                  <span>{profile.binding.config.model}</span>
                )}
                <span>
                  {Object.values(profile.capabilities).filter(Boolean).length}{" "}
                  capabilities
                </span>
                <span>
                  {profile.allowedProfileIds.length} allowed profiles
                </span>
                <span>
                  Notifications {profile.notificationsEnabled ? "opted in" : "muted"}
                </span>
              </div>
              <div className="agent-card-footer">
                <span>Revision {profile.revision}</span>
                <span className="automation-actions">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      setFailure("");
                      setEditing(profile);
                    }}
                  >
                    Edit
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => void archive(profile)}
                  >
                    <Archive size={13} />
                    {profile.archived ? "Restore" : "Archive"}
                  </Button>
                </span>
              </div>
            </article>
          ))}
        </div>
      )}
      {profiles.hasNextPage && (
        <Button variant="outline" disabled={profiles.isFetchingNextPage} onClick={() => void profiles.fetchNextPage()}>
          {profiles.isFetchingNextPage ? "Loading more profiles…" : "Load more profiles"}
        </Button>
      )}
      <ProfileDialog
        open={creating || !!editing}
        profile={editing}
        profiles={items}
        hasMoreProfiles={profiles.hasNextPage}
        loadingMoreProfiles={profiles.isFetchingNextPage}
        onLoadMoreProfiles={() => void profiles.fetchNextPage()}
        onClose={() => {
          setCreating(false);
          setEditing(null);
        }}
      />
    </>
  );
}

function ProfileDialog({
  open,
  profile,
  profiles,
  hasMoreProfiles,
  loadingMoreProfiles,
  onLoadMoreProfiles,
  onClose,
}: {
  open: boolean;
  profile: Profile | null;
  profiles: Profile[];
  hasMoreProfiles: boolean;
  loadingMoreProfiles: boolean;
  onLoadMoreProfiles: () => void;
  onClose: () => void;
}) {
  const client = useQueryClient();
  const [savedProfile, setSavedProfile] = useState(profile);
  const [name, setName] = useState("");
  const [instructions, setInstructions] = useState("");
  const [workspaceId, setWorkspaceId] = useState("");
  const [harness, setHarness] = useState<ProfileHarness>("codex");
  const [model, setModel] = useState("");
  const [provider, setProvider] = useState("");
  const [capabilities, setCapabilities] = useState<ProfileCapabilities>(
    { ...EMPTY_CAPABILITIES },
  );
  const [allowed, setAllowed] = useState<string[]>([]);
  const [notifications, setNotifications] = useState(false);
  const [picker, setPicker] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [preview, setPreview] = useState<ProfileSwitchPreview | null>(null);
  const [shareHistory, setShareHistory] = useState(false);
  const [reviewedPolicy, setReviewedPolicy] = useState("");
  const [notificationPermission, setNotificationPermission] = useState<
    NotificationPermission | "unsupported"
  >(() => typeof Notification === "undefined" ? "unsupported" : Notification.permission);
  const [notificationError, setNotificationError] = useState("");
  const lifecycle = useRef(0);

  const harnesses = useQuery({
    queryKey: ["harnesses"],
    queryFn: () => api<{ id: string }[]>("harnesses"),
    enabled: open,
    gcTime: 0,
  });
  const workspaces = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => api<Workspace[]>("workspaces"),
    enabled: open,
  });
  const models = useQuery({
    queryKey: ["models", harness],
    queryFn: () => api<{ items: ModelOption[] }>("models", { harness }),
    enabled: open && harness !== "openai" && harness !== "acp",
    retry: false,
    gcTime: 0,
    staleTime: 60000,
  });

  const connections = useQuery({
    queryKey: ["connections"],
    queryFn: () => automation("connections.list", {}),
    enabled: open && (harness === "openai" || harness === "acp"),
    gcTime: 0,
  });

  useEffect(() => {
    lifecycle.current++;
    setSavedProfile(profile);
    setBusy(false);
    setError(null);
    setPreview(null);
    setShareHistory(false);
    setReviewedPolicy("");
    setNotificationError("");
    setPicker(false);
    if (!open) {
      setName("");
      setInstructions("");
      return;
    }
    if (profile) {
      setName(profile.name);
      setInstructions(profile.instructions);
      setWorkspaceId(profile.workspaceId);
      setHarness(profile.binding.harness);
      setModel(profile.binding.config.model || "");
      setProvider(profile.binding.config.provider || "");
      setCapabilities({ ...profile.capabilities });
      setAllowed([...profile.allowedProfileIds]);
      setNotifications(profile.notificationsEnabled);
    } else {
      setName("");
      setInstructions("");
      setWorkspaceId("");
      setHarness("codex");
      setModel("");
      setProvider("");
      setCapabilities({ ...EMPTY_CAPABILITIES });
      setAllowed([]);
      setNotifications(false);
    }
  }, [open, profile]);

  useEffect(() => () => { lifecycle.current++; }, []);

  useEffect(() => {
    const update = () => setNotificationPermission(
      typeof Notification === "undefined" ? "unsupported" : Notification.permission,
    );
    window.addEventListener("focus", update);
    return () => window.removeEventListener("focus", update);
  }, []);

  const connectionHarness = harness === "openai" || harness === "acp";
  const binding: ProfileProvider = {
    harness,
    config: {
      ...(model ? { model } : {}),
      ...(provider ? { provider } : {}),
    },
    ...(connectionHarness && provider ? { connectionId: provider } : {}),
  };
  const bindingKey = JSON.stringify(binding);
  const bindingChanged = !!savedProfile && (
    savedProfile.binding.harness !== harness ||
    (savedProfile.binding.config.model || "") !== model ||
    (savedProfile.binding.config.provider || "") !== provider ||
    (savedProfile.binding.connectionId || "") !== (binding.connectionId || "")
  );
  const widenedCapabilities = capabilityLabels.filter(
    ({ key }) => capabilities[key] && !savedProfile?.capabilities[key],
  );
  const widenedProfiles = allowed.filter(
    (id) => !savedProfile?.allowedProfileIds.includes(id),
  );
  const workspaceChanged = !!savedProfile && workspaceId !== savedProfile.workspaceId;
  const needsPolicyReview = widenedCapabilities.length > 0 || widenedProfiles.length > 0 || workspaceChanged;
  const policyKey = JSON.stringify({ capabilities, allowed, workspaceId, binding });
  const policyReviewed = !needsPolicyReview || reviewedPolicy === policyKey;
  const selectedConnection = harness === "openai"
    ? connections.data?.endpoints.find((item) => item.id === provider)
    : connections.data?.acp.find((item) => item.id === provider);
  const formValid = !!name.trim() && !!workspaceId &&
    new TextEncoder().encode(name.trim()).length <= 128 &&
    new TextEncoder().encode(instructions).length <= 16384 &&
    (!connectionHarness || !!provider);

  useEffect(() => {
    setPreview(null);
    setShareHistory(false);
  }, [bindingKey]);

  async function enableNotifications() {
    setNotificationError("");
    if (typeof Notification === "undefined") {
      setNotificationPermission("unsupported");
      return;
    }
    const session = lifecycle.current;
    try {
      const permission = await Notification.requestPermission();
      if (session !== lifecycle.current) return;
      setNotificationPermission(permission);
      if (permission === "granted") setNotifications(true);
      else setNotificationError("Browser notifications were not enabled. In-app attention remains available.");
    } catch (e) {
      if (session === lifecycle.current) setNotificationError((e as Error).message);
    }
  }

  async function chooseFolder(path: string) {
    setError(null);
    const known = workspaces.data?.find((w) => w.path === path);
    if (known) {
      setWorkspaceId(known.id);
      return;
    }
    try {
      const created = await api<Workspace>("workspaces/add", {
        name: path.split("/").filter(Boolean).pop() || "workspace",
        path,
      });
      await client.invalidateQueries({ queryKey: ["workspaces"] });
      setWorkspaceId(created.id);
    } catch (e) {
      setError(e as Error);
    }
  }

  async function save(e: FormEvent) {
    e.preventDefault();
    if (busy || bindingChanged || !formValid || !policyReviewed) return;
    setBusy(true);
    setError(null);
    const session = lifecycle.current;
    try {
      await automation("profiles.save", {
        ...(savedProfile ? { id: savedProfile.id, expectedRevision: savedProfile.revision } : {}),
        name: name.trim(),
        instructions,
        workspaceId,
        binding,
        ...(savedProfile?.avatarAttachmentId ? { avatarAttachmentId: savedProfile.avatarAttachmentId } : {}),
        capabilities,
        allowedProfileIds: allowed,
        notificationsEnabled: notifications,
      });
      await client.invalidateQueries({ queryKey: ["profiles"] });
      if (session === lifecycle.current) onClose();
    } catch (e) {
      if (session === lifecycle.current) setError(e as Error);
    } finally {
      if (session === lifecycle.current) setBusy(false);
    }
  }

  async function loadPreview() {
    if (!savedProfile || busy) return;
    setBusy(true);
    setError(null);
    setPreview(null);
    setShareHistory(false);
    const session = lifecycle.current;
    try {
      const result = await automation("profiles.switchPreview", {
        id: savedProfile.id,
        expectedRevision: savedProfile.revision,
        binding,
      });
      if (session === lifecycle.current) setPreview(result);
    } catch (e) {
      if (session === lifecycle.current) setError(e as Error);
    } finally {
      if (session === lifecycle.current) setBusy(false);
    }
  }

  async function switchProvider() {
    if (!savedProfile || !preview || !shareHistory || busy) return;
    setBusy(true);
    setError(null);
    const session = lifecycle.current;
    try {
      const updated = await automation("profiles.switch", {
        id: savedProfile.id,
        expectedRevision: savedProfile.revision,
        binding,
        shareHistory: true,
      });
      if (session !== lifecycle.current) return;
      setSavedProfile(updated);
      setPreview(null);
      setShareHistory(false);
      await client.invalidateQueries({ queryKey: ["profiles"] });
    } catch (e) {
      if (session === lifecycle.current) {
        setError(e as Error);
        setPreview(null);
        setShareHistory(false);
      }
    } finally {
      if (session === lifecycle.current) setBusy(false);
    }
  }

  return (
    <>
      <Dialog
        open={open}
        onOpenChange={() => !busy && onClose()}
        title={profile ? `Edit ${profile.name}` : "New profile"}
        description="A profile carries one identity, one workspace, and explicit capabilities."
        wide
      >
        <form className="form-stack" onSubmit={save}>
          <fieldset disabled={busy} className="form-stack" style={{ border: 0, padding: 0, margin: 0 }}>
          <label htmlFor="profile-name">Name</label>
          <input
            id="profile-name"
            required
            maxLength={128}
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Release reviewer"
          />
          <label htmlFor="profile-instructions">Instructions</label>
          <textarea
            id="profile-instructions"
            rows={5}
            maxLength={16384}
            value={instructions}
            onChange={(e) => setInstructions(e.target.value)}
            placeholder="What this profile is for, and how it should work."
          />
          <small className="muted">
            Name: {new TextEncoder().encode(name.trim()).length}/128 bytes ·
            Instructions: {new TextEncoder().encode(instructions).length}/16384 bytes.
            Instructions are sent to the selected provider. Do not include credentials.
          </small>
          <label htmlFor="profile-workspace">Workspace</label>
          <div className="automation-inline">
            <select
              id="profile-workspace"
              required
              value={workspaceId}
              onChange={(e) => setWorkspaceId(e.target.value)}
            >
              <option value="">Select a workspace</option>
              {workspaces.data?.map((w) => (
                <option value={w.id} key={w.id}>
                  {w.name}
                </option>
              ))}
            </select>
            <Button
              type="button"
              variant="outline"
              onClick={() => setPicker(true)}
            >
              <FolderOpen size={15} />
              Choose folder
            </Button>
          </div>
          {workspaces.isError && <Notice>{workspaces.error.message}</Notice>}
          <label htmlFor="profile-harness">Provider</label>
          <select
            id="profile-harness"
            value={harness}
            onChange={(e) => {
              setHarness(e.target.value as ProfileHarness);
              setModel("");
              setProvider("");
            }}
          >
            {Array.from(new Set([
              harness,
              ...(harnesses.data?.map((h) => h.id) || []),
            ])).map((id) => (
              <option value={id} key={id}>
                {harnessName(id)}
              </option>
            ))}
          </select>
          {harnesses.isError && <Notice>{harnesses.error.message}</Notice>}
          {connectionHarness && (
            <>
              <label htmlFor="profile-connection">Saved connection</label>
              <select
                id="profile-connection"
                required
                value={provider}
                onChange={(e) => { setProvider(e.target.value); setModel(""); }}
              >
                <option value="">Select a saved connection</option>
                {provider && !selectedConnection && <option value={provider}>{provider} (unavailable)</option>}
                {(harness === "openai" ? connections.data?.endpoints : connections.data?.acp)?.map((item) => (
                  <option key={item.id} value={item.id}>
                    {item.name} · {item.enabled ? "enabled" : "disabled"}
                  </option>
                ))}
              </select>
              {connections.isError && <Notice>{connections.error.message}</Notice>}
              <small className="muted">Configure saved endpoints and ACP agents in Settings → Connections. Saving a profile does not enable or authenticate a connection.</small>
            </>
          )}
          <label htmlFor="profile-model">Model</label>
          {connectionHarness ? (
            <input
              id="profile-model"
              value={model}
              maxLength={200}
              onChange={(e) => setModel(e.target.value)}
              placeholder={harness === "openai" ? "Model ID required by this endpoint" : "ACP agent default"}
            />
          ) : (
            <select
              id="profile-model"
              value={JSON.stringify([provider, model])}
              onChange={(e) => {
                const [nextProvider, nextModel] = JSON.parse(e.target.value) as [string, string];
                setModel(nextModel);
                setProvider(nextProvider);
              }}
            >
              <option value={JSON.stringify(["", ""])}>Native default</option>
              {model && !models.data?.items.some((m) => m.id === model && (m.provider || "") === provider) && (
                <option value={JSON.stringify([provider, model])}>{provider ? `${provider} · ` : ""}{model} (saved)</option>
              )}
              {models.data?.items.map((m) => (
                <option
                  key={JSON.stringify([m.provider || "", m.id])}
                  value={JSON.stringify([m.provider || "", m.id])}
                >
                  {m.provider ? m.provider + " · " : ""}{m.name}
                </option>
              ))}
            </select>
          )}
          {!connectionHarness && models.isError && (
            <p className="picker-hint">
              Model discovery unavailable. The native default stays available.
            </p>
          )}
          <section className="automation-card" aria-label="Provider and policy disclosure">
            <strong>Provider and policy disclosure</strong>
            <p>
              {harnessName(harness)} · {provider || "native configured provider"} · {model || "provider default model"}.
              {harness === "openai" && selectedConnection && "baseUrl" in selectedConnection
                ? ` Endpoint: ${selectedConnection.baseUrl}.`
                : harness === "acp" && selectedConnection && "executable" in selectedConnection
                  ? ` Saved ACP executable: ${selectedConnection.executable}.`
                  : " Native account authentication and provider routing stay managed by the selected harness."}
            </p>
            <p className="muted">
              Profile instructions, submitted messages, selected attachments and granted context can reach this provider.
              Conversation history is bounded; a provider switch requires a separate disclosure below.
              Capabilities authorize Meshra's brokered tools, not unrestricted native tools or access to your desktop.
              Saving does not start a binding, grant network access, bind MCP tools, or approve an action.
              Approvals, authentication and human takeover stay in this console.
            </p>
          </section>
          {open && (
            <ProfileGeneration
              key={profile?.id || "new"}
              harness={harness}
              config={binding.config}
              workspaceId={workspaceId}
              disabled={busy || (connectionHarness && !provider)}
              onApply={(draft) => { setName(draft.name); setInstructions(draft.instructions); }}
            />
          )}
          <fieldset className="automation-capabilities">
            <legend>Capabilities</legend>
            {capabilityLabels.map((entry) => {
              const locked = !savedProfile && !!EXECUTION_CAPABILITIES[entry.key];
              return (
                <label key={entry.key} className="automation-capability">
                  <input
                    type="checkbox"
                    disabled={locked}
                    checked={capabilities[entry.key]}
                    onChange={(e) => {
                      const next = { ...capabilities };
                      next[entry.key] = e.target.checked;
                      setCapabilities(next);
                    }}
                  />
                  <span>
                    <strong>{entry.label}</strong>
                    <small>{entry.hint}</small>
                  </span>
                </label>
              );
            })}
            <small>
              Execution and external capabilities must start disabled. Later opt-in still requires
              the corresponding configured backend and operator-reviewed tool/network grants.
            </small>
          </fieldset>
          <fieldset className="automation-capabilities">
            <legend>Allowed profiles</legend>
            <small>These are outbound profile communication grants, not conversation participants. At most 64 profiles may be allowed.</small>
            {profiles
              .filter((p) => (!p.archived || allowed.includes(p.id)) && p.id !== profile?.id)
              .map((p) => (
                <label key={p.id} className="automation-capability">
                  <input
                    type="checkbox"
                    disabled={!allowed.includes(p.id) && (p.archived || allowed.length >= 64)}
                    checked={allowed.includes(p.id)}
                    onChange={(e) =>
                      setAllowed((v) =>
                        e.target.checked
                          ? [...v, p.id]
                          : v.filter((id) => id !== p.id),
                      )
                    }
                  />
                  <span>
                    <strong>{p.name}</strong>
                    <small>{p.id}{p.archived ? " · archived; remove this grant before saving" : ""}</small>
                  </span>
                </label>
              ))}
            {allowed.filter((id) => !profiles.some((p) => p.id === id)).map((id) => (
              <label key={id} className="automation-capability">
                <input type="checkbox" checked onChange={() => setAllowed((ids) => ids.filter((value) => value !== id))} />
                <span><strong>{id}</strong><small>Existing grant outside the loaded profile list. Uncheck to remove it.</small></span>
              </label>
            ))}
            {!profiles.some((p) => !p.archived && p.id !== profile?.id) && (
              <small>No other profiles in the loaded list.</small>
            )}
            {hasMoreProfiles && (
              <Button type="button" variant="outline" disabled={loadingMoreProfiles} onClick={onLoadMoreProfiles}>
                {loadingMoreProfiles ? "Loading profiles…" : "Load more profile choices"}
              </Button>
            )}
          </fieldset>
          <fieldset className="automation-capabilities">
            <legend>Browser notifications · off by default</legend>
            <label className="automation-inline">
              <input
                type="checkbox"
                checked={notifications}
                onChange={(e) => {
                  if (!e.target.checked) setNotifications(false);
                  else if (notificationPermission === "granted") setNotifications(true);
                  else void enableNotifications();
                }}
              />
              {notifications ? "Notify for this profile" : "Muted for this profile"}
            </label>
            <small>
              Opt in per profile to completion, error and attention notifications, then save.
              Browser permission: {notificationPermission}. Notifications never approve actions
              or expose approval controls outside this console.
            </small>
            {notificationPermission !== "granted" && (
              <>
                <p className="muted">
                  {notificationPermission === "unsupported"
                    ? "This browser does not support desktop notifications."
                    : notificationPermission === "denied"
                      ? "Notifications are blocked. Change this site's browser permission to enable them."
                      : "Browser notification permission is requested only after you explicitly opt in."}
                  {" "}In-app attention badges remain available.
                </p>
                {notificationPermission === "default" && (
                  <Button type="button" variant="outline" onClick={() => void enableNotifications()}>
                    Opt in and request browser permission
                  </Button>
                )}
              </>
            )}
            {notificationError && <Notice>{notificationError}</Notice>}
          </fieldset>
          {needsPolicyReview && (
            <section className="automation-card" aria-label="Permission widening review">
              <strong><ShieldCheck size={15} /> Review authority changes</strong>
              <ul>
                {widenedCapabilities.map(({ key, label, hint }) => <li key={key}>Enable {label}: {hint}</li>)}
                {widenedProfiles.map((id) => <li key={id}>Allow outbound communication to {profiles.find((p) => p.id === id)?.name || id} ({id})</li>)}
                {workspaceChanged && <li>Move workspace authority from {savedProfile?.workspaceId} to {workspaceId || "(not selected)"}.</li>}
              </ul>
              <p className="muted">New permissions and workspace changes require the profile's outstanding work to settle. Capability reductions can invalidate active tool calls.</p>
              <label className="automation-inline">
                <input type="checkbox" checked={policyReviewed} onChange={(e) => setReviewedPolicy(e.target.checked ? policyKey : "")} />
                I reviewed these grants, workspace and provider disclosure.
              </label>
            </section>
          )}
          {bindingChanged && (
            <section className="automation-card" aria-label="Provider switch review">
              <strong><ShieldCheck size={15} /> Provider change requires sharing review</strong>
              <p>
                A switch creates new bindings; native sessions remain with the old provider.
                Only the provider is changed by this action. Unsaved form edits are retained;
                save them separately afterward. The preview shows persisted instructions, not unsaved edits.
              </p>
              <Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void loadPreview()}>
                Preview exact handoff
              </Button>
              {preview && (
                <>
                  <p>{preview.conversations.length} conversations · handoff limit {preview.handoffByteLimit} bytes per conversation.</p>
                  <details>
                    <summary>Instructions to be shared</summary>
                    <pre className="raw-payload">{preview.instructions || "(none)"}</pre>
                  </details>
                  {preview.conversations.map((conversation) => (
                    <details key={conversation.conversationId}>
                      <summary>{conversation.title || conversation.conversationId} · {conversation.handoff.length} exchanges · {conversation.contexts.length} contexts</summary>
                      <pre className="raw-payload">{JSON.stringify(conversation.handoff, null, 2)}</pre>
                      <ul>
                        {conversation.contexts.map((context) => (
                          <li key={context.contextId + ":" + context.version}>
                            {context.title} · {context.contextId} v{context.version} · {context.size} bytes · SHA-256 {context.sha256}
                          </li>
                        ))}
                      </ul>
                    </details>
                  ))}
                  <label className="automation-inline">
                    <input type="checkbox" checked={shareHistory} onChange={(e) => setShareHistory(e.target.checked)} />
                    I authorize sharing these instructions, bounded exchanges and pinned contexts with the selected provider.
                  </label>
                  <Button type="button" disabled={busy || !shareHistory} onClick={() => void switchProvider()}>
                    Switch provider only
                  </Button>
                </>
              )}
            </section>
          )}
          {error && (
            <Notice>
              {error.message}
              {error instanceof APIError && error.status === 409
                ? " The profile revision, outstanding work or sharing preview may have changed. Review the error before trying again."
                : error instanceof APIError && error.status === 503
                  ? " The requested backend or dependency is unavailable. Check Settings → Connections and runtime readiness."
                  : ""}
            </Notice>
          )}
          <div className="dialog-footer">
            <Button
              type="button"
              variant="outline"
              onClick={onClose}
              disabled={busy}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={busy || !formValid || !policyReviewed || bindingChanged}>
              {busy ? "Saving…" : profile ? "Save profile" : "Create profile"}
            </Button>
          </div>
          </fieldset>
        </form>
      </Dialog>
      <DirectoryPicker
        open={picker}
        onOpenChange={setPicker}
        onSelect={(path) => void chooseFolder(path)}
      />
    </>
  );
}

function ProfileGeneration({
  harness,
  config,
  workspaceId,
  disabled,
  onApply,
}: {
  harness: ProfileHarness;
  config: ProfileConfig;
  workspaceId: string;
  disabled: boolean;
  onApply: (draft: ProfileDraft) => void;
}) {
  const [description, setDescription] = useState("");
  const [taskId, setTaskId] = useState("");
  const [starting, setStarting] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const [startError, setStartError] = useState("");
  const [error, setError] = useState("");
  const [raw, setRaw] = useState("");
  const [applied, setApplied] = useState(false);
  const lifecycle = useRef(0);
  useEffect(() => () => { lifecycle.current++; }, []);

  const task = useQuery({
    queryKey: ["profiles", "generation", taskId, "status"],
    queryFn: () => api<Task>("tasks/" + encodeURIComponent(taskId)),
    enabled: !!taskId,
    gcTime: 0,
    retry: false,
    refetchInterval: (query) => query.state.data && terminal(query.state.data.state) ? false : 2000,
  });
  const events = useQuery({
    queryKey: ["profiles", "generation", taskId, "events"],
    queryFn: () => api<TraceEvent[]>("tasks/" + encodeURIComponent(taskId) + "/events?after=0"),
    enabled: !!taskId,
    gcTime: 0,
    retry: false,
    refetchInterval: task.data && terminal(task.data.state) ? false : 2000,
  });
  useEffect(() => {
    if (task.data && terminal(task.data.state)) void events.refetch();
  }, [task.data?.state, events.refetch]);
  const draft = useQuery({
    queryKey: ["profiles", "generation", taskId, "draft"],
    queryFn: () => automation("profiles.generationDraft", { taskId }),
    enabled: !!task.data && terminal(task.data.state),
    gcTime: 0,
    retry: false,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
  useEffect(() => {
    if (!draft.data) return;
    setRaw(draft.data.raw || "");
    setApplied(false);
    setError("");
  }, [draft.data]);

  async function generate() {
    if (disabled || starting || !workspaceId || !description.trim() ||
      new TextEncoder().encode(description).length > 16384 ||
      (taskId && (!task.data || !terminal(task.data.state)))) return;
    setStarting(true);
    setError("");
    setStartError("");
    setApplied(false);
    const session = lifecycle.current;
    try {
      const accepted = await automation("profiles.generate", {
        description,
        harness,
        config,
        workspaceId,
      });
      if (session !== lifecycle.current) return;
      setRaw("");
      setTaskId(accepted.taskId);
      setStartError(accepted.startError || "");
    } catch (e) {
      if (session === lifecycle.current) setError((e as Error).message);
    } finally {
      if (session === lifecycle.current) setStarting(false);
    }
  }

  async function cancelGeneration() {
    if (!taskId || cancelling) return;
    setCancelling(true);
    setError("");
    const session = lifecycle.current;
    try {
      await api("tasks/" + encodeURIComponent(taskId) + "/cancel", {});
      if (session === lifecycle.current) {
        await task.refetch();
        await events.refetch();
      }
    } catch (e) {
      if (session === lifecycle.current) setError((e as Error).message);
    } finally {
      if (session === lifecycle.current) setCancelling(false);
    }
  }

  function applyReviewedDraft() {
    setError("");
    setApplied(false);
    try {
      if (new TextEncoder().encode(raw).length > 24 * 1024)
        throw new Error("Draft exceeds 24 KiB. Edit it explicitly before review.");
      const value: unknown = JSON.parse(raw);
      if (typeof value !== "object" || value === null || Array.isArray(value))
        throw new Error("Draft must be one JSON object with name and instructions only.");
      const fields = Object.keys(value);
      if (fields.length !== 2 || !fields.includes("name") || !fields.includes("instructions") ||
        !("name" in value) || typeof value.name !== "string" ||
        !("instructions" in value) || typeof value.instructions !== "string")
        throw new Error("Draft must contain exactly two string fields: name and instructions. No permissions or settings are accepted.");
      if (!value.name.trim() || new TextEncoder().encode(value.name).length > 128 ||
        /[\u0000-\u001f\u007f-\u009f]/.test(value.name))
        throw new Error("Draft name must be 1–128 UTF-8 bytes without control characters.");
      if (!value.instructions.trim() || new TextEncoder().encode(value.instructions).length > 16384 ||
        /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/.test(value.instructions))
        throw new Error("Draft instructions must be 1–16384 UTF-8 bytes without invalid control characters.");
      onApply({ name: value.name, instructions: value.instructions });
      setApplied(true);
    } catch (e) {
      setError((e as Error).message);
    }
  }

  const active = !!taskId && (!task.data || !terminal(task.data.state));
  return (
    <details className="automation-card">
      <summary>Generate an editable name and instructions draft</summary>
      <p className="muted">
        Your description is sent to the selected provider in a disposable empty workspace,
        with verified tool-free mode including native reads. No profile permissions are inherited.
        Unsupported adapters fail visibly. Generation creates a task, not a profile;
        closing this editor does not cancel an accepted task.
      </p>
      <label htmlFor="profile-generation-description">Describe the profile</label>
      <textarea
        id="profile-generation-description"
        rows={3}
        maxLength={16384}
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        placeholder="Describe the role and working instructions. Do not include credentials."
        disabled={starting || active}
      />
      <small className="muted">{new TextEncoder().encode(description).length}/16384 bytes · select a workspace and provider first.</small>
      <div className="automation-actions">
        <Button
          type="button"
          variant="outline"
          disabled={disabled || starting || active || !description.trim() || !workspaceId || new TextEncoder().encode(description).length > 16384}
          onClick={() => void generate()}
        >
          {starting ? "Submitting generation…" : taskId ? "Generate another draft" : "Generate draft"}
        </Button>
        {active && (
          <Button type="button" variant="outline" disabled={cancelling} onClick={() => void cancelGeneration()}>
            {cancelling ? "Cancelling…" : "Cancel generation task"}
          </Button>
        )}
      </div>
      {taskId && (
        <section aria-label="Generation task">
          <p role="status">Task {taskId} · {task.data?.state || "Loading status…"}</p>
          {task.data?.error && <Notice>{task.data.error}</Notice>}
          {startError && <Notice>Task accepted, but starting the generation adapter failed: {startError}. It has not produced a draft.</Notice>}
          {task.isError && <Notice>{task.error.message}</Notice>}
          {events.isError && <Notice>{events.error.message}</Notice>}
          {draft.isError && <Notice>{draft.error.message}</Notice>}
          {!!events.data?.length && (
            <details>
              <summary>Task lifecycle events</summary>
              <ol className="trace-list">
                {events.data.slice(-40).map((event) => (
                  <li key={event.id}><div><strong>{event.type}</strong><time>{event.time}</time></div></li>
                ))}
              </ol>
            </details>
          )}
          {draft.data?.error && <Notice>Draft rejected: {draft.data.error}. No fields were changed. Review or explicitly edit the output below.</Notice>}
          {draft.data && (
            <>
              <p>{draft.data.draft ? "The server accepted the draft shape. Review before using it." : "No valid generated draft is available. You can edit the JSON explicitly or generate again."}</p>
              <label htmlFor="profile-generation-json">Editable draft JSON · name and instructions only</label>
              <textarea
                id="profile-generation-json"
                rows={8}
                value={raw}
                onChange={(e) => { setRaw(e.target.value); setApplied(false); setError(""); }}
                spellCheck={false}
              />
              <small className="muted">
                {new TextEncoder().encode(raw).length}/24576 bytes. Invalid output is not repaired or applied automatically.
                Using this draft replaces only the editable name and instructions above. Permissions,
                workspace, provider, notifications and allowed profiles are unchanged.
              </small>
              <Button type="button" variant="outline" disabled={disabled || !raw} onClick={applyReviewedDraft}>
                Use reviewed name and instructions
              </Button>
              {applied && <p role="status">Draft copied to the form. Review those fields, then save the profile explicitly. Nothing has been saved yet.</p>}
            </>
          )}
        </section>
      )}
      {error && <Notice>{error}</Notice>}
    </details>
  );
}
