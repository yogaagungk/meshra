export type Agent = {
  managedChat?: boolean;
  id: string;
  harness: string;
  name: string;
  cwd: string;
  state: string;
  sessionId?: string;
  error?: string;
  capabilities: Record<string, boolean>;
  policy: {
    destinations: string[] | null;
    operations: string[] | null;
    senders?: string[];
    inboundOperations?: string[];
    delegate: boolean;
  };
};
export type Ref = { contextId: string; version: number };
export type Conversation = {
  chatSettings?: ChatSettings;
  mode?: "chat";
  conversationId: string;
  title: string;
  owner: string;
  createdAt: string;
  participants: string[];
  contextRefs: Ref[];
};
export type InboxItem = {
  conversation: Conversation;
  preview: string;
  updatedAt: string;
  state: string;
};
export type Task = {
  seq: number;
  taskId: string;
  messageId: string;
  conversationId?: string;
  from: string;
  to: string;
  text: string;
  result?: string;
  state: string;
  error?: string;
  errorCategory?: string;
  ambiguous?: boolean;
  correlationId: string;
  parentTaskId?: string;
  originAgentId: string;
  originSessionId?: string;
  destinationSessionId?: string;
  createdAt: string;
  updatedAt: string;
  deadline: string;
  attempts: number;
  deliveryAttempts: number;
  contextRefs?: Ref[];
  resultRef?: Ref;
  dependsOn?: string[];
};
export type Page = { items: Task[]; nextBefore: number; hasMore: boolean };
export type SharedContext = {
  contextId: string;
  version: number;
  owner: string;
  title: string;
  type: string;
  scope: string;
  sha256: string;
  size: number;
  content?: string;
};
export type TraceEvent = {
  id: number;
  type: string;
  data: string;
  time: string;
  taskId: string;
};
export const terminal = (state: string) =>
  ["completed", "failed", "cancelled", "timed_out"].includes(state);
export function mergeTasks(pages: Page[]) {
  const map = new Map<string, Task>();
  for (const page of [...pages].reverse())
    for (const task of page.items) map.set(task.taskId, task);
  return [...map.values()].sort((a, b) => a.seq - b.seq);
}
export function stateLabel(task: Task) {
  if (task.ambiguous && task.state !== "completed") return "Outcome uncertain";
  return (
    (
      {
        dispatching: "Accepted",
        running: "Running",
        completed: "Completed",
        queued: "Queued",
        failed: "Failed",
        cancelled: "Cancelled",
        timed_out: "Timed out",
      } as Record<string, string>
    )[task.state] ?? task.state
  );
}

export type PersonalPreferences = {
  displayName: string;
  maxParallelTurns: number;
  maxStartedAgents: number;
  idleMinutes: number;
  dailyTurnLimit: number;
};
export type Workspace = {
  id: string;
  name: string;
  path: string;
  createdAt: string;
};
export type ModelOption = { id: string; name: string; provider?: string };
export type ChatSettings = {
  workspaceId: string;
  config: { model?: string; provider?: string };
};
