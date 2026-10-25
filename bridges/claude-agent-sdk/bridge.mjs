#!/usr/bin/env node
// One query per process. Only this narrow JSONL protocol is exposed to Go.
import * as sdk from '@anthropic-ai/claude-agent-sdk';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { execFileSync } from 'node:child_process';
import { createInterface } from 'node:readline';

const { query } = sdk;
let generationDraft = false;
let profileManaged = false;
let brokerTools = new Set();
const emit = (type, data) => process.stdout.write(JSON.stringify({ type, data }) + '\n');
const abortController = new AbortController();
process.on('SIGTERM', () => abortController.abort());
process.on('SIGINT', () => abortController.abort());
let token = '';
let scopedToken = '';
const safeError = (error) => [token, scopedToken].filter(Boolean).reduce(
  (text, secret) => text.replaceAll(secret, '[redacted]'), String(error?.message ?? error)).slice(0, 4096);
const readOnly = new Set(['Read', 'Glob', 'Grep']);
const meshraTools = new Set(['meshra_agents_list', 'meshra_agent_status', 'meshra_send', 'meshra_request', 'meshra_task_status', 'meshra_task_events', 'meshra_task_cancel', 'meshra_task_trace', 'meshra_collaboration_conversation_create', 'meshra_collaboration_conversation_get', 'meshra_collaboration_conversation_list', 'meshra_collaboration_conversation_addParticipant', 'meshra_collaboration_conversation_events', 'meshra_collaboration_context_publish', 'meshra_collaboration_context_get', 'meshra_collaboration_context_search', 'meshra_collaboration_context_attach', 'meshra_collaboration_context_grant', 'meshra_collaboration_context_revoke', 'meshra_collaboration_context_audit', 'meshra_collaboration_task_delegate', 'meshra_collaboration_task_list']);
const allowed = (name) => !generationDraft && (profileManaged ? brokerTools.has(name) :
  readOnly.has(name) || (name.startsWith('mcp__meshra__') && meshraTools.has(name.slice('mcp__meshra__'.length))));
const denied = new Set();
function deny(name, id) {
  const key = id || name;
  if (!denied.has(key)) {
    denied.add(key);
    emit('approval_required', `Denied tool ${name}: Meshra has no interactive approval channel`);
  }
}
let nativeSession = '';
let readySession = '';
let sessionWaiter;
function acknowledge(frame) {
  if (frame?.type !== 'session_ready' || frame.data !== nativeSession || !sessionWaiter) {
    throw new Error('Unexpected Claude session acknowledgement');
  }
  readySession = frame.data;
  sessionWaiter.resolve();
  sessionWaiter = undefined;
}
async function session(id) {
  if (!id || (nativeSession && nativeSession !== id)) throw new Error('Claude returned an unexpected native session');
  if (profileManaged && readySession !== id) {
    if (!sessionWaiter) {
      nativeSession = id;
      let resolve, reject;
      const promise = new Promise((ok, fail) => { resolve = ok; reject = fail; });
      sessionWaiter = { promise, resolve, reject };
      emit('session', id);
    }
    await sessionWaiter.promise;
  } else if (id !== nativeSession) {
    nativeSession = id;
    emit('session', id);
  }
}
abortController.signal.addEventListener('abort', () => sessionWaiter?.reject(new Error('Claude session acknowledgement cancelled')), { once: true });
try {
  const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
  const iterator = lines[Symbol.asyncIterator]();
  const first = await iterator.next();
  const input = first.done ? undefined : JSON.parse(first.value);
  profileManaged = Array.isArray(input?.toolDefinitions);
  if (profileManaged) {
    // Keep the host channel independent of SDK tool promises and approvals.
    void (async () => {
      try {
        for await (const line of iterator) acknowledge(JSON.parse(line));
        if (!abortController.signal.aborted) abortController.abort();
      } catch (error) {
        sessionWaiter?.reject(error);
        abortController.abort();
      }
    })();
  } else {
    lines.close();
  }
  if (input?.discoverModels) {
    async function* noMessages() {
      await new Promise(resolve => abortController.signal.addEventListener('abort', resolve, { once: true }));
    }
    const catalog = query({ prompt: noMessages(), options: { cwd: input.cwd, abortController, settingSources: [], strictMcpConfig: true, mcpServers: {}, tools: [], persistSession: false } });
    try {
      const models = await catalog.supportedModels();
      emit('models', JSON.stringify(models.slice(0, 200).map(m => ({ id: m.value, name: m.displayName, provider: 'anthropic' }))));
    } finally { catalog.close(); abortController.abort(); }
    process.exit(0);
  }
  if (!input || typeof input.prompt !== 'string') throw new Error('Missing bridge prompt');
  generationDraft = input.generationDraft === true;
  if (generationDraft && profileManaged) throw new Error('Claude profile generation cannot receive broker authority');
  if (generationDraft || profileManaged) {
    if (generationDraft && input.sessionId) throw new Error('Claude profile generation cannot resume a native session');
    const installed = JSON.parse(readFileSync(new URL('./node_modules/@anthropic-ai/claude-agent-sdk/package.json', import.meta.url), 'utf8'));
    if (installed.version !== '0.3.303' || typeof sdk.resolveSettings !== 'function' || !Array.isArray(sdk.BUILTIN_TOOL_NAMES)) {
      throw new Error('Claude restricted profiles require pinned SDK 0.3.303; run npm ci --prefix bridges/claude-agent-sdk');
    }
    // Managed hooks/plugins can run before SDK hooks. Inspect the installed
    // resolver before opening a query; never rely on a post-prompt roster.
    const resolved = await sdk.resolveSettings({ cwd: input.cwd, settingSources: [] });
    if (!Array.isArray(resolved.sources) || resolved.sources.some(source => source.source === 'managed' && Object.keys(source.settings).length > 0)) {
      throw new Error('Claude restricted profiles are unavailable with managed native settings; use a supported harness without ambient policy hooks/plugins');
    }
  }
  if (profileManaged) {
    const require = createRequire(import.meta.url);
    const nativePackage = require.resolve(`@anthropic-ai/claude-agent-sdk-${process.platform}-${process.arch}/package.json`);
    const native = JSON.parse(readFileSync(nativePackage, 'utf8'));
    if (native.version !== '0.3.303') throw new Error('Claude profile automation requires the pinned native SDK executable; run npm ci --prefix bridges/claude-agent-sdk');
    input.nativeExecutable = join(dirname(nativePackage), process.platform === 'win32' ? 'claude.exe' : 'claude');
    if (execFileSync(input.nativeExecutable, ['--version'], { encoding: 'utf8', timeout: 5000 }).trim() !== '2.1.303 (Claude Code)') {
      throw new Error('Claude profile automation requires native Claude Code 2.1.303; run npm ci --prefix bridges/claude-agent-sdk');
    }
    const names = input.toolDefinitions.map(tool => tool.name);
    if (names.some(name => typeof name !== 'string' || !/^meshra_[A-Za-z0-9_]+$/.test(name)) || new Set(names).size !== names.length) {
      throw new Error('Invalid exact Meshra broker tool catalog');
    }
    brokerTools = new Set(names.map(name => `mcp__meshra__${name}`));
    if (input.checkProfileAutomation === true) {
      emit('result', 'profileAutomation');
      process.exit(0);
    }
    if (!input.toolSocket?.startsWith('/') || !input.toolSocket.endsWith('.sock') || !input.toolToken ||
        !input.toolBridge?.startsWith('/')) throw new Error('Claude profile automation requires the scoped Meshra stdio tool endpoint');
    scopedToken = input.toolToken;
  }
  if (input.checkGenerationDraft === true && generationDraft) {
    emit('result', 'toolFreeDraft');
    process.exit(0);
  }
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
      SessionStart: [{ hooks: [async (event) => { await session(event.session_id); return {}; }] }],
      PreToolUse: [{ hooks: [async (event) => {
        if (profileManaged) await session(event.session_id);
        if (allowed(event.tool_name)) return {};
        deny(event.tool_name, event.tool_use_id);
        return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'Meshra denies native, subagent, foreign MCP, and unknown tools' } };
      }] }],
    },
    canUseTool: async (name, toolInput, event) => {
      if (profileManaged && (!nativeSession || readySession !== nativeSession)) return { behavior: 'deny', message: 'Meshra native session has not been bound to the active task' };
      if (allowed(name)) return { behavior: 'allow', updatedInput: toolInput };
      deny(name, event.toolUseID);
      return { behavior: 'deny', message: 'Meshra requires human approval; no approval channel is available' };
    },
    mcpServers: profileManaged ? { meshra: { command: process.execPath, args: [input.toolBridge],
      env: { MESHRA_TOOL_SOCKET: input.toolSocket, MESHRA_TOOL_TOKEN: scopedToken } } } :
      !generationDraft && input.mcpURL && token ? { meshra: { type: 'http', url: input.mcpURL, headers: { Authorization: `Bearer ${token}` } } } : {},
  };
  if (generationDraft || profileManaged) {
    options.tools = [];
    options.allowedTools = profileManaged ? [...brokerTools] : [];
    options.disallowedTools = [...sdk.BUILTIN_TOOL_NAMES, ...Object.keys(sdk.LEGACY_TOOL_NAME_ALIASES || {}), ...(generationDraft ? ['mcp__*'] : [])];
    options.plugins = [];
    options.skills = [];
    options.agents = {};
    options.persistSession = !generationDraft;
    options.enableFileCheckpointing = false;
    options.promptSuggestions = false;
    options.agentProgressSummaries = false;
    options.settings = {
      disableAllHooks: generationDraft,
      disableBundledSkills: true,
      disableSkillShellExecution: true,
      autoMemoryEnabled: false,
      autoDreamEnabled: false,
      syncClaudeAiSkills: false,
      syncClaudeAiPlugins: false,
      claudeMdExcludes: ['**'],
    };
    options.env = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
      (!key.startsWith('CLAUDE_') || key === 'CLAUDE_CODE_OAUTH_TOKEN' || key === 'CLAUDE_CONFIG_DIR') &&
      !key.startsWith('MESHRA_') && key !== 'NODE_OPTIONS' && key !== 'BUN_OPTIONS'));
    Object.assign(options.env, {
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1',
      CLAUDE_CODE_DISABLE_BACKGROUND_TASKS: '1',
    });
  }
  if (profileManaged) options.pathToClaudeCodeExecutable = input.nativeExecutable;
  if (input.sessionId) options.resume = input.sessionId;
  if (input.model) options.model = input.model;
  let resultSeen = false;
  let draftInitialized = false;
  let latestAssistant; 
  const conversation = query({ prompt: input.prompt, options });
  try {
    for await (const message of conversation) {
      if (message.type === 'assistant' && message.message?.usage) latestAssistant = message.message;
      if (message.type === 'system' && message.subtype === 'init') {
        if (generationDraft && (message.claude_code_version !== '2.1.303' ||
            ['tools', 'mcp_servers', 'plugins', 'skills'].some(key => !Array.isArray(message[key]) || message[key].length))) {
          abortController.abort();
          throw new Error('Claude native session did not enforce the empty profile-generation tool/skill/plugin roster');
        }
        if (profileManaged && (message.claude_code_version !== '2.1.303' ||
            !Array.isArray(message.tools) || message.tools.length !== brokerTools.size || message.tools.some(name => !brokerTools.has(name)) ||
            !Array.isArray(message.mcp_servers) || message.mcp_servers.length !== 1 || message.mcp_servers[0].name !== 'meshra' || message.mcp_servers[0].status !== 'connected' ||
            ['plugins', 'skills'].some(key => !Array.isArray(message[key]) || message[key].length))) {
          abortController.abort();
          throw new Error('Claude native session did not enforce the exact broker-only tool roster');
        }
        draftInitialized = true;
        await session(message.session_id);
      }
      if (message.type === 'stream_event' && message.event?.type === 'content_block_delta' && message.event.delta?.type === 'text_delta') emit('delta', message.event.delta.text);
      if (message.type === 'system' && message.subtype === 'permission_denied') deny(message.tool_name, message.tool_use_id);
      if (message.type === 'result') {
        if ((generationDraft || profileManaged) && !draftInitialized) throw new Error('Claude restricted profile received a result without an enforced native tool roster');
        await session(message.session_id);
        if (message.usage) emit('usage', JSON.stringify({ inputTokens: message.usage.input_tokens + (message.usage.cache_read_input_tokens || 0) + (message.usage.cache_creation_input_tokens || 0), outputTokens: message.usage.output_tokens, cachedInputTokens: (message.usage.cache_read_input_tokens || 0) + (message.usage.cache_creation_input_tokens || 0) }));
        const modelInfo = latestAssistant && message.modelUsage?.[latestAssistant.model];
        if (latestAssistant && modelInfo?.contextWindow > 0) {
          const u = latestAssistant.usage;
          emit('context_usage', JSON.stringify({ usedTokens: u.input_tokens + (u.cache_read_input_tokens || 0) + (u.cache_creation_input_tokens || 0) + u.output_tokens, windowTokens: modelInfo.contextWindow, estimated: false, source: 'Claude latest assistant message' }));
        }
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
  if (profileManaged) { lines.close(); process.stdin.destroy(); }
} catch (error) {
  abortController.abort();
  if (profileManaged) process.stdin.destroy();
  emit('error', safeError(error));
  process.exitCode = 1;
}
