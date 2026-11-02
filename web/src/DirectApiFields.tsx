import { useQuery } from "@tanstack/react-query";
import { automation } from "./automation-api";

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
  const endpoints = connections.data?.endpoints.filter((e) => e.enabled) ?? [];
  const models = useQuery({
    queryKey: ["connection-models", value.provider],
    queryFn: () => automation("connections.models", { id: value.provider }),
    enabled: enabled && !!value.provider,
    retry: false,
    gcTime: 0,
    staleTime: 60000,
  });
  return (
    <>
      <label htmlFor="direct-endpoint">Endpoint</label>
      <select
        id="direct-endpoint"
        required
        value={value.provider}
        onChange={(e) => onChange({ provider: e.target.value, model: "" })}
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
      {models.data?.length ? (
        <select
          id="direct-model"
          required
          value={value.model}
          onChange={(e) => onChange({ ...value, model: e.target.value })}
        >
          <option value="">Select a model…</option>
          {models.data.map((m) => (
            <option key={m.id} value={m.id}>
              {m.name}
            </option>
          ))}
        </select>
      ) : (
        <input
          id="direct-model"
          required
          placeholder="Model ID required by this endpoint"
          value={value.model}
          onChange={(e) => onChange({ ...value, model: e.target.value })}
        />
      )}
      {models.isFetching && <p role="status">Discovering models…</p>}
      {models.isError && (
        <p className="picker-hint">
          Model discovery is unavailable for this endpoint. Type the model ID instead.
        </p>
      )}
    </>
  );
}
