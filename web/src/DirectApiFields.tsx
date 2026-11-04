import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { automation } from "./automation-api";
import { cacheAge, modelCacheOptions } from "./modelCache";

export type DirectApiValue = { provider: string; model: string };

/**
 * Endpoint and model pickers for the Direct API harness. Endpoints are the
 * enabled OpenAI-compatible connections saved in Settings → Connections.
 */
export function DirectApiFields({
  value,
  onChange,
  enabled = true,
}: {
  value: DirectApiValue;
  onChange: (v: DirectApiValue) => void;
  enabled?: boolean;
}) {
  const connections = useQuery({
    queryKey: ["connections"],
    queryFn: () => automation("connections.list", {}),
    enabled,
  });
  const [manual, setManual] = useState(false);
  // TypeSafe System One returns judgments, not text, so it cannot run a turn.
  const endpoints =
    connections.data?.endpoints.filter((e) => e.enabled && e.apiFormat !== "typesafe") ?? [];
  const models = useQuery({
    queryKey: ["connection-models", value.provider],
    queryFn: () => automation("connections.models", { id: value.provider }),
    enabled: enabled && !!value.provider,
    ...modelCacheOptions,
  });
  return (
    <>
      <label htmlFor="direct-endpoint">Endpoint</label>
      <select
        id="direct-endpoint"
        required
        value={value.provider}
        onChange={(e) => {
          setManual(false);
          onChange({ provider: e.target.value, model: "" });
        }}
      >
        <option value="">Select an endpoint…</option>
        {endpoints.map((e) => (
          <option key={e.id} value={e.id}>
            {e.name}
          </option>
        ))}
      </select>
      {connections.isSuccess && !endpoints.length && (
        <p className="picker-hint">
          No enabled endpoint yet. Add an OpenAI-compatible endpoint in Settings → Connections.
        </p>
      )}
      <label htmlFor="direct-model">Model</label>
      {!value.provider || models.isFetching ? (
        <select id="direct-model" required disabled value="">
          <option value="">
            {value.provider ? "Discovering models…" : "Select an endpoint first"}
          </option>
        </select>
      ) : models.data?.length && !manual ? (
        <>
          <select
            id="direct-model"
            required
            value={value.model}
            onChange={(e) => onChange({ ...value, model: e.target.value })}
          >
            <option value="">Select a model…</option>
            {models.data.map((m) => (
              <option key={m.id} value={m.id}>
                {m.name === m.id ? m.id : `${m.name} (${m.id})`}
              </option>
            ))}
          </select>
          <p className="picker-hint">
            {models.data.length} models, cached {cacheAge(models.dataUpdatedAt)} (24 h).{" "}
            <button type="button" className="text-button" onClick={() => void models.refetch()}>
              Refresh
            </button>{" · "}
            <button type="button" className="text-button" onClick={() => setManual(true)}>
              Enter a model ID manually
            </button>
          </p>
        </>
      ) : (
        <>
          <input
            id="direct-model"
            required
            placeholder="Model ID required by this endpoint"
            value={value.model}
            onChange={(e) => onChange({ ...value, model: e.target.value })}
          />
          <p className="picker-hint">
            {models.isError
              ? `Model discovery failed: ${models.error.message}. `
              : models.data && !models.data.length && !manual
                ? "The endpoint listed no models for this API format. "
                : ""}
            <button type="button" className="text-button" onClick={() => { setManual(false); void models.refetch(); }}>
              Retry discovery
            </button>
          </p>
        </>
      )}
    </>
  );
}
