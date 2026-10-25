import { useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { automation } from "./automation-api";

/** Metadata only. Coalesce bursts without retaining task or secret payloads. */
export function AutomationEvents() {
  const client = useQueryClient();
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    let disposed = false;
    const roots = new Set<string>();
    const refresh = (root?: string) => {
      if (root) roots.add(root);
      else roots.add("*");
      if (timer) return;
      timer = setTimeout(() => {
        timer = undefined;
        const all = roots.has("*");
        const changed = new Set(roots);
        roots.clear();
        void client.invalidateQueries({ predicate: ({ queryKey }) => {
          const key = String(queryKey[0]);
          if (key === "automation" || key === "inbox" || key === "agents") return true;
          return all || [...changed].some((root) => key.startsWith(root));
        } }, { cancelRefetch: false });
      }, 300);
    };
    const source = new EventSource("/console/api/automation/events?after=0");
    source.onmessage = (event) => {
      try {
        const value = JSON.parse(event.data) as { type?: string; profileId?: string; data?: unknown };
        const root = value.type?.split(".")[0];
        if (!root) return;
        refresh(root === "connections" ? "connections" : root);
        if (value.profileId && /(?:completed|failed|attention|approval|question)/.test(value.type || "") && "Notification" in window && Notification.permission === "granted") {
          void automation("profiles.get", { id: value.profileId }).then((profile) => {
            if (!disposed && profile.notificationsEnabled)
              new Notification("Meshra · " + profile.name, { body: "Work needs your attention. Open the console for details.", tag: profile.id });
          }).catch(() => {});
        }
      } catch { refresh(); }
    };
    source.addEventListener("resync", () => refresh());
    source.addEventListener("expired", () => {
      source.close();
      window.dispatchEvent(new Event("meshra:expired"));
    });
    return () => {
      disposed = true;
      source.close();
      if (timer) clearTimeout(timer);
      roots.clear();
    };
  }, [client]);
  return null;
}
