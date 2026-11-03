import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  AlertTriangle,
  Check,
  ChevronRight,
  FileText,
  Globe,
  Loader2,
  Search,
  Terminal,
  Wrench,
} from "lucide-react";
import { api } from "./api";
import { terminal, type Task, type TraceEvent } from "./types";

type ToolItem = {
  id: string;
  name: string;
  input?: unknown;
  output?: string;
  isError?: boolean;
  done: boolean;
};

type TodoItem = { id: string; text: string; status: "pending" | "in_progress" | "done" };

/** The latest todo event is a complete snapshot of the turn's plan. */
function todoItems(events: TraceEvent[]): TodoItem[] | undefined {
  const event = events.filter((e) => e.type === "todo").at(-1);
  if (!event) return undefined;
  try {
    const data: unknown = JSON.parse(event.data);
    if (!Array.isArray(data)) return undefined;
    return data.filter((item): item is TodoItem =>
      item !== null && typeof item === "object" &&
      typeof item.id === "string" && typeof item.text === "string" &&
      ["pending", "in_progress", "done"].includes(item.status));
  } catch {
    return undefined;
  }
}

/** Pair tool_use and tool_result events by call id, in the order they began. */
export function toolItems(events: TraceEvent[]): ToolItem[] {
  const items = new Map<string, ToolItem>();
  for (const e of events) {
    if (e.type !== "tool_use" && e.type !== "tool_result") continue;
    let data: Record<string, unknown>;
    try {
      data = JSON.parse(e.data);
    } catch {
      data = { id: "event-" + e.id, name: "tool", output: e.data };
    }
    const id = String(data.id ?? "event-" + e.id);
    const item = items.get(id) ?? { id, name: "tool", done: false };
    if (e.type === "tool_use") {
      item.name = String(data.name ?? item.name);
      item.input = data.input;
    } else {
      item.output = typeof data.output === "string" ? data.output : "";
      item.isError = data.isError === true;
      item.done = true;
    }
    items.set(id, item);
  }
  return [...items.values()];
}

function iconFor(name: string) {
  const n = name.toLowerCase();
  if (/bash|shell|exec|command|run/.test(n)) return Terminal;
  if (/read|write|edit|file|patch/.test(n)) return FileText;
  if (/grep|glob|search|find/.test(n)) return Search;
  if (/web|fetch|http|browser/.test(n)) return Globe;
  return Wrench;
}

/** The most telling short argument: a command, path, pattern or URL. */
function brief(input: unknown): string {
  if (input == null) return "";
  if (typeof input === "string") return input.slice(0, 120);
  if (typeof input !== "object") return String(input);
  const o = input as Record<string, unknown>;
  for (const key of ["command", "file_path", "path", "pattern", "query", "url", "text"]) {
    if (typeof o[key] === "string") return (o[key] as string).slice(0, 120);
  }
  const first = Object.values(o).find((v) => typeof v === "string");
  return typeof first === "string" ? first.slice(0, 120) : "";
}

function pretty(input: unknown): string {
  if (typeof input === "string") return input;
  try {
    return JSON.stringify(input, null, 2);
  } catch {
    return String(input);
  }
}

/**
 * Animated tool activity for one response. While the turn runs the summary
 * shows the tool in flight; the list expands to each call's input and output.
 */
export function ToolCalls({ task }: { task: Task }) {
  const running = !terminal(task.state);
  const [open, setOpen] = useState(false);
  const wanted = running || (task.toolCalls ?? 0) > 0;
  const events = useQuery({
    queryKey: ["task-tools", task.taskId, task.toolCalls ?? 0, running],
    queryFn: async () => {
      const all: TraceEvent[] = [];
      let after = 0;
      for (;;) {
        const page = await api<TraceEvent[]>("tasks/" + encodeURIComponent(task.taskId) + "/events?after=" + after);
        all.push(...page);
        if (page.length < 256) return all;
        after = page[page.length - 1].id;
      }
    },
    enabled: wanted,
    refetchInterval: running ? 1200 : false,
    staleTime: running ? 0 : Infinity,
    gcTime: 5 * 60_000,
  });
  const items = toolItems(events.data ?? []);
  const todos = todoItems(events.data ?? []);
  if (!items.length && !todos) return null;
  const active = running ? items.filter((i) => !i.done).at(-1) : undefined;
  const failed = items.filter((i) => i.isError).length;
  return (
    <div className={"tool-calls" + (active ? " is-running" : "")} data-open={open}>
      <button
        type="button"
        className="tool-calls-summary"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <ChevronRight size={14} className="tool-calls-chevron" aria-hidden />
        {active ? (
          <Loader2 size={14} className="tool-spin" aria-hidden />
        ) : failed ? (
          <AlertTriangle size={14} aria-hidden />
        ) : (
          <Check size={14} aria-hidden />
        )}
        <span className="tool-calls-label">
          {active ? (
            <>
              Running <code>{active.name}</code>
              {brief(active.input) && <span className="tool-calls-brief"> · {brief(active.input)}</span>}
            </>
          ) : (
            <>
              {items.length ? `Used ${items.length} tool${items.length === 1 ? "" : "s"}` : "Turn plan"}
              {failed ? ` · ${failed} failed` : ""}
            </>
          )}
        </span>
      </button>
      {open && (
        <>
        {todos && (
          <div className="tool-call-body">
            <h4>Turn plan</h4>
            {todos.length ? (
              <ul>
                {todos.map((todo) => (
                  <li key={todo.id}>
                    {todo.text} <span className="muted"> · {todo.status === "in_progress" ? "In progress" : todo.status === "done" ? "Done" : "Pending"}</span>
                  </li>
                ))}
              </ul>
            ) : <p className="muted">No plan items.</p>}
          </div>
        )}
        <ol className="tool-calls-list">
          {items.map((item) => {
            const Icon = iconFor(item.name);
            const live = running && !item.done;
            return (
              <li key={item.id} className={"tool-call" + (live ? " is-live" : item.isError ? " is-error" : "")}>
                <details>
                  <summary>
                    <Icon size={14} aria-hidden />
                    <code>{item.name}</code>
                    <span className="tool-calls-brief">{brief(item.input)}</span>
                    {live ? (
                      <Loader2 size={13} className="tool-spin" aria-label="Running" />
                    ) : item.isError ? (
                      <AlertTriangle size={13} aria-label="Failed" />
                    ) : (
                      <Check size={13} aria-label="Completed" />
                    )}
                  </summary>
                  <div className="tool-call-body">
                    {item.input !== undefined && (
                      <>
                        <h4>Input</h4>
                        <pre>{pretty(item.input)}</pre>
                      </>
                    )}
                    {item.output !== undefined ? (
                      <>
                        <h4>{item.isError ? "Error" : "Output"}</h4>
                        <pre>{item.output || "(no output)"}</pre>
                      </>
                    ) : (
                      live && <p className="muted">Waiting for the result…</p>
                    )}
                  </div>
                </details>
              </li>
            );
          })}
        </ol>
        </>
      )}
    </div>
  );
}
