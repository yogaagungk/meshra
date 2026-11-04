import type { QueryClient } from "@tanstack/react-query";

/** Discovered model lists stay in memory for 24 hours. */
export const MODEL_CACHE_MS = 24 * 60 * 60 * 1000;

/**
 * Shared options for every "Discovering models" query. The list is served from
 * the in-memory query cache until it is 24 hours old; a person can refresh it
 * sooner with the Refresh controls (`refetch()` ignores the TTL). Nothing is
 * written to disk, and a failed discovery is retried on the next open.
 */
export const modelCacheOptions = {
  retry: false,
  staleTime: MODEL_CACHE_MS,
  gcTime: MODEL_CACHE_MS,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
} as const;

/** Drops cached model lists so the next open discovers them again. */
export function clearModelCache(client: QueryClient, scope?: "connection-models" | "models") {
  const keys = scope ? [scope] : ["connection-models", "models"];
  for (const key of keys) void client.invalidateQueries({ queryKey: [key] });
}

/** "3h ago" style age of a cached list, for the Refresh hint. */
export function cacheAge(updatedAt: number): string {
  if (!updatedAt) return "";
  const minutes = Math.max(0, Math.round((Date.now() - updatedAt) / 60000));
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m ago`;
  return `${Math.round(minutes / 60)}h ago`;
}
