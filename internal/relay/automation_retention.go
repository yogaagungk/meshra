package relay

import (
	"database/sql"
	"time"

	"meshra/internal/model"
)

// Feature history has its own lifetime. A negative legacy task retention disables
// task pruning, not expiration of terminal automation history or sensitive files.
const automationHistoryRetention = 30 * 24 * time.Hour
const automationRetentionBatch = 256
const connectorPendingLimit = 4096

// reserveConnectorCapacityTx checks capacity in the same write transaction that
// accepts new work. The caller serializes admission with relay ownership. Counts
// are additions, not target totals; retries reserve one outbox slot. No accepted
// event or unresolved delivery is evicted to make room. A transport must leave an
// event unacknowledged when its durable receipt cannot be inserted.
func reserveConnectorCapacityTx(tx *sql.Tx, inbox, outbox int) error {
	if inbox < 0 || outbox < 0 {
		return model.Err(400, "invalid connector capacity reservation")
	}
	for _, bound := range []struct {
		addition int
		query    string
	}{
		{inbox, `SELECT count(*) FROM (SELECT 1 FROM connector_inbox WHERE state NOT IN ('invalid','ignored_bot','denied','failed') AND (state<>'admitted' OR NOT EXISTS(SELECT 1 FROM tasks t WHERE t.id=connector_inbox.task_id AND t.state IN ('completed','failed','cancelled','timed_out'))) LIMIT 4096)`},
		{outbox, `SELECT count(*) FROM (SELECT 1 FROM connector_outbox WHERE state NOT IN ('sent','failed') LIMIT 4096)`},
	} {
		if bound.addition == 0 {
			continue
		}
		var count int
		if err := tx.QueryRow(bound.query).Scan(&count); err != nil {
			return err
		}
		if bound.addition > connectorPendingLimit || count > connectorPendingLimit-bound.addition {
			return model.Err(429, "connector pending capacity reached; retry without acknowledging the event")
		}
	}
	return nil
}

// These predicates deliberately treat unknown states and uncertain effects as
// live. All task aliases are t; retention_active_tasks also protects terminal
// tasks whose adapter has not returned yet.
const retentionOpenTaskSQL = `(
 t.state NOT IN ('completed','failed','cancelled','timed_out')
 OR EXISTS(SELECT 1 FROM retention_active_tasks a WHERE a.task_id=t.id)
 OR EXISTS(SELECT 1 FROM tool_invocations i WHERE i.task_id=t.id AND i.state NOT IN ('completed','failed','cancelled','denied'))
 OR EXISTS(SELECT 1 FROM tool_invocations i JOIN workspace_changesets c ON c.invocation_id=i.id WHERE i.task_id=t.id)
 OR EXISTS(SELECT 1 FROM connector_outbox o WHERE o.task_id=t.id AND o.state NOT IN ('sent','failed'))
 OR EXISTS(SELECT 1 FROM connector_inbox x WHERE x.task_id=t.id AND x.state NOT IN ('admitted','invalid','ignored_bot','denied','failed'))
 OR EXISTS(SELECT 1 FROM browser_sessions b WHERE b.task_id=t.id AND b.state NOT IN ('closed','error'))
)`

// Retained receipts must not lose their task or cause a source event/reply to be
// admitted twice. Linked inbox receipts and one final terminal outbox marker
// survive until their source task is removed in the same transaction.
const retentionTaskReferencesSQL = `(
 EXISTS(SELECT 1 FROM routine_steps s WHERE s.task_id=t.id)
 OR EXISTS(SELECT 1 FROM routine_runs rr,json_each(rr.body,'$.taskIds') j WHERE j.value=t.id)
 OR EXISTS(SELECT 1 FROM connector_outbox o WHERE o.task_id=t.id AND (o.state NOT IN ('sent','failed') OR julianday(json_extract(o.body,'$.updatedAt')) IS NULL OR julianday(json_extract(o.body,'$.updatedAt'))>=julianday(:feature_cutoff)))
 OR EXISTS(SELECT 1 FROM connector_inbox x WHERE x.task_id=t.id AND (x.state NOT IN ('admitted','invalid','ignored_bot','denied','failed') OR julianday(x.received_at) IS NULL OR julianday(x.received_at)>=julianday(:feature_cutoff) OR EXISTS(SELECT 1 FROM routine_trigger_receipts p WHERE p.event_id=x.event_id)))
 OR EXISTS(SELECT 1 FROM tool_invocations i WHERE i.task_id=t.id)
 OR EXISTS(SELECT 1 FROM browser_sessions b WHERE b.task_id=t.id)
)`

func (r *Relay) retentionActiveTasksTx(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS retention_active_tasks(task_id TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM retention_active_tasks`); err != nil {
		return err
	}
	for _, slot := range r.slots {
		if slot.taskID != "" {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO retention_active_tasks(task_id) VALUES(?)`, slot.taskID); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneAutomationHistory is called with relay ownership and performs database
// work only. Browser/voice lifecycles must expire ephemeral artifacts before
// these terminal control records become eligible; retention never stops workers
// or pretends that a closed state proves that private files were removed.
func (r *Relay) pruneAutomationHistory(before time.Time) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = r.retentionActiveTasksTx(tx); err != nil {
		return err
	}
	cutoff := before.UTC().Format(time.RFC3339Nano)
	if _, err = tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS retention_runs(id TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	for {
		if _, err = tx.Exec(`DELETE FROM retention_runs`); err != nil {
			return err
		}
		result, err := tx.Exec(`INSERT INTO retention_runs(id)
 SELECT rr.id FROM routine_runs rr
 WHERE rr.state IN ('completed','failed','cancelled','skipped')
 AND julianday(json_extract(rr.body,'$.updatedAt'))<julianday(?)
 AND (coalesce(json_extract(rr.body,'$.conversationId'),'')='' OR coalesce(json_extract(rr.body,'$.bindingsReleased'),0)=1)
 AND NOT EXISTS(SELECT 1 FROM routine_steps s WHERE s.run_id=rr.id AND s.state NOT IN ('completed','failed','cancelled','skipped'))
 AND NOT EXISTS(SELECT 1 FROM json_each(rr.body,'$.steps') s WHERE coalesce(json_extract(s.value,'$.state'),'') NOT IN ('completed','failed','cancelled','skipped'))
 AND NOT EXISTS(SELECT 1 FROM routine_steps s JOIN tasks t ON t.id=s.task_id WHERE s.run_id=rr.id AND `+retentionOpenTaskSQL+`)
 AND NOT EXISTS(SELECT 1 FROM json_each(rr.body,'$.taskIds') j JOIN tasks t ON t.id=j.value WHERE `+retentionOpenTaskSQL+`)
 ORDER BY rr.rowid LIMIT 256`, cutoff)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			break
		}
		// Receipts have no FK to runs. Remove them before their source, while
		// steps use ON DELETE CASCADE in the same transaction.
		if _, err = tx.Exec(`DELETE FROM routine_trigger_receipts WHERE run_id IN (SELECT id FROM retention_runs)`); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM routine_runs WHERE id IN (SELECT id FROM retention_runs)`); err != nil {
			return err
		}
	}
	queries := []string{
		`DELETE FROM connector_inbox WHERE rowid IN (SELECT x.rowid FROM connector_inbox x
 WHERE x.state IN ('admitted','invalid','ignored_bot','denied','failed') AND julianday(x.received_at)<julianday(?)
 AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.id=x.task_id)
 AND NOT EXISTS(SELECT 1 FROM routine_steps s WHERE x.task_id<>'' AND s.task_id=x.task_id)
 ORDER BY x.rowid LIMIT 256)`,
		`DELETE FROM connector_outbox WHERE rowid IN (SELECT o.rowid FROM connector_outbox o
 WHERE o.state IN ('sent','failed') AND julianday(json_extract(o.body,'$.updatedAt'))<julianday(?)
 AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.id=o.task_id AND ` + retentionOpenTaskSQL + `)
 AND EXISTS(SELECT 1 FROM connector_outbox marker WHERE marker.task_id=o.task_id AND marker.chunk<o.chunk)
 ORDER BY o.rowid LIMIT 256)`,
		`DELETE FROM browser_sessions WHERE rowid IN (SELECT b.rowid FROM browser_sessions b
 WHERE b.state IN ('closed','error') AND julianday(json_extract(b.body,'$.updatedAt'))<julianday(?)
 AND coalesce(json_extract(b.body,'$.artifactsExpired'),0)=1
 AND coalesce(json_extract(b.body,'$.snapshotText'),'')=''
 AND coalesce(json_extract(b.body,'$.leaseOwner'),'')=''
 AND json_extract(b.body,'$.leaseExpiresAt') IS NULL
 AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.id=b.task_id AND ` + retentionOpenTaskSQL + `)
 ORDER BY b.rowid LIMIT 256)`,
		`DELETE FROM voice_jobs WHERE rowid IN (SELECT rowid FROM voice_jobs
 WHERE state IN ('expired','cancelled','failed') AND julianday(expires_at)<julianday(?)
 AND julianday(json_extract(body,'$.updatedAt'))<julianday(?)
 AND coalesce(json_extract(body,'$.audioAvailable'),0)=0 AND coalesce(json_extract(body,'$.audioFile'),'')=''
 ORDER BY rowid LIMIT 256)`,
		`DELETE FROM template_import_previews WHERE rowid IN (SELECT rowid FROM template_import_previews WHERE julianday(expires_at)<julianday(?) ORDER BY rowid LIMIT 256)`,
		`DELETE FROM mcp_auth_flows WHERE rowid IN (SELECT rowid FROM mcp_auth_flows WHERE julianday(expires_at)<julianday(?) ORDER BY rowid LIMIT 256)`,
		`DELETE FROM routine_execution_tokens WHERE rowid IN (SELECT rowid FROM routine_execution_tokens WHERE julianday(nullif(revoked_at,''))<julianday(?) OR julianday(nullif(expires_at,''))<julianday(?) ORDER BY rowid LIMIT 256)`,
	}
	for i, query := range queries {
		args := []any{cutoff}
		if i == 3 || i == len(queries)-1 {
			args = append(args, cutoff)
		}
		if err = retentionDeleteBatches(tx, query, args...); err != nil {
			return err
		}
	}
	// Definitions, memories, immutable skill/flow versions, and binding
	// revisions are library/user data, not expiring execution history.
	if err = retentionDeleteBatches(tx, `DELETE FROM automation_events WHERE seq IN (
 SELECT e.seq FROM automation_events e WHERE julianday(e.time)<julianday(?)
 AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.id=e.resource_id AND `+retentionOpenTaskSQL+`)
 AND NOT EXISTS(SELECT 1 FROM tool_invocations i JOIN tasks t ON t.id=i.task_id WHERE i.id=e.resource_id AND `+retentionOpenTaskSQL+`)
 AND NOT EXISTS(SELECT 1 FROM routine_steps s WHERE s.task_id=e.resource_id)
 AND NOT EXISTS(SELECT 1 FROM routine_runs rr,json_each(rr.body,'$.taskIds') j WHERE j.value=e.resource_id)
 AND NOT EXISTS(SELECT 1 FROM workspace_changesets c WHERE c.id=e.resource_id)
 AND NOT EXISTS(SELECT 1 FROM routine_runs rr WHERE rr.id=e.resource_id)
 AND NOT EXISTS(SELECT 1 FROM connector_outbox o WHERE o.id=e.resource_id AND o.state NOT IN ('sent','failed'))
 AND NOT EXISTS(SELECT 1 FROM browser_sessions b WHERE b.id=e.resource_id)
 AND NOT EXISTS(SELECT 1 FROM voice_jobs v WHERE v.id=e.resource_id)
 ORDER BY e.seq LIMIT 256)`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

func retentionDeleteBatches(tx *sql.Tx, query string, args ...any) error {
	for {
		result, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
	}
}

// pruneUnownedAttachments runs after task ownership cascades have committed.
// Marking metadata unavailable and unlinking bytes uses the existing crash-safe
// attachment lifecycle. The final metadata delete cannot race another owner
// because the caller holds relay ownership throughout.
func (r *Relay) pruneUnownedAttachments(before time.Time) error {
	const unowned = `NOT EXISTS(SELECT 1 FROM attachment_conversations ac WHERE ac.attachment_id=a.id)
 AND NOT EXISTS(SELECT 1 FROM attachment_tasks at WHERE at.attachment_id=a.id)
 AND NOT EXISTS(SELECT 1 FROM agent_profiles p WHERE json_extract(p.body,'$.avatarAttachmentId')=a.id)
 AND NOT EXISTS(SELECT 1 FROM local_skill_versions s,json_each(s.body,'$.assetAttachmentIds') x WHERE x.value=a.id)`
	for {
		rows, err := r.db.Query(`SELECT a.id FROM attachments a WHERE julianday(a.created_at)<julianday(?) AND `+unowned+` ORDER BY a.id LIMIT 256`, before.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		ids := make([]string, 0, automationRetentionBatch)
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			a, err := r.attachment(id)
			if err != nil {
				return err
			}
			if err = r.removeAttachment(a); err != nil {
				return err
			}
			if _, err = r.db.Exec(`DELETE FROM attachments AS a WHERE id=? AND `+unowned, id); err != nil {
				return err
			}
		}
	}
}
