import { useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Check,
  CircleAlert,
  Clock,
  MessageSquare,
  ShieldCheck,
  X,
} from "lucide-react";
import { automation, type AttentionItem } from "./automation-api";
import { Notice } from "./App";
import { Button } from "./components/ui/button";
import { ApprovalRules } from "./ApprovalRules";

const ATTENTION_STATUS_LABELS: Record<string, string> = {
  pending: "Waiting for you",
  approved: "Approved",
  denied: "Denied",
  answered: "Answered",
  expired: "Expired",
  invalidated: "No longer valid",
};

function argumentsText(item: AttentionItem) {
  const value = item.action?.arguments;
  if (value === undefined || value === null) return "";
  if (typeof value === "string") {
    try {
      return JSON.stringify(JSON.parse(value), null, 2);
    } catch {
      return value;
    }
  }
  return JSON.stringify(value, null, 2);
}

function exactRule(item: AttentionItem) {
  const args = item.action?.arguments;
  if (!args || typeof args !== "object" || Array.isArray(args)) return null;
  const value = args as Record<string, unknown>;
  if (item.name === "meshra_workspace_command" && !value.shell && Array.isArray(value.inputArgv) &&
      value.inputArgv.length > 0 && value.inputArgv.every((arg) => typeof arg === "string" && arg.length > 0 && !arg.includes("[REDACTED]"))) {
    return { tool: item.name, matcherKind: "argv_exact" as const, matcher: JSON.stringify(value.inputArgv) };
  }
  if (item.name === "meshra_workspace_write" && typeof value.path === "string" &&
      value.path && !["?", "*", "[", "]", "\\"].some((char) => (value.path as string).includes(char)) && !value.path.includes("[REDACTED]")) {
    return { tool: item.name, matcherKind: "path_glob" as const, matcher: value.path };
  }
  return null;
}

/**
 * Attention records are the console's human authority over exact tool
 * actions. Nothing else in Meshra can approve, deny or answer them.
 */
export function Approvals({ profileId }: { profileId?: string } = {}) {
  const client = useQueryClient();
  const [answers, setAnswers] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [results, setResults] = useState<Record<string, string>>({});
  const keys = useRef(new Map<string, string>());
  const list = useQuery({
    queryKey: ["approvals", profileId],
    queryFn: () => automation("approvals.list", { profileId }),
    refetchInterval: 15000,
    refetchIntervalInBackground: false,
  });
  const items = list.data?.items || [];
  const waiting = items.filter((i) => i.status === "pending");

  function keyFor(id: string, decision: string) {
    const mapKey = `${id}:${decision}`;
    const existing = keys.current.get(mapKey);
    if (existing) return existing;
    const random =
      typeof crypto !== "undefined" && "randomUUID" in crypto
        ? crypto.randomUUID()
        : Math.random().toString(36).slice(2) + Date.now().toString(36);
    const created = `${id}:${decision}:${random}`;
    keys.current.set(mapKey, created);
    return created;
  }

  async function decide(item: AttentionItem, decision: "approve" | "deny") {
    setBusy(item.id);
    setError("");
    try {
      const result = await automation("approvals.decide", {
        id: item.id,
        expectedRevision: item.revision,
        decision,
        actionHash: item.actionHash,
        idempotencyKey: keyFor(item.id, decision),
      });
      setResults((v) => ({ ...v, [item.id]: result.status }));
      await client.invalidateQueries({ queryKey: ["approvals"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  async function alwaysAllow(item: AttentionItem) {
    const rule = exactRule(item);
    if (!rule || !window.confirm(`Approve this action, then create a rule for ${rule.matcher}? ${rule.matcherKind === "path_glob" ? "Future writes may replace any content at this path. " : ""}The rule applies to future matching actions when the allowlist policy is enabled. Creating it cancels other outstanding actions for this profile.`)) return;
    setBusy(item.id);
    setError("");
    let approved = false;
    try {
      const result = await automation("approvals.decide", {
        id: item.id, expectedRevision: item.revision, decision: "approve",
        actionHash: item.actionHash, idempotencyKey: keyFor(item.id, "approve"),
      });
      approved = true;
      setResults((v) => ({ ...v, [item.id]: result.status }));
      // Policy changes invalidate admitted invocations; wait until this one has
      // finished before creating the rule, rather than cancelling the approval.
      let status = result.status;
      const deadline = Date.now() + 120000;
      while (status === "approved" || status === "claimed" || status === "pending") {
        if (Date.now() >= deadline) throw new Error("The action is still running. No rule was created; add it later in Approval rules.");
        await new Promise((resolve) => setTimeout(resolve, 1000));
        const current = await automation("approvals.list", { taskId: item.taskId });
        const found = current.items.find((entry) => entry.id === item.id);
        if (!found) throw new Error("Could not find the approved action. No rule was created; check its status and add the rule later.");
        status = found.status;
      }
      if (status !== "completed" && status !== "failed") throw new Error(`The action ended as ${status}. No rule was created.`);
      await automation("approval_rules.create", { profileId: item.profileId, ...rule });
      await client.invalidateQueries({ queryKey: ["approval-rules", item.profileId] });
      setResults((v) => ({ ...v, [item.id]: `${status} · allow rule created` }));
    } catch (e) {
      setError(`${approved ? "Action approved. " : ""}${(e as Error).message}`);
    } finally {
      await client.invalidateQueries({ queryKey: ["approvals"] });
      setBusy("");
    }
  }

  async function answer(item: AttentionItem) {
    const text = (answers[item.id] || "").trim();
    if (!text) return;
    setBusy(item.id);
    setError("");
    try {
      const result = await automation("questions.answer", {
        id: item.id,
        expectedRevision: item.revision,
        answer: text,
      });
      setResults((v) => ({ ...v, [item.id]: result.status }));
      await client.invalidateQueries({ queryKey: ["approvals"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  return (
    <div className="automation-attention">
      <ApprovalRules profileId={profileId} />
      <div className="automation-section-heading">
        <h2>Needs your attention</h2>
        <span className="muted">
          {waiting.length
            ? `${waiting.length} waiting`
            : "Nothing is waiting for you"}
        </span>
      </div>
      {error && <Notice>{error}</Notice>}
      {list.isError && <Notice>{list.error.message}</Notice>}
      {list.isPending ? (
        <p role="status">Loading attention records…</p>
      ) : !items.length ? (
        <p className="muted">
          <ShieldCheck size={14} />
          No tool call is waiting for a human. Approvals and questions are
          decided only here.
        </p>
      ) : (
        <ul className="automation-attention-list">
          {items.map((item) => {
            const status = results[item.id] || item.status;
            const pending = item.status === "pending" && !results[item.id];
            const detail = argumentsText(item);
            return (
              <li key={item.id} className="automation-attention-item">
                <div className="automation-attention-top">
                  <span className="automation-attention-name">
                    {item.kind === "question" ? (
                      <MessageSquare size={15} />
                    ) : (
                      <CircleAlert size={15} />
                    )}
                    <strong>{item.action?.label || item.name}</strong>
                    <span className="muted">{item.name}</span>
                  </span>
                  <span className="automation-attention-status">
                    {ATTENTION_STATUS_LABELS[status] || status}
                  </span>
                </div>
                <div className="automation-attention-meta">
                  <span>
                    <Clock size={13} />
                    Expires {new Date(item.expiresAt).toLocaleString()}
                  </span>
                  <span>Profile {item.profileId}</span>
                  <span>Task {item.taskId}</span>
                  <span className="mono">{item.actionHash.slice(0, 12)}</span>
                </div>
                {detail && (
                  <pre className="automation-attention-detail mono">
                    {detail}
                  </pre>
                )}
                {item.action?.diff && (
                  <pre className="automation-attention-detail mono">
                    {item.action.diff}
                  </pre>
                )}
                {item.action?.truncated && (
                  <p className="picker-hint">
                    This preview is truncated. Review the full change before
                    deciding.
                  </p>
                )}
                {pending && item.kind === "question" && (
                  <div className="automation-answer">
                    <label htmlFor={"answer-" + item.id}>Your answer</label>
                    <textarea
                      id={"answer-" + item.id}
                      rows={2}
                      maxLength={4096}
                      value={answers[item.id] || ""}
                      onChange={(e) =>
                        setAnswers((v) => ({ ...v, [item.id]: e.target.value }))
                      }
                      placeholder="Answer the agent's question."
                    />
                    <Button
                      onClick={() => void answer(item)}
                      disabled={busy === item.id || !(answers[item.id] || "").trim()}
                    >
                      <Check size={15} />
                      Send answer
                    </Button>
                  </div>
                )}
                {pending && item.kind !== "question" && (
                  <div className="automation-actions">
                    <Button
                      variant="positive"
                      onClick={() => void decide(item, "approve")}
                      disabled={busy === item.id}
                    >
                      <Check size={15} />
                      Approve this action
                    </Button>
                    {exactRule(item) && <Button
                      variant="outline"
                      onClick={() => void alwaysAllow(item)}
                      disabled={busy === item.id}
                    >Always allow this</Button>}
                    <Button
                      variant="error"
                      onClick={() => void decide(item, "deny")}
                      disabled={busy === item.id}
                    >
                      <X size={15} />
                      Deny
                    </Button>
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
