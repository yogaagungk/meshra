package relay

import "meshra/internal/model"

// deleteConversation runs under r.mu, so no task can be admitted while the
// active-work check and deletion are performed.
func (r *Relay) deleteConversation(q model.ConsoleRequest) (any, error) {
	if q.ConversationID == "" || !q.Confirm {
		return nil, model.Err(400, "conversationId and confirmation are required")
	}
	if err := r.member(model.Principal{Admin: true}, q.ConversationID, true); err != nil {
		return nil, err
	}
	rows, err := r.db.Query(`SELECT agent_id FROM profile_bindings WHERE conversation_id=?`, q.ConversationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var agentID string
		if err = rows.Scan(&agentID); err != nil {
			rows.Close()
			return nil, err
		}
		if _, running := r.slots[agentID]; running {
			rows.Close()
			return nil, model.Err(409, "stop profile bindings before deleting this conversation")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var active int
	if err = tx.QueryRow(`SELECT count(*) FROM tasks WHERE json_extract(body,'$.conversationId')=? AND state IN ('queued','dispatching','running')`, q.ConversationID).Scan(&active); err != nil {
		return nil, err
	}
	if active != 0 {
		return nil, model.Err(409, "cancel or finish active tasks before deleting this conversation")
	}
	if err = tx.QueryRow(`SELECT count(*) FROM browser_sessions WHERE conversation_id=? AND state NOT IN ('closed','error')`, q.ConversationID).Scan(&active); err != nil {
		return nil, err
	}
	if active != 0 {
		return nil, model.Err(409, "close browser sessions before deleting this conversation")
	}
	// Keep shared contexts, files, usage records, and operator-scoped memory.
	// Remove only conversation-scoped data and the transcript's task records.
	if err = tx.QueryRow(`SELECT count(*) FROM connector_outbox o JOIN tasks t ON t.id=o.task_id WHERE json_extract(t.body,'$.conversationId')=? AND o.state NOT IN ('sent','failed')`, q.ConversationID).Scan(&active); err != nil {
		return nil, err
	}
	if active != 0 {
		return nil, model.Err(409, "wait for pending connector deliveries before deleting this conversation")
	}
	if err = tx.QueryRow(`SELECT count(*) FROM tool_invocations i JOIN tasks t ON t.id=i.task_id WHERE json_extract(t.body,'$.conversationId')=? AND i.state NOT IN ('completed','failed','cancelled','denied')`, q.ConversationID).Scan(&active); err != nil {
		return nil, err
	}
	if active != 0 {
		return nil, model.Err(409, "resolve active tool invocations before deleting this conversation")
	}
	for _, statement := range []string{
		`DELETE FROM events WHERE task_id IN (SELECT id FROM tasks WHERE json_extract(body,'$.conversationId')=?)`,
		`DELETE FROM credentials WHERE task_id IN (SELECT id FROM tasks WHERE json_extract(body,'$.conversationId')=?)`,
		`DELETE FROM profile_memories WHERE conversation_id=?`,
		`DELETE FROM context_bindings WHERE conversation_id=?`,
		`DELETE FROM profile_history WHERE conversation_id=?`,
		`DELETE FROM profile_task_snapshots WHERE task_id IN (SELECT id FROM tasks WHERE json_extract(body,'$.conversationId')=?)`,
		`DELETE FROM attachment_tasks WHERE task_id IN (SELECT id FROM tasks WHERE json_extract(body,'$.conversationId')=?)`,
		`DELETE FROM conversation_events WHERE conversation_id=?`,
		`DELETE FROM conversation_participants WHERE conversation_id=?`,
		`DELETE FROM tasks WHERE json_extract(body,'$.conversationId')=? AND NOT EXISTS (SELECT 1 FROM tool_invocations i JOIN workspace_changesets c ON c.invocation_id=i.id WHERE i.task_id=tasks.id)`,
		`UPDATE tasks SET body=json_remove(body,'$.conversationId','$.text','$.result','$.error','$.contextRefs','$.attachmentIds','$.resultRef','$.idempotencyKey','$.requestHash'), idempotency_key=NULL WHERE json_extract(body,'$.conversationId')=?`,
		`DELETE FROM conversations WHERE id=?`,
	} {
		if _, err = tx.Exec(statement, q.ConversationID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]bool{"deleted": true}, nil
}
