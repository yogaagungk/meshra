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
export async function authenticatedFetch(
  path: string,
  init: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(init.headers);
  if (
    init.method &&
    !["GET", "HEAD"].includes(init.method.toUpperCase()) &&
    !headers.has("X-Meshra-CSRF")
  )
    headers.set("X-Meshra-CSRF", csrf);
  const response = await fetch("/console/api/" + path, {
    ...init,
    credentials: "same-origin",
    signal: init.signal ?? AbortSignal.timeout(60000),
    headers,
  });
  if (response.status === 401 && path !== "session" && path !== "login")
    window.dispatchEvent(new Event("meshra:expired"));
  return response;
}
export async function api<T>(
  path: string,
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<T> {
  const response = await authenticatedFetch(path, {
    method: body === undefined ? "GET" : "POST",
    headers: {
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      ...headers,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const value = await response.json();
  if (!response.ok) {
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
