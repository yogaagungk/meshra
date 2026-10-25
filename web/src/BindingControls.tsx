import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { automation } from "./automation-api";
import { Button } from "./components/ui/button";
import { Notice } from "./App";

export function BindingControls({ profileId, conversationId, agentId }: { profileId: string; conversationId: string; agentId?: string }) {
  const client = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function run(action: "start" | "stop" | "restart") {
    if (busy) return;
    if (action !== "start" && !window.confirm("Stop this profile binding" + (action === "restart" ? " and start a fresh runtime" : "") + "? Running work is cancelled and may already have external effects.")) return;
    setBusy(true); setError("");
    try {
      await automation("profiles.bindingAction", { profileId, conversationId, action });
      await Promise.all([
        client.invalidateQueries({ queryKey: ["agents"] }),
        client.invalidateQueries({ queryKey: ["queue"] }),
        client.invalidateQueries({ queryKey: ["automation", "binding", profileId, conversationId] }),
      ]);
    } catch (failure) { setError((failure as Error).message); }
    finally { setBusy(false); }
  }
  return <div className="form-stack" aria-label={"Profile binding " + (agentId || profileId)}>
    <div className="automation-actions">{(["start", "stop", "restart"] as const).map((action) => <Button key={action} type="button" variant="outline" disabled={busy} onClick={() => void run(action)}>{action === "start" ? "Start binding" : action === "stop" ? "Stop binding" : "Restart binding"}</Button>)}</div>
    {error && <Notice>{error}</Notice>}
  </div>;
}
