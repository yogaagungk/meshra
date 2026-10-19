#!/usr/bin/env node
// One query per process. Only this narrow JSONL protocol is exposed to Go.
import { query } from '@anthropic-ai/claude-agent-sdk';
import { createInterface } from 'node:readline';

const emit = (type, data) => process.stdout.write(JSON.stringify({ type, data }) + '\n');
const abortController = new AbortController();
process.on('SIGTERM', () => abortController.abort());
process.on('SIGINT', () => abortController.abort());
let token = '';
const safeError = (error) => String(error?.message ?? error).replaceAll(token || '\0', '[redacted]').slice(0, 4096);
const readOnly = new Set(['Read', 'Glob', 'Grep']);
const meshraTools = new Set(['meshra_agents_list', 'meshra_agent_status', 'meshra_send', 'meshra_request', 'meshra_task_status', 'meshra_task_events', 'meshra_task_cancel', 'meshra_task_trace', 'meshra_collaboration_conversation_create', 'meshra_collaboration_conversation_get', 'meshra_collaboration_conversation_list', 'meshra_collaboration_conversation_addParticipant', 'meshra_collaboration_conversation_events', 'meshra_collaboration_context_publish', 'meshra_collaboration_context_get', 'meshra_collaboration_context_search', 'meshra_collaboration_context_attach', 'meshra_collaboration_context_grant', 'meshra_collaboration_context_revoke', 'meshra_collaboration_context_audit', 'meshra_collaboration_task_delegate', 'meshra_collaboration_task_list']);
const allowed = (name) => readOnly.has(name) || (name.startsWith('mcp__meshra__') && meshraTools.has(name.slice('mcp__meshra__'.length)));
const denied = new Set();
function deny(name, id) {
  const key = id || name;
  if (!denied.has(key)) {
    denied.add(key);
    emit('approval_required', `Denied tool ${name}: Meshra has no interactive approval channel`);
  }
}
let nativeSession = '';
function session(id) {
  if (!id || id === nativeSession) return;
  nativeSession = id;
  emit('session', id);
}
try {
  const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
  let input;
  for await (const line of lines) { input = JSON.parse(line); lines.close(); break; }
  if (!input || typeof input.prompt !== 'string') throw new Error('Missing bridge prompt');
  token = input.token || '';
  const options = {
    cwd: input.cwd,
    abortController,
    permissionMode: 'default',
    allowDangerouslySkipPermissions: false,
    settingSources: [],
    strictMcpConfig: true,
    includePartialMessages: true,
    persistSession: true,
    // Enforce before permission auto-approval, including inherited rules.
    hooks: {
      SessionStart: [{ hooks: [async (event) => { session(event.session_id); return {}; }] }],
      PreToolUse: [{ hooks: [async (event) => {
        if (allowed(event.tool_name)) return {};
        deny(event.tool_name, event.tool_use_id);
        return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'Meshra denies mutating, shell, subagent, and unknown tools' } };
      }] }],
    },
    canUseTool: async (name, toolInput, event) => {
      if (allowed(name)) return { behavior: 'allow', updatedInput: toolInput };
      deny(name, event.toolUseID);
      return { behavior: 'deny', message: 'Meshra requires human approval; no approval channel is available' };
    },
    mcpServers: input.mcpURL && token ? { meshra: { type: 'http', url: input.mcpURL, headers: { Authorization: `Bearer ${token}` } } } : {},
  };
  if (input.sessionId) options.resume = input.sessionId;
  if (input.model) options.model = input.model;
  let resultSeen = false;
  const conversation = query({ prompt: input.prompt, options });
  try {
    for await (const message of conversation) {
      if (message.type === 'system' && message.subtype === 'init') session(message.session_id);
      if (message.type === 'stream_event' && message.event?.type === 'content_block_delta' && message.event.delta?.type === 'text_delta') emit('delta', message.event.delta.text);
      if (message.type === 'system' && message.subtype === 'permission_denied') deny(message.tool_name, message.tool_use_id);
      if (message.type === 'result') {
        session(message.session_id);
        for (const deniedTool of message.permission_denials || []) deny(deniedTool.tool_name, deniedTool.tool_use_id);
        resultSeen = true;
        if (message.subtype !== 'success' || message.is_error) throw new Error(`${message.subtype}: ${(message.errors || [message.result || 'Claude query failed']).join('; ')}`);
        emit('result', message.result || '');
      }
    }
    if (!resultSeen) throw new Error('Claude SDK ended without a result');
  } finally {
    conversation.close();
  }
} catch (error) {
  emit('error', safeError(error));
  process.exitCode = 1;
}
