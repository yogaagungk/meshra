package relay

import (
	"database/sql"
	"errors"
)

// Called under relay ownership before binding materialization. Conversation
// participants share an audience, even after a trigger or connector is removed.
func (r *Relay) conversationExternalLocked(conversationID string) (bool, error) {
	var external bool
	err := r.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM profile_conversations c
 JOIN profile_bindings b ON b.profile_id=c.profile_id AND b.conversation_id=c.conversation_id
 WHERE c.conversation_id=? AND coalesce(json_extract(b.body,'$.externalConversation'),1)=1)`, conversationID).Scan(&external)
	return external, err
}

// Startup-only upgrade of pre-release bindings lacking audience metadata. Their
// erased source receipts cannot prove a private audience, so fail closed. Keep
// all stored history/memory, but rotate their next native context and cutoff;
// no historical task/session ownership is rewritten. Legacy named chats have no
// profile binding and are untouched. Every new private binding stores false.
func (r *Relay) recoverProfileExposure() error {
	for {
		tx, err := r.db.Begin()
		if err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT id,agent_id,profile_id,conversation_id FROM profile_bindings
 WHERE json_type(body,'$.externalConversation') IS NULL ORDER BY id LIMIT 64`)
		if err != nil {
			tx.Rollback()
			return err
		}
		type binding struct{ id, agent, profile, conversation string }
		entries := []binding{}
		for rows.Next() {
			var item binding
			if err = rows.Scan(&item.id, &item.agent, &item.profile, &item.conversation); err != nil {
				break
			}
			entries = append(entries, item)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			tx.Rollback()
			return err
		}
		if len(entries) == 0 {
			return tx.Commit()
		}
		for _, item := range entries {
			if _, err = tx.Exec("UPDATE profile_bindings SET body=json_set(body,'$.externalConversation',json('true')) WHERE id=?", item.id); err != nil {
				break
			}
			if _, err = tx.Exec("UPDATE agents SET body=json_remove(body,'$.sessionId') WHERE id=?", item.agent); err != nil {
				break
			}
			var cutoff int64
			if err = tx.QueryRow("SELECT coalesce(max(seq),0) FROM tasks WHERE json_extract(body,'$.conversationId')=?", item.conversation).Scan(&cutoff); err != nil {
				break
			}
			var changed sql.Result
			changed, err = tx.Exec("UPDATE profile_context_epochs SET epoch=epoch+1,rotate_pending=0,reset_after_seq=max(reset_after_seq,?) WHERE binding_id=?", cutoff, item.id)
			if err != nil {
				break
			}
			count, e := changed.RowsAffected()
			if e != nil {
				err = e
				break
			}
			if count != 1 {
				err = errors.New("profile audience recovery requires its context epoch")
				break
			}
			if _, err = tx.Exec("DELETE FROM credentials WHERE agent_id=?", item.agent); err != nil {
				break
			}
			if err = insertAutomationEvent(tx, "profiles.audienceRecovered", item.id, item.profile, map[string]any{"conversationId": item.conversation, "externalConversation": true}); err != nil {
				break
			}
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
}
