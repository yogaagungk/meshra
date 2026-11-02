import { useQuery } from "@tanstack/react-query";
import { automation, type Profile } from "./automation-api";

/** Active profiles keyed by ID, so panels show names instead of identifiers. */
export function useProfiles() {
  const query = useQuery({
    queryKey: ["profiles", "all"],
    queryFn: () => automation("profiles.list", { limit: 100 }),
    staleTime: 15000,
  });
  const items: Profile[] = query.data?.items || [];
  const byId = new Map(items.map((p) => [p.id, p]));
  return {
    items,
    byId,
    isPending: query.isPending,
    name: (id?: string) => (id ? byId.get(id)?.name || "Unknown profile" : "No profile"),
  };
}

/** Switches the Automation tab; the hash keeps links and back navigation working. */
export function goAutomation(tab: string, item = "") {
  location.hash = "automation/" + tab + (item ? "/" + encodeURIComponent(item) : "");
}

export const skipReasons: Record<string, string> = {
  overlap: "The previous run was still going, so this occurrence was skipped.",
  backlog: "Too many occurrences were waiting, so older ones were dropped.",
  downtime: "Meshra was not running when this was due.",
  auto_start_disabled: "The agent was stopped and this routine is not allowed to start it.",
  skipped_dependency: "An earlier step in the flow did not succeed.",
  binding_superseded: "The profile was switched to another provider after this was scheduled.",
};

export const runStateLabels: Record<string, string> = {
  claimed: "Starting",
  running: "Running",
  completed: "Completed",
  failed: "Failed",
  cancelled: "Cancelled",
  skipped: "Skipped",
};

export function runTone(state: string): "ok" | "bad" | "warn" | "idle" {
  if (state === "completed") return "ok";
  if (state === "failed") return "bad";
  if (state === "running" || state === "claimed") return "warn";
  return "idle";
}

export function relativeTime(iso?: string, now = Date.now()) {
  if (!iso) return "";
  const diff = now - new Date(iso).getTime();
  const abs = Math.abs(diff);
  const unit =
    abs < 60000
      ? "moments"
      : abs < 3600000
        ? Math.round(abs / 60000) + " min"
        : abs < 86400000
          ? Math.round(abs / 3600000) + " h"
          : Math.round(abs / 86400000) + " d";
  return unit === "moments" ? (diff >= 0 ? "just now" : "in moments") : diff >= 0 ? unit + " ago" : "in " + unit;
}
