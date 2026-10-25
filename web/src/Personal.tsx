import { useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Monitor, RefreshCw, ShieldCheck } from "lucide-react";
import { AccountForm } from "./Account";
import { AIStatusPanel } from "./AIStatus";
import { api } from "./api";
import { Connections } from "./Connections";
import { Notice } from "./App";
import { HarnessLogo, harnessName } from "./HarnessLogo";
import { Button } from "./components/ui/button";
import type { ModelOption, PersonalPreferences } from "./types";

type Session = {
  id: string;
  device: string;
  createdAt: string;
  lastSeen: string;
  expiresAt: string;
  current: boolean;
};
type Usage = {
  day: string;
  turns: number;
  dailyTurnLimit: number;
  items: {
    harness: string;
    model: string;
    provider: string;
    turns: number;
    completed: number;
    inputBytes: number;
    outputBytes: number;
    durationMs: number;
    inputTokens: number;
    outputTokens: number;
    reportedTurns: number;
  }[];
};
export function bytes(value: number) {
  return value >= 1048576
    ? (value / 1048576).toFixed(1) + " MiB"
    : (value / 1024).toFixed(1) + " KiB";
}
export function SettingsPanels({ children }: { children: ReactNode }) {
  const [tab, setTab] = useState("appearance");
  return (
    <>
      <div
        className="personal-tabs"
        role="group"
        aria-label="Settings sections"
      >
        {[
          ["appearance", "Appearance"],
          ["access", "Personal access"],
          ["resources", "Resources"],
          ["models", "Models & usage"],
          ["ai-status", "AI status"],
          ["connections", "Connections"],
        ].map(([key, label]) => (
          <button
            key={key}
            aria-pressed={tab === key}
            onClick={() => setTab(key)}
          >
            {label}
          </button>
        ))}
      </div>
      {tab === "appearance" ? (
        children
      ) : tab === "ai-status" ? (
        <AIStatusPanel />
      ) : tab === "models" ? (
        <ModelsUsage />
      ) : tab === "connections" ? (
        <Connections />
      ) : (
        <PersonalControls key={tab} access={tab === "access"} />
      )}
    </>
  );
}
function PersonalControls({ access }: { access: boolean }) {
  const client = useQueryClient();
  const preferences = useQuery({
    queryKey: ["preferences"],
    queryFn: () => api<PersonalPreferences>("preferences"),
  });
  const resources = useQuery({
    queryKey: ["resources"],
    queryFn: () =>
      api<{
        startedAgents: number;
        activeTurns: number;
        queued: number;
        goHeapBytes: number;
        daemonRssBytes: number;
      }>("resources"),
    enabled: !access,
    refetchInterval: 15000,
    gcTime: 0,
  });
  const sessions = useQuery({
    queryKey: ["sessions"],
    queryFn: () => api<Session[]>("sessions"),
    enabled: access,
    gcTime: 0,
  });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setBusy(true);
    setError("");
    setSaved(false);
    const data = new FormData(e.currentTarget);
    const value = { ...preferences.data! };
    if (access) value.displayName = String(data.get("displayName"));
    else
      for (const key of [
        "maxParallelTurns",
        "maxStartedAgents",
        "idleMinutes",
        "dailyTurnLimit",
      ] as const)
        value[key] = Number(data.get(key));
    try {
      await api("preferences/save", { settings: value });
      await client.invalidateQueries({ queryKey: ["preferences"] });
      await client.invalidateQueries({ queryKey: ["resources"] });
      setSaved(true);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function revoke(id?: string) {
    setBusy(true);
    setError("");
    try {
      await api(
        id ? "sessions/revoke" : "sessions/revoke-others",
        id ? { id } : {},
      );
      await sessions.refetch();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="preferences personal-preferences">
      {access && <AccountForm />}
      {preferences.isError && <Notice>{preferences.error.message}</Notice>}
      {preferences.isPending && <p role="status">Loading settings…</p>}
      {error && <Notice>{error}</Notice>}
      {preferences.data && (
        <section>
          <h2>{access ? "Your profile" : "Resource limits"}</h2>
          <p>
            {access
              ? "Your display name is shared across your browser sessions. It is separate from your sign-in username."
              : "Limits apply across chats and agents. Changes persist on this machine and take effect without a restart."}
          </p>
          <form
            onSubmit={save}
            className="personal-form"
            key={JSON.stringify(preferences.data)}
          >
            {access ? (
              <label>
                Display name
                <input
                  name="displayName"
                  maxLength={80}
                  required
                  defaultValue={preferences.data.displayName}
                />
              </label>
            ) : (
              <>
                <label>
                  Concurrent AI turns
                  <input
                    name="maxParallelTurns"
                    type="number"
                    min={1}
                    max={32}
                    required
                    defaultValue={preferences.data.maxParallelTurns}
                  />
                  <small>
                    Additional work waits in the bounded queue. Running work
                    finishes normally.
                  </small>
                </label>
                <label>
                  Started agents
                  <input
                    name="maxStartedAgents"
                    type="number"
                    min={1}
                    max={64}
                    required
                    defaultValue={preferences.data.maxStartedAgents}
                  />
                  <small>
                    Caps readiness slots; chat turns may use an additional
                    temporary harness process.
                  </small>
                </label>
                <label>
                  Stop idle agents after (minutes)
                  <input
                    name="idleMinutes"
                    type="number"
                    min={0}
                    max={1440}
                    required
                    defaultValue={preferences.data.idleMinutes}
                  />
                  <small>
                    0 keeps agents ready. Agents with queued work stay running.
                  </small>
                </label>
                <label>
                  Daily AI turn limit
                  <input
                    name="dailyTurnLimit"
                    type="number"
                    min={0}
                    max={100000}
                    required
                    defaultValue={preferences.data.dailyTurnLimit}
                  />
                  <small>
                    0 means unlimited. Counts started attempts and
                    regenerations; resets at midnight UTC. Queued work still has
                    a deadline.
                  </small>
                </label>
              </>
            )}
            <div className="personal-actions">
              <Button disabled={busy}>
                Save {access ? "profile" : "limits"}
              </Button>
              {saved && <span role="status">Saved</span>}
            </div>
          </form>
        </section>
      )}
      {access ? (
        <section>
          <div className="personal-section-heading">
            <h2>Signed-in browsers</h2>
            <Button
              variant="outline"
              size="sm"
              disabled={busy || !sessions.data?.some((s) => !s.current)}
              onClick={() => void revoke()}
            >
              Sign out other browsers
            </Button>
          </div>
          <p>
            Sessions expire after eight hours. Restarting Meshra signs out every
            browser.
          </p>
          {sessions.isError && <Notice>{sessions.error.message}</Notice>}
          {sessions.isPending && <p role="status">Loading sessions…</p>}
          <ul className="personal-session-list">
            {sessions.data?.map((s) => (
              <li key={s.id}>
                <Monitor size={18} />
                <div>
                  <strong>
                    {s.device}
                    {s.current && (
                      <span className="personal-current">This browser</span>
                    )}
                  </strong>
                  <small>
                    Last active {new Date(s.lastSeen).toLocaleString()} ·
                    Expires {new Date(s.expiresAt).toLocaleTimeString()}
                  </small>
                </div>
                {!s.current && (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy}
                    onClick={() => void revoke(s.id)}
                  >
                    Revoke access
                  </Button>
                )}
              </li>
            ))}
          </ul>
          <p className="policy-note">
            <ShieldCheck size={17} /> One personal administrator account.
            Separate users and sharing permissions are planned for later.
          </p>
        </section>
      ) : (
        <section>
          <div className="personal-section-heading">
            <h2>Current activity</h2>
            <Button
              variant="ghost"
              size="icon"
              aria-label="Refresh resource usage"
              onClick={() => void resources.refetch()}
            >
              <RefreshCw size={16} />
            </Button>
          </div>
          {resources.isError && <Notice>{resources.error.message}</Notice>}
          {resources.data && (
            <dl className="personal-stats">
              <dt>AI turns running</dt>
              <dd>{resources.data.activeTurns}</dd>
              <dt>Queued work</dt>
              <dd>{resources.data.queued}</dd>
              <dt>Started agents</dt>
              <dd>{resources.data.startedAgents}</dd>
              <dt>Daemon resident memory</dt>
              <dd>
                {resources.data.daemonRssBytes
                  ? bytes(resources.data.daemonRssBytes)
                  : "Unavailable"}
              </dd>
              <dt>Go heap in use</dt>
              <dd>{bytes(resources.data.goHeapBytes)}</dd>
            </dl>
          )}
          <p>
            Memory figures cover Meshra itself. Native harness processes use
            additional RAM; these limits bound concurrency rather than enforce a
            total RAM ceiling.
          </p>
        </section>
      )}
    </div>
  );
}
function ModelsUsage() {
  const [harness, setHarness] = useState("codex");
  const models = useQuery({
    queryKey: ["models", harness],
    queryFn: () => api<{ items: ModelOption[] }>("models", { harness }),
    retry: false,
    gcTime: 0,
    staleTime: 60000,
  });
  const usage = useQuery({
    queryKey: ["usage"],
    queryFn: () => api<Usage>("usage"),
    refetchInterval: 30000,
    gcTime: 0,
  });
  return (
    <div className="preferences personal-preferences">
      <section>
        <div className="personal-section-heading">
          <h2>Available models</h2>
          <Button
            variant="outline"
            size="sm"
            disabled={models.isFetching}
            onClick={() => void models.refetch()}
          >
            <RefreshCw size={14} />
            Refresh models
          </Button>
        </div>
        <p>
          Discovered from the installed harness and its native authentication.
          Choose a model when starting a new AI chat; Native default lets the
          harness choose.
        </p>
        <label className="personal-harness-label">
          AI harness
          <select value={harness} onChange={(e) => setHarness(e.target.value)}>
            {["codex", "claude", "opencode", "omp"].map((h) => (
              <option key={h} value={h}>
                {harnessName(h)}
              </option>
            ))}
          </select>
        </label>
        {models.isPending ? (
          <p role="status">Discovering models…</p>
        ) : models.isError ? (
          <Notice>
            {models.error.message}. You can still use Native default.
          </Notice>
        ) : !models.data.items.length ? (
          <p>
            No models were returned. Check the harness’s provider connection.
          </p>
        ) : (
          <ul className="personal-model-list">
            {models.data.items.map((m) => (
              <li key={(m.provider || "") + "/" + m.id}>
                <HarnessLogo id={harness} />
                <div>
                  <strong>{m.name}</strong>
                  <small>
                    {m.provider && m.provider + " · "}
                    {m.id}
                  </small>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
      <section>
        <div className="personal-section-heading">
          <h2>Today’s usage</h2>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Refresh AI usage"
            onClick={() => void usage.refetch()}
          >
            <RefreshCw size={16} />
          </Button>
        </div>
        <p>
          Started attempts since midnight UTC, including failed and regenerated
          replies. History is retained for 30 days; tracking begins with this
          update.
        </p>
        {usage.isError && <Notice>{usage.error.message}</Notice>}
        {usage.data && (
          <>
            <p className="personal-usage-total">
              <strong>{usage.data.turns}</strong> turns
              {usage.data.dailyTurnLimit > 0 &&
                " / " + usage.data.dailyTurnLimit + " daily limit"}
            </p>
            {!usage.data.items.length ? (
              <p>No AI turns recorded today.</p>
            ) : (
              <div className="personal-table-scroll">
                <table className="personal-usage-table">
                  <caption>AI usage by harness and selected model</caption>
                  <thead>
                    <tr>
                      <th scope="col">Model</th>
                      <th scope="col">Turns</th>
                      <th scope="col">Tokens in / out</th>
                      <th scope="col">Output</th>
                    </tr>
                  </thead>
                  <tbody>
                    {usage.data.items.map((v) => (
                      <tr key={v.harness + "/" + v.provider + "/" + v.model}>
                        <th scope="row">
                          {harnessName(v.harness)}
                          <small>{v.model || "Native default"}</small>
                        </th>
                        <td>
                          {v.turns}
                          <small>{v.completed} completed</small>
                        </td>
                        <td>
                          {v.reportedTurns ? (
                            <>
                              {v.inputTokens.toLocaleString()} /{" "}
                              {v.outputTokens.toLocaleString()}
                              <small>
                                Reported for {v.reportedTurns} of {v.turns}{" "}
                                turns
                              </small>
                            </>
                          ) : (
                            "Not reported"
                          )}
                        </td>
                        <td>{bytes(v.outputBytes)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </>
        )}
        <p>
          Token counts appear only when reported by the harness. Provider quota
          is available under AI status where supported; billed costs are not
          inferred. The daily turn limit lives in Resources.
        </p>
      </section>
    </div>
  );
}
