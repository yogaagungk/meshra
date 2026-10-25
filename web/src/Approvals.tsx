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
            const pending = item.status === "pending";
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
                      onClick={() => void decide(item, "approve")}
                      disabled={busy === item.id}
                    >
                      <Check size={15} />
                      Approve this action
                    </Button>
                    <Button
                      variant="outline"
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
