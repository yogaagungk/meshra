import { api, APIError, authenticatedFetch } from "./api";
import type { Agent, Conversation, Task } from "./types";

export type ProfileHarness = "codex" | "claude" | "omp" | "opencode" | "openai" | "acp";
export type ProfileConfig = { model?: string; provider?: string };
export type ProfileProvider = {
  harness: ProfileHarness;
  config: ProfileConfig;
  connectionId?: string;
};
export type ProfileCapabilities = {
  workspaceRead: boolean;
  workspaceWrite: boolean;
  command: boolean;
  browser: boolean;
  computer: boolean;
  memoryWrite: boolean;
  externalMcp: boolean;
  attachments: boolean;
};
export type ProfileFields = {
  name: string;
  instructions: string;
  avatarAttachmentId?: string;
  workspaceId: string;
  binding: ProfileProvider;
  capabilities: ProfileCapabilities;
  allowedProfileIds: string[];
  notificationsEnabled: boolean;
};
export type Profile = ProfileFields & {
  id: string;
  revision: number;
  archived: boolean;
};
export type ProfileBinding = ProfileProvider & {
  id: string;
  profileId: string;
  conversationId: string;
  generation: number;
  agentId: string;
  toolCatalogDigest: string;
};
export type ProfileConversation = Conversation & { profileIds: string[] };
export type Pagination = { after?: string; limit?: number };
export type ResourcePage<T> = { items: T[]; nextAfter?: string; hasMore: boolean };
export type RevisionRequest = { id: string; expectedRevision: number };
export type QueueState = {
  agentId: string;
  profileId?: string;
  paused: boolean;
  revision: number;
  tasks: Task[];
};
export type Memory = {
  id: string;
  profileId: string;
  conversationId?: string;
  scope: "operator" | "conversation";
  revision: number;
  content: string;
  sourceTaskId?: string;
  sourceAvailable?: boolean;
  pinned: boolean;
  updatedAt: string;
};
export type HistoryHit = { taskId: string; excerpt: string };
export type HistoryExchange = {
  taskId: string;
  seq: number;
  text: string;
  result?: string;
};
export type ProfileDraft = { name: string; instructions: string };
export type ProfileSwitchPreview = {
  profileId: string;
  revision: number;
  from: ProfileProvider;
  to: ProfileProvider;
  instructions: string;
  handoffByteLimit: number;
  conversations: {
    conversationId: string;
    title: string;
    bindingId: string;
    handoff: { taskId: string; from: string; to: string; message: string; reply: string }[];
    contexts: { contextId: string; version: number; title: string; sha256: string; size: number }[];
  }[];
};
export type RunUsage = {
  taskId: string; attempt: number; startedAt: string; harness: string;
  configuredModel: string; configuredProvider: string;
  profileId: string | null; profileRevision: number | null; bindingId: string | null;
  reportedModel: string | null; reportedProvider: string | null;
  inputTokens: number | null; outputTokens: number | null; cachedInputTokens: number | null;
  reportedCost: number | null; currency: string | null;
  inputBytes: number; outputBytes: number; durationMs: number;
  state: string; source: "native" | "unreported"; partial: boolean; stale: boolean;
};

/** Attention records are console-only: approvals and questions are decided here. */
export type AttentionAction = {
  label: string;
  arguments?: unknown;
  manifestHash?: string;
  diff?: string;
  truncated?: boolean;
};
export type AttentionItem = {
  type: "attention";
  id: string;
  invocationId: string;
  taskId: string;
  profileId: string;
  name: string;
  kind: "approval" | "question";
  revision: number;
  status: string;
  expiresAt: string;
  actionHash: string;
  action: AttentionAction;
};
export type ChangesetEntry = {
  kind: string;
  hash?: string;
  mode?: number;
  link?: string;
};
export type ChangesetChange = {
  path: string;
  before?: ChangesetEntry;
  after?: ChangesetEntry;
};
export type Changeset = {
  id: string;
  invocationId: string;
  workspaceId: string;
  manifestHash: string;
  revision: number;
  state: string;
  changes: ChangesetChange[];
  preview: string;
  previewTruncated: boolean;
  createdAt: string;
  updatedAt: string;
  error?: string;
};
export type ChangesetSummary = {
  id: string;
  invocationId: string;
  workspaceId: string;
  manifestHash: string;
  revision: number;
  state: string;
  createdAt: string;
  updatedAt: string;
  error?: string;
};
export type ChangesetJournalEntry = {
  path: string;
  state: string;
  originalHash: string;
  resultHash: string;
  error?: string;
  updatedAt: string;
};
export type ChangesetDetail = Changeset & { journal: ChangesetJournalEntry[] };
export type NetworkGrant = {
  id: string;
  revision: number;
  profileId?: string;
  connectionId?: string;
  origin: string;
  allowPrivate: boolean;
  enabled: boolean;
};
export type RuntimeLimits = {
  snapshotBytes: number;
  snapshotFiles: number;
  storeBytes: number;
  cgroupRoot?: string;
};
export type RuntimeAsset = { path: string; sha256?: string; system: boolean };
export type RuntimeConfig = {
  executables: Record<string, string>;
  runtimePaths: string[];
  limits: RuntimeLimits;
  manifest: RuntimeAsset[];
};
export type RuntimeReadiness = {
  revision: number;
  config: RuntimeConfig;
  status: { state: string; prerequisites: string[] };
};

/** Operator reviewed provider endpoints. Secrets are referenced, never returned. */
export type ProviderEndpoint = {
  id: string;
  revision: number;
  name: string;
  baseUrl: string;
  enabled: boolean;
  secretRef?: string;
  allowInsecureHttp?: boolean;
  supportsTools: boolean;
  supportsImages: boolean;
  contextWindowTokens?: number;
  createdAt: string;
  updatedAt: string;
  secretStatus: "missing" | "saved" | "unreadable";
};
export type ACPAgent = {
  id: string;
  revision: number;
  name: string;
  preset: string;
  executable: string;
  args: string[];
  runtimePaths: string[];
  secretEnvRefs: string[];
  readOnlyAuthFiles: string[];
  allowedOrigins: string[];
  enabled: boolean;
  createdAt: string;
  updatedAt: string;
  secretStatus: "missing" | "saved" | "unreadable";
};
export type ACPInspection = {
  protocolVersion: number;
  supportsLoad: boolean;
  authMethods: { id: string; name: string; description?: string }[];
  ready: boolean;
};
export type ConnectionList = { endpoints: ProviderEndpoint[]; acp: ACPAgent[] };
export type LocalModelScan = {
  url: string;
  status: "available" | "unreachable" | "error";
  models?: string[];
  detail?: string;
};
export type VoiceReadiness = {
  revision: number;
  configured: boolean;
  state: string;
  prerequisites: string[];
  speakerId?: number;
  autoReadReplies?: boolean;
  stt?: { configured: boolean; model?: string };
  tts?: { configured: boolean; modelDirectory?: string };
};
export type MCPTool = {
  name: string;
  description?: string;
  inputSchema?: unknown;
  digest?: string;
};
export type MCPBinding = {
  profileId: string;
  serverId: string;
  revision: number;
  enabled: boolean;
  tools: { name: string; digest: string }[];
};
export type MCPServer = {
  id: string;
  revision: number;
  name: string;
  transport: string;
  url?: string;
  executable?: string;
  args?: string[];
  secretHeaderRefs?: string[];
  secretEnvRefs?: string[];
  secretStatus?: Record<string, string>;
  workspaceId: string;
  allowedOrigins?: string[];
  allowPrivate?: boolean;
  enabled: boolean;
  auth: string;
  authExpiresAt?: string;
  tools?: MCPTool[];
  bindings?: MCPBinding[];
};

export type ConnectorRoute = { id: string; revision: number; profileId: string; allow: { workspaces?: string[]; guilds?: string[]; channels?: string[]; chats?: string[]; users?: string[] }; autoStart: boolean; enabled: boolean };
export type Connector = { id: string; revision: number; kind: string; name: string; enabled: boolean; secrets: Record<string, string>; routes: ConnectorRoute[]; lastError?: string; cursor?: string; createdAt: string; updatedAt: string };
export type ConnectorDelivery = { id: string; taskId: string; chunk: number; state: string; remoteId?: string; error?: string; createdAt: string; updatedAt: string };
export type VoiceSettings = { sttExecutable?: string; sttModelPath?: string; ttsExecutable?: string; ttsModelDirectory?: string; ffmpegExecutable?: string; speakerId: number; autoReadReplies: boolean };
export type VoiceJob = { id: string; kind: string; state: string; expiresAt: string; transcript?: string; draft: boolean; autoSend: boolean; audioAvailable: boolean; error?: string };
export type BrowserSession = { id: string; profileId: string; conversationId: string; state: string; epoch: number; frameSeq: number; leaseOwner?: string; leaseExpiresAt?: string; snapshotText?: string; tabs: number; lastError?: string };
export type BrowserAction = { action: string; url?: string; selector?: string; text?: string; key?: string; direction?: string; amount?: number; tab?: number; attachmentId?: string };

/**
 * Routines, flows, skills and templates are the durable automation surface.
 * Their field names follow the reviewed definitions; every field that the
 * relay has not confirmed yet is optional so the console never invents data.
 */
export type RoutineTrigger = {
  kind: "once" | "interval" | "cron" | "webhook" | "manual";
  at?: string;
  everySeconds?: number;
  expression?: string;
  timeZone?: string;
};
export type RoutineDefinition = {
  id?: string;
  revision?: number;
  name?: string;
  profileId?: string;
  instruction?: string;
  trigger?: RoutineTrigger;
  flowId?: string;
  flowRevision?: number;
  enabled?: boolean;
  autoStart?: boolean;
  timeoutSeconds?: number;
  webhookSecret?: string;
};
export type Routine = RoutineDefinition & {
  nextDueAt?: string;
  webhookSecretStatus?: "missing" | "saved" | "unreadable";
};
export type RoutineStep = {
  nodeId: string;
  bindingId?: string;
  taskId?: string;
  state?: string;
  reason?: string;
};
export type RoutineRun = {
  runId: string;
  routineId: string;
  revision: number;
  scheduledAt: string;
  source: string;
  state: string;
  reason?: string;
  skippedCount?: number;
  conversationId?: string;
  taskIds?: string[];
  steps?: RoutineStep[];
  error?: string;
  createdAt: string;
  updatedAt: string;
};
export type FlowNode = { id: string; profileId: string; instruction: string };
export type FlowEdge = { from: string; to: string };
export type Flow = {
  id: string;
  revision: number;
  nodes: FlowNode[];
  edges: FlowEdge[];
};
export type Skill = {
  id?: string;
  revision?: number;
  name?: string;
  description?: string;
  version?: number;
  markdown?: string;
  assetAttachmentIds?: string[];
  assets?: { path: string; mime: string; data?: string; sha256: string }[];
  sha256?: string;
};
export type Template = {
  id?: string;
  revision?: number;
  schemaVersion?: number;
  name?: string;
  kind?: string;
  definition: unknown;
  enabled: boolean;
  skillId?: string;
};
export type RoutineToken = { id: string; routineIds: string[]; expiresAt?: string; revokedAt?: string; createdAt: string; token?: string };
export type TemplatePreview = { id: string; digest: string; expiresAt: string; template: Template; assets?: { path: string; mime: string; size: number; sha256: string }[] };
/** A list response whose envelope is not yet confirmed by the relay. */
export type UnverifiedList<T> = {
  items?: T[];
  nextAfter?: string;
  hasMore?: boolean;
};

type Operation<Request, Response> = { request: Request; response: Response };
export type AutomationOperations = {
  "profiles.binding": Operation<{ profileId: string; conversationId: string }, ProfileBinding>;
  "browser.sessions": Operation<{ profileId: string; conversationId: string }, { items: BrowserSession[] }>;
  "skills.bindings": Operation<{ profileId: string }, { items: { profileId: string; skillId: string; version: number; enabled: boolean; revision: number }[] }>;
  "profiles.list": Operation<Pagination, ResourcePage<Profile>>;
  "profiles.get": Operation<{ id: string }, Profile>;
  "profiles.save": Operation<ProfileFields & { id?: string; expectedRevision?: number }, Profile>;
  "profiles.archive": Operation<RevisionRequest & { archived: boolean }, Profile>;
  "profiles.switch": Operation<RevisionRequest & { binding: ProfileProvider; shareHistory: true }, Profile>;
  "profiles.switchPreview": Operation<RevisionRequest & { binding: ProfileProvider }, ProfileSwitchPreview>;
  "profiles.send": Operation<{
    id: string;
    conversationId?: string;
    text: string;
    attachmentIds?: string[];
    idempotencyKey: string;
    timeoutSeconds?: number;
  }, Task>;
  "profiles.createConversation": Operation<{
    title: string;
    profileIds: string[];
    kind: "chat" | "task" | "channel";
  }, ProfileConversation>;
  "profiles.conversations": Operation<Pagination, ResourcePage<ProfileConversation>>;
  "profiles.bindingAction": Operation<{
    profileId: string;
    conversationId: string;
    action: "start" | "stop" | "restart";
  }, Agent>;
  "profiles.generate": Operation<{
    description: string;
    harness: ProfileHarness;
    config?: ProfileConfig;
    workspaceId: string;
  }, { taskId: string; state: string; startError?: string }>;
  "profiles.generationDraft": Operation<{ taskId: string }, {
    taskId: string; state: string; draft?: ProfileDraft; raw?: string; error?: string;
  }>;
  "profiles.resetContext": Operation<RevisionRequest & { conversationId: string }, { epoch: number; resetAfterSeq: number }>;
  "queue.list": Operation<{ agentId?: string; profileId?: string }, { items: QueueState[] }>;
  "queue.pause": Operation<{ agentId: string; expectedRevision: number }, QueueState>;
  "queue.resume": Operation<{ agentId: string; expectedRevision: number }, QueueState>;
  "queue.reorder": Operation<{ agentId: string; expectedRevision: number; taskIds: string[] }, QueueState>;
  "history.search": Operation<Pagination & {
    profileId: string;
    conversationId: string;
    query: string;
  }, ResourcePage<HistoryHit>>;
  "history.read": Operation<{
    profileId: string;
    conversationId: string;
    taskId?: string;
    before?: number;
    offset?: number;
    limit?: number;
  }, { items: HistoryExchange[]; nextBefore?: number; hasMore: boolean }>;
  "memory.list": Operation<{ profileId: string; conversationId?: string }, { items: Memory[] }>;
  "memory.save": Operation<{
    profileId: string;
    id?: string;
    expectedRevision?: number;
    content: string;
    pinned: boolean;
    scope: "operator" | "conversation";
    conversationId?: string;
    sourceTaskId?: string;
  }, Memory>;
  "memory.remove": Operation<RevisionRequest & { profileId: string }, { ok: boolean }>;
  "attachments.list": Operation<Pagination & { conversationId?: string }, Attachment[]>;
  "attachments.remove": Operation<{ id: string; confirm: true }, Attachment>;
  "usage.list": Operation<{ profileId?: string; bindingId?: string; conversationId?: string; harness?: string; limit?: number }, {
    items: RunUsage[]; hasMore: boolean; retentionDays: number;
  }>;
  "approvals.list": Operation<
    { taskId?: string; profileId?: string },
    { items: AttentionItem[] }
  >;
  "approvals.decide": Operation<{
    id: string;
    expectedRevision: number;
    decision: "approve" | "deny";
    actionHash: string;
    idempotencyKey: string;
  }, AttentionItem>;
  "questions.answer": Operation<
    { id: string; expectedRevision: number; answer: string },
    AttentionItem
  >;
  "changesets.list": Operation<
    { taskId?: string; after?: string; limit?: number },
    { changesets: ChangesetSummary[]; hasMore: boolean; nextAfter?: string }
  >;
  "changesets.get": Operation<{ id: string }, ChangesetDetail>;
  "changesets.apply": Operation<
    { id: string; manifestHash: string; expectedRevision: number },
    Changeset
  >;
  "network.list": Operation<
    { profileId?: string; connectionId?: string },
    NetworkGrant[]
  >;
  "network.grant": Operation<{
    profileId?: string;
    connectionId?: string;
    origin: string;
    allowPrivate: boolean;
    expectedRevision?: number;
  }, NetworkGrant>;
  "network.revoke": Operation<
    { id: string; expectedRevision: number },
    NetworkGrant
  >;
  "runtime.status": Operation<Record<string, never>, RuntimeReadiness>;
  "runtime.configure": Operation<{
    expectedRevision: number;
    executables: Record<string, string>;
    runtimePaths: string[];
    limits: RuntimeLimits;
  }, { revision: number; config: RuntimeConfig }>;
  "connections.list": Operation<Record<string, never>, ConnectionList>;
  "connections.saveEndpoint": Operation<{
    expectedRevision?: number;
    endpoint: {
      id?: string;
      revision?: number;
      name: string;
      baseUrl: string;
      enabled?: boolean;
      secretRef?: string;
      allowInsecureHttp?: boolean;
      supportsTools?: boolean;
      supportsImages?: boolean;
      contextWindowTokens?: number;
    };
    apiKey?: string;
  }, ProviderEndpoint>;
  "connections.disable": Operation<
    { id: string; expectedRevision: number },
    ProviderEndpoint
  >;
  "connections.remove": Operation<
    { id: string; expectedRevision: number },
    { id: string; removed: boolean }
  >;
  "connections.scanLocal": Operation<
    Record<string, never>,
    { results: LocalModelScan[]; acpPresets: { preset: string; executable?: string; args: string[]; installed: boolean }[] }
  >;
  "connections.checkAcp": Operation<
    { id: string; expectedRevision?: number },
    { id: string; revision: number; status: "ready" | "failed"; inspection: ACPInspection; error?: string }
  >;
  "connections.acpStatus": Operation<
    { id: string; expectedRevision?: number },
    { configuration: ACPAgent; runtime: RuntimeReadiness["status"]; verified: false }
  >;
  "connections.authenticateAcp": Operation<{
    id: string;
    expectedRevision: number;
    profileId: string;
    conversationId: string;
    methodId: string;
  }, { id: string; revision: number; bindingId: string; status: "ready" | "failed"; inspection: ACPInspection; error?: string }>;
  "connections.saveAcp": Operation<{ expectedRevision?: number; acp: { id?: string; name: string; preset: string; executable: string; args?: string[]; runtimePaths?: string[]; secretEnvRefs?: string[]; readOnlyAuthFiles?: string[]; allowedOrigins?: string[]; enabled?: boolean } }, ACPAgent>;
  "connections.disableAcp": Operation<RevisionRequest, ACPAgent>;
  "connections.removeAcp": Operation<RevisionRequest, { id: string; removed: boolean }>;
  "mcp.list": Operation<Pagination, ResourcePage<MCPServer>>;
  "mcp.save": Operation<{ expectedRevision?: number; server: { id?: string; name: string; transport: "http" | "stdio"; url?: string; executable?: string; args?: string[]; secretHeaderRefs?: string[]; secretEnvRefs?: string[]; workspaceId: string; allowedOrigins?: string[]; allowPrivate?: boolean; enabled: false } }, MCPServer>;
  "mcp.test": Operation<{ id: string }, MCPServer>;
  "mcp.setEnabled": Operation<RevisionRequest & { enabled: boolean }, MCPServer>;
  "mcp.bind": Operation<{ profileId: string; serverId: string; expectedRevision: number; toolNames: string[]; enabled: boolean }, MCPBinding>;
  "mcp.remove": Operation<RevisionRequest, { id: string; removed: boolean }>;
  "mcp.authStart": Operation<{ id: string; redirectUri?: string }, { id: string; serverId: string; authorizationUrl: string; expiresAt: string }>;
  "mcp.authStatus": Operation<{ id: string }, { serverId: string; auth: string; expiresAt: string }>;
  "mcp.signOut": Operation<{ id: string }, { serverId: string; auth: string; upstreamRevoked: boolean }>;
  "connectors.list": Operation<Record<string, never>, Connector[]>;
  "connectors.save": Operation<{ expectedRevision?: number; connector: { id?: string; kind: string; name: string }; secrets?: Record<string, string> }, Connector>;
  "connectors.test": Operation<{ id: string }, { ok: boolean; kind: string }>;
  "connectors.setEnabled": Operation<RevisionRequest & { enabled: boolean }, Connector>;
  "connectors.routesSave": Operation<RevisionRequest & { routes: Omit<ConnectorRoute, "revision">[] }, Connector>;
  "connectors.remove": Operation<RevisionRequest, { id: string; removed: boolean }>;
  "connectors.deliveries": Operation<{ id: string; after?: string; limit?: number }, ConnectorDelivery[]>;
  "connectors.retryDelivery": Operation<{ deliveryId: string; confirmDuplicateRisk: true }, { id: string; state: string }>;
  "voice.configure": Operation<VoiceSettings & { expectedRevision: number }, VoiceSettings & { revision: number }>;
  "voice.transcribe": Operation<{ audioAttachmentId: string }, VoiceJob>;
  "voice.synthesize": Operation<{ text: string; speakerId: number }, VoiceJob>;
  "voice.job": Operation<{ id: string }, VoiceJob>;
  "voice.cancel": Operation<{ id: string }, VoiceJob>;
  "browser.open": Operation<{ profileId: string; conversationId: string; taskId?: string }, BrowserSession>;
  "browser.state": Operation<{ id: string }, BrowserSession>;
  "browser.takeover": Operation<{ id: string; expectedEpoch: number }, BrowserSession>;
  "browser.release": Operation<{ id: string; expectedEpoch: number }, BrowserSession>;
  "browser.input": Operation<{ id: string; epoch: number; frameSeq: number; action: BrowserAction }, { id: string; epoch: number; frameSeq: number; state: string; snapshot: { text: string }; url?: string; title?: string }>;
  "browser.close": Operation<{ id: string }, BrowserSession>;
  "browser.clearData": Operation<{ profileId: string; conversationId: string; confirm: true }, { cleared: boolean }>;
  "voice.status": Operation<Record<string, never>, VoiceReadiness>;
  "routines.list": Operation<Pagination, UnverifiedList<Routine>>;
  "routines.save": Operation<
    { routine: RoutineDefinition; expectedRevision?: number },
    Routine
  >;
  "routines.setEnabled": Operation<
    { id: string; enabled: boolean; expectedRevision: number },
    Routine
  >;
  "routines.run": Operation<
    { id: string; idempotencyKey: string },
    RoutineRun
  >;
  "routines.runs": Operation<
    { id: string; after?: string; limit?: number },
    UnverifiedList<RoutineRun>
  >;
  "routines.preview": Operation<{ trigger: RoutineTrigger; count?: number }, { trigger: RoutineTrigger; timeZone: string; occurrences: string[] }>;
  "routines.cancelRun": Operation<{ runId: string }, RoutineRun>;
  "routineTokens.list": Operation<Pagination, ResourcePage<RoutineToken>>;
  "routineTokens.create": Operation<{ routineIds: string[]; expiresAt?: string }, RoutineToken>;
  "routineTokens.revoke": Operation<{ id: string }, RoutineToken>;
  "flows.get": Operation<{ id: string; revision?: number }, Flow>;
  "flows.save": Operation<{
    id?: string;
    expectedRevision?: number;
    nodes: FlowNode[];
    edges: FlowEdge[];
  }, Flow>;
  "skills.list": Operation<Pagination, ResourcePage<Skill>>;
  "skills.read": Operation<{ id: string; version?: number }, Skill>;
  "skills.save": Operation<{
    id?: string;
    expectedRevision?: number;
    name: string;
    description: string;
    markdown: string;
    assetAttachmentIds?: string[];
  }, Skill>;
  "templates.list": Operation<Pagination, ResourcePage<Template>>;
  "skills.bind": Operation<{ profileId: string; skillId: string; version: number; enabled: boolean; expectedRevision: number }, { profileId: string; skillId: string; version: number; enabled: boolean; revision: number }>;
  "skills.remove": Operation<RevisionRequest, { removed: boolean }>;
  "templates.export": Operation<{ kind: string; id: string; version?: number }, { attachment: Attachment; digest: string }>;
  "templates.importPreview": Operation<{ attachmentId: string; name?: string; description?: string }, TemplatePreview>;
  "templates.import": Operation<{ previewId: string; expectedDigest: string }, { template: Template; skill?: Skill }>;
};
export type AutomationOperation = keyof AutomationOperations;
export type AutomationRequest<K extends AutomationOperation> = AutomationOperations[K]["request"];
export type AutomationResponse<K extends AutomationOperation> = AutomationOperations[K]["response"];

export function automation<K extends AutomationOperation>(
  operation: K,
  request: AutomationRequest<K>,
): Promise<AutomationResponse<K>> {
  return api<AutomationResponse<K>>("automation/" + operation, request);
}

export type Attachment = {
  id: string;
  sha256: string;
  size: number;
  mime: string;
  name: string;
  owner: string;
  createdAt: string;
  unavailable?: boolean;
};

async function requireSuccess(response: Response): Promise<void> {
  if (response.ok) return;
  let message = "Request failed";
  try {
    const value: unknown = await response.json();
    if (typeof value === "object" && value !== null && "error" in value && typeof value.error === "string")
      message = value.error;
  } catch {
    // Binary endpoints may be rejected before a JSON response is available.
  }
  throw new APIError(response.status, message);
}

export async function uploadAttachment(
  file: Blob,
  name: string,
  conversationId?: string,
  signal?: AbortSignal,
): Promise<Attachment> {
  const query = new URLSearchParams();
  if (conversationId !== undefined) query.set("conversationId", conversationId);
  const response = await authenticatedFetch("attachments/upload" + (query.size ? "?" + query.toString() : ""), {
    method: "POST",
    headers: {
      "Content-Type": file.type || "application/octet-stream",
      "X-Meshra-Filename": encodeURIComponent(name),
    },
    body: file,
    signal,
  });
  await requireSuccess(response);
  return response.json() as Promise<Attachment>;
}

export async function downloadAttachment(id: string, signal?: AbortSignal): Promise<Blob> {
  const response = await authenticatedFetch("attachments/" + encodeURIComponent(id), { signal });
  await requireSuccess(response);
  return response.blob();
}
