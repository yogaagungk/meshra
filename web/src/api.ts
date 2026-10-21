let csrf = "";
export class APIError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}
export function setCSRF(value: string) {
  csrf = value;
}
export async function api<T>(
  path: string,
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<T> {
  const response = await fetch("/console/api/" + path, {
    method: body === undefined ? "GET" : "POST",
    credentials: "same-origin",
    signal: AbortSignal.timeout(60000),
    headers: {
      ...(body === undefined
        ? {}
        : { "Content-Type": "application/json", "X-Meshra-CSRF": csrf }),
      ...headers,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const value = await response.json();
  if (!response.ok) {
    if (response.status === 401 && path !== "session" && path !== "login")
      window.dispatchEvent(new Event("meshra:expired"));
    throw new APIError(response.status, value.error || "Request failed");
  }
  return value;
}
export const collab = <T>(op: string, body: unknown = {}) =>
  api<T>("collaboration/" + op, body);
export function localRead<T>(key: string, fallback: T): T {
  try {
    const value = JSON.parse(localStorage.getItem("meshra:" + key) || "null");
    if (value === null) return fallback;
    if (Array.isArray(fallback))
      return Array.isArray(value) ? (value as T) : fallback;
    return typeof value === typeof fallback ? (value as T) : fallback;
  } catch {
    return fallback;
  }
}
export function localWrite(key: string, value: unknown) {
  try {
    localStorage.setItem("meshra:" + key, JSON.stringify(value));
  } catch {
    /* Storage can be disabled; the session still works. */
  }
}
export function draftKey(id: string) {
  return "draft:" + location.origin + ":" + id;
}
