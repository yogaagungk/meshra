import { automation } from "./automation-api";
import { localRead, localWrite } from "./api";

/** Lead routing is a per-browser convenience: it only narrows who receives a send. */
export type RoutingSettings = { lead: string; purpose: string };
export type RoutedNote = { by: string; reason: string; ids: string[] };

export const noRouting: RoutingSettings = { lead: "", purpose: "" };

export function readRouting(id: string): RoutingSettings {
  const value = localRead<Partial<RoutingSettings>>("routing:" + id, {});
  return {
    lead: typeof value.lead === "string" ? value.lead : "",
    purpose: typeof value.purpose === "string" ? value.purpose.slice(0, 1024) : "",
  };
}
export function writeRouting(id: string, settings: RoutingSettings) {
  localWrite("routing:" + id, settings);
}
export function readRouteNotes(id: string): Record<string, RoutedNote> {
  return localRead<Record<string, RoutedNote>>("route-notes:" + id, {});
}
export function writeRouteNote(id: string, messageId: string, note: RoutedNote) {
  const notes = readRouteNotes(id);
  const keep = Object.entries(notes).slice(-199);
  localWrite("route-notes:" + id, { ...Object.fromEntries(keep), [messageId]: note });
}

export class RoutingError extends Error {}

type Call = typeof automation;

/**
 * Asks the lead to pick recipients and waits for its verified answer. The
 * relay validates the answer against the current participants, so the result
 * can only name members of this conversation.
 */
export async function routeWithLead(
  input: { conversationId: string; lead: string; text: string; purpose: string; signal?: AbortSignal },
  call: Call = automation,
  options: { intervalMs?: number; timeoutMs?: number } = {},
): Promise<{ ids: string[]; reason: string }> {
  const interval = options.intervalMs ?? 1000;
  const deadline = Date.now() + (options.timeoutMs ?? 150000);
  const started = await call("routing.route", {
    conversationId: input.conversationId,
    leadAgentId: input.lead,
    text: input.text,
    purpose: input.purpose || undefined,
  });
  if (started.startError) throw new RoutingError(started.startError);
  for (;;) {
    if (input.signal?.aborted) throw new RoutingError("Routing was cancelled.");
    const result = await call("routing.result", { taskId: started.taskId });
    if (result.error) throw new RoutingError(result.error);
    if (result.state === "completed") return { ids: result.agentIds || [], reason: result.reason || "" };
    if (["failed", "timed_out", "cancelled"].includes(result.state))
      throw new RoutingError("The lead could not route this message.");
    if (Date.now() > deadline) throw new RoutingError("The lead took too long to choose recipients.");
    await new Promise((resolve) => setTimeout(resolve, interval));
  }
}
