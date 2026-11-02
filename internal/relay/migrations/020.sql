-- Remove the Codex, OpenCode and ACP harnesses. Run inside a transaction while
-- foreign keys are OFF, so every dependent row is removed explicitly here.
CREATE TEMP TABLE gone_agents AS SELECT id FROM agents WHERE json_extract(body,'$.harness') IN ('codex','opencode','acp');
CREATE TEMP TABLE gone_profiles AS SELECT id FROM agent_profiles WHERE json_extract(body,'$.binding.harness') IN ('codex','opencode','acp');
INSERT OR IGNORE INTO gone_agents SELECT agent_id FROM profile_bindings WHERE profile_id IN (SELECT id FROM gone_profiles);
CREATE TEMP TABLE gone_tasks AS SELECT id FROM tasks WHERE sender IN (SELECT id FROM gone_agents) OR recipient IN (SELECT id FROM gone_agents);
CREATE TEMP TABLE gone_invocations AS SELECT id FROM tool_invocations WHERE task_id IN (SELECT id FROM gone_tasks);

DELETE FROM workspace_apply_journal WHERE changeset_id IN (SELECT id FROM workspace_changesets WHERE invocation_id IN (SELECT id FROM gone_invocations));
DELETE FROM workspace_changesets WHERE invocation_id IN (SELECT id FROM gone_invocations);
DELETE FROM tool_approvals WHERE invocation_id IN (SELECT id FROM gone_invocations);
DELETE FROM tool_invocations WHERE id IN (SELECT id FROM gone_invocations);
DELETE FROM connector_outbox WHERE task_id IN (SELECT id FROM gone_tasks);
DELETE FROM attachment_tasks WHERE task_id IN (SELECT id FROM gone_tasks);
DELETE FROM profile_task_snapshots WHERE task_id IN (SELECT id FROM gone_tasks) OR profile_id IN (SELECT id FROM gone_profiles);
DELETE FROM task_dispatch_order WHERE task_id IN (SELECT id FROM gone_tasks) OR agent_id IN (SELECT id FROM gone_agents);
DELETE FROM events WHERE task_id IN (SELECT id FROM gone_tasks);
DELETE FROM credentials WHERE task_id IN (SELECT id FROM gone_tasks) OR agent_id IN (SELECT id FROM gone_agents);
DELETE FROM tasks WHERE id IN (SELECT id FROM gone_tasks);

DELETE FROM profile_context_epochs WHERE binding_id IN (SELECT id FROM profile_bindings WHERE profile_id IN (SELECT id FROM gone_profiles) OR agent_id IN (SELECT id FROM gone_agents));
DELETE FROM profile_context_grants WHERE binding_id IN (SELECT id FROM profile_bindings WHERE profile_id IN (SELECT id FROM gone_profiles) OR agent_id IN (SELECT id FROM gone_agents));
DELETE FROM profile_bindings WHERE profile_id IN (SELECT id FROM gone_profiles) OR agent_id IN (SELECT id FROM gone_agents);
DELETE FROM profile_memories WHERE profile_id IN (SELECT id FROM gone_profiles);
DELETE FROM profile_history WHERE profile_id IN (SELECT id FROM gone_profiles);
DELETE FROM profile_skill_bindings WHERE profile_id IN (SELECT id FROM gone_profiles);
DELETE FROM profile_mcp_bindings WHERE profile_id IN (SELECT id FROM gone_profiles);
DELETE FROM profile_tool_policies WHERE profile_id IN (SELECT id FROM gone_profiles);
DELETE FROM profile_conversations WHERE profile_id IN (SELECT id FROM gone_profiles);
DELETE FROM browser_sessions WHERE profile_id IN (SELECT id FROM gone_profiles);
DELETE FROM connector_routes WHERE profile_id IN (SELECT id FROM gone_profiles);
DELETE FROM network_grants WHERE profile_id IN (SELECT id FROM gone_profiles) OR connection_id IN (SELECT id FROM acp_agents);
DELETE FROM agent_profiles WHERE id IN (SELECT id FROM gone_profiles);

DELETE FROM agent_queue_state WHERE agent_id IN (SELECT id FROM gone_agents);
DELETE FROM conversation_participants WHERE agent_id IN (SELECT id FROM gone_agents);
DELETE FROM context_acl WHERE agent_id IN (SELECT id FROM gone_agents);
DELETE FROM agents WHERE id IN (SELECT id FROM gone_agents);

-- Conversations left with no participant and no profile are unreachable.
CREATE TEMP TABLE gone_conversations AS SELECT id FROM conversations c WHERE NOT EXISTS (SELECT 1 FROM conversation_participants p WHERE p.conversation_id=c.id) AND NOT EXISTS (SELECT 1 FROM profile_conversations p WHERE p.conversation_id=c.id) AND NOT EXISTS (SELECT 1 FROM tasks t WHERE json_extract(t.body,'$.conversationId')=c.id AND t.state IN ('queued','dispatching','running'));
DELETE FROM events WHERE task_id IN (SELECT id FROM tasks WHERE json_extract(body,'$.conversationId') IN (SELECT id FROM gone_conversations));
DELETE FROM profile_task_snapshots WHERE task_id IN (SELECT id FROM tasks WHERE json_extract(body,'$.conversationId') IN (SELECT id FROM gone_conversations));
DELETE FROM attachment_tasks WHERE task_id IN (SELECT id FROM tasks WHERE json_extract(body,'$.conversationId') IN (SELECT id FROM gone_conversations));
DELETE FROM profile_memories WHERE conversation_id IN (SELECT id FROM gone_conversations);
DELETE FROM profile_history WHERE conversation_id IN (SELECT id FROM gone_conversations);
DELETE FROM context_bindings WHERE conversation_id IN (SELECT id FROM gone_conversations);
DELETE FROM conversation_events WHERE conversation_id IN (SELECT id FROM gone_conversations);
DELETE FROM tasks WHERE json_extract(body,'$.conversationId') IN (SELECT id FROM gone_conversations);
DELETE FROM conversations WHERE id IN (SELECT id FROM gone_conversations);

DELETE FROM automation_secrets WHERE kind='acp.env';
DROP TABLE acp_agents;
DROP TABLE gone_conversations; DROP TABLE gone_invocations; DROP TABLE gone_tasks; DROP TABLE gone_profiles; DROP TABLE gone_agents;
UPDATE schema_version SET version=20;
