package relay

import (
	"meshra/internal/model"
)

func (r *Relay) profileContextGrant(op string, q model.CollaborationRequest) (any, error) {
	if err := r.currentProfileAgent(q.AgentID); err != nil {
		return nil, err
	}
	if op == "context.revoke" {
		query := `DELETE FROM profile_context_grants WHERE binding_id=(SELECT id FROM profile_bindings WHERE agent_id=?) AND context_id=?`
		args := []any{q.AgentID, q.ContextID}
		if q.Version > 0 {
			query += " AND version=?"
			args = append(args, q.Version)
		}
		_, err := r.db.Exec(query, args...)
		return map[string]bool{"updated": err == nil}, err
	}
	if q.Version < 1 {
		return nil, model.Err(400, "profile context grants require an exact pinned version")
	}
	var count int
	if err := r.db.QueryRow(`SELECT count(*) FROM profile_bindings b JOIN context_bindings c ON c.conversation_id=b.conversation_id WHERE b.agent_id=? AND c.context_id=? AND c.version=?`, q.AgentID, q.ContextID, q.Version).Scan(&count); err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, model.Err(403, "context version is not pinned to this profile conversation")
	}
	_, err := r.db.Exec(`INSERT OR IGNORE INTO profile_context_grants(binding_id,context_id,version) SELECT id,?,? FROM profile_bindings WHERE agent_id=?`, q.ContextID, q.Version, q.AgentID)
	return map[string]bool{"updated": err == nil}, err
}
