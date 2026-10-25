package relay

import (
	"database/sql"
	"encoding/json"
	"meshra/internal/model"
	"strconv"
	"strings"
	"time"
)

func actor(p model.Principal) string {
	if p.Admin {
		return "admin"
	}
	return p.AgentID
}
func (r *Relay) member(p model.Principal, id string, owner bool) error {
	var creator string
	if err := r.db.QueryRow("SELECT owner FROM conversations WHERE id=?", id).Scan(&creator); err != nil {
		if err == sql.ErrNoRows {
			return model.Err(404, "conversation not found")
		}
		return err
	}
	if p.Admin || creator == p.AgentID {
		return nil
	}
	if !owner {
		var n int
		if err := r.db.QueryRow("SELECT count(*) FROM conversation_participants WHERE conversation_id=? AND agent_id=?", id, p.AgentID).Scan(&n); err != nil {
			return err
		}
		if n == 1 {
			return nil
		}
	}
	return model.Err(403, "conversation access denied")
}
func (r *Relay) conversation(p model.Principal, id string) (model.Conversation, error) {
	c := model.Conversation{Participants: []string{}, ContextRefs: []model.ContextRef{}}
	err := r.db.QueryRow("SELECT id,title,owner,created_at FROM conversations WHERE id=?", id).Scan(&c.ID, &c.Title, &c.Owner, &c.CreatedAt)
	if err != nil {
		return c, err
	}
	chat, err := r.isChat(id)
	if err != nil {
		return c, err
	}
	if chat {
		c.Mode = "chat"
		settings, e := r.chatSettings(id)
		if e != nil {
			return c, e
		}
		c.ChatSettings = &settings
	}
	rows, err := r.db.Query("SELECT agent_id FROM conversation_participants WHERE conversation_id=? ORDER BY agent_id", id)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err = rows.Scan(&a); err != nil {
			return c, err
		}
		c.Participants = append(c.Participants, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return c, err
	}
	bindings, err := r.db.Query(`SELECT b.context_id,b.version FROM context_bindings b JOIN contexts c ON c.id=b.context_id
 WHERE b.conversation_id=? AND (? OR c.owner=? OR EXISTS(SELECT 1 FROM context_acl a WHERE a.context_id=c.id AND a.agent_id=?)) ORDER BY b.context_id,b.version`, id, p.Admin, p.AgentID, p.AgentID)
	if err != nil {
		return c, err
	}
	defer bindings.Close()
	for bindings.Next() {
		var ref model.ContextRef
		if err = bindings.Scan(&ref.ContextID, &ref.Version); err != nil {
			return c, err
		}
		c.ContextRefs = append(c.ContextRefs, ref)
	}
	err = bindings.Err()
	bindings.Close()
	if err != nil {
		return c, err
	}
	profiles, err := r.db.Query("SELECT profile_id FROM profile_conversations WHERE conversation_id=? ORDER BY profile_id", id)
	if err != nil {
		return c, err
	}
	for profiles.Next() {
		var profileID string
		if err = profiles.Scan(&profileID); err != nil {
			break
		}
		c.ProfileIDs = append(c.ProfileIDs, profileID)
	}
	if err == nil {
		err = profiles.Err()
	}
	profiles.Close()
	if err != nil {
		return c, err
	}
	if len(c.ProfileIDs) > 0 {
		err = r.db.QueryRow("SELECT kind FROM profile_conversation_settings WHERE conversation_id=?", id).Scan(&c.ProfileKind)
	}
	return c, err
}
func (r *Relay) Collaborate(p model.Principal, op string, q model.CollaborationRequest) (any, error) {
	if op == "task.delegate" {
		if q.ConversationID == "" {
			return nil, model.Err(400, "conversationId required for delegation")
		}
		return r.Send(p, model.SendRequest{From: q.From, To: q.To, Text: q.Instruction, Type: "request", ConversationID: q.ConversationID, ContextRefs: q.ContextRefs, DependsOn: q.DependsOn, IdempotencyKey: q.IdempotencyKey, TimeoutSeconds: q.TimeoutSeconds, ParentTaskID: q.ParentTaskID})
	}
	for _, id := range []string{q.ID, q.ContextID, q.ConversationID, q.TargetTaskID, q.AgentID} {
		if len(id) > 200 || !safeRegistryText(id) || redact(id) != id {
			return nil, model.Err(400, "invalid resource identifier")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !p.Admin {
		if _, err := r.agent(p.AgentID); err != nil {
			return nil, model.Err(403, "registered identity required")
		}
	}
	if strings.HasPrefix(op, "context.") {
		return r.contextOperation(p, op, q)
	}
	switch op {
	case "conversation.create":
		if q.Mode != "" && q.Mode != "chat" {
			return nil, model.Err(400, "invalid conversation mode")
		}
		if q.Mode == "chat" && (!p.Admin || len(q.Participants) != 1) {
			return nil, model.Err(400, "chat requires the human operator and one AI")
		}
		if strings.TrimSpace(q.Title) == "" || len(q.Title) > 512 || redact(q.Title) != q.Title {
			return nil, model.Err(400, "invalid title")
		}
		ids := map[string]bool{}
		for _, id := range q.Participants {
			ids[id] = true
		}
		if !p.Admin {
			ids[p.AgentID] = true
		}
		if len(ids) > 64 {
			return nil, model.Err(400, "participant limit exceeded")
		}
		for id := range ids {
			if _, err := r.agent(id); err != nil {
				return nil, err
			}
		}
		id := randomID()
		tx, err := r.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("INSERT INTO conversations VALUES(?,?,?,?)", id, q.Title, actor(p), now().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if q.Mode == "chat" {
			if _, err = tx.Exec("INSERT INTO chat_conversations VALUES(?)", id); err != nil {
				return nil, err
			}
		}
		for a := range ids {
			if _, err = tx.Exec("INSERT INTO conversation_participants VALUES(?,?)", id, a); err != nil {
				return nil, err
			}
		}
		if _, err = tx.Exec("INSERT INTO conversation_events(conversation_id,actor,operation,resource,time) VALUES(?,?,?,?,?)", id, actor(p), op, id, now().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return r.conversation(p, id)
	case "conversation.rename", "conversation.removeParticipant":
		if op == "conversation.removeParticipant" {
			chat, err := r.isChat(q.ID)
			if err != nil {
				return nil, err
			}
			if chat {
				return nil, model.Err(409, "start a new chat to change the AI")
			}
		}
		if err := r.member(p, q.ID, true); err != nil {
			return nil, err
		}
		tx, err := r.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if op == "conversation.rename" {
			if strings.TrimSpace(q.Title) == "" || len(q.Title) > 512 || redact(q.Title) != q.Title {
				return nil, model.Err(400, "invalid title")
			}
			_, err = tx.Exec("UPDATE conversations SET title=? WHERE id=?", q.Title, q.ID)
		} else {
			var active int
			if err = tx.QueryRow("SELECT count(*) FROM tasks WHERE json_extract(body,'$.conversationId')=? AND state IN ('queued','dispatching','running') AND (sender=? OR recipient=?)", q.ID, q.AgentID, q.AgentID).Scan(&active); err != nil {
				return nil, err
			}
			if active > 0 {
				return nil, model.Err(409, "cancel or finish active tasks before removing participant")
			}
			_, err = tx.Exec("DELETE FROM conversation_participants WHERE conversation_id=? AND agent_id=?", q.ID, q.AgentID)
		}
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec("INSERT INTO conversation_events(conversation_id,actor,operation,resource,time) VALUES(?,?,?,?,?)", q.ID, actor(p), op, q.AgentID, now().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return r.conversation(p, q.ID)
	case "conversation.addParticipant":
		chat, err := r.isChat(q.ID)
		if err != nil {
			return nil, err
		}
		if chat {
			return nil, model.Err(409, "start a new chat to change the AI")
		}
		if err := r.member(p, q.ID, true); err != nil {
			return nil, err
		}
		if _, err := r.agent(q.AgentID); err != nil {
			return nil, err
		}
		tx, err := r.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("INSERT OR IGNORE INTO conversation_participants VALUES(?,?)", q.ID, q.AgentID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("INSERT INTO conversation_events(conversation_id,actor,operation,resource,time) VALUES(?,?,?,?,?)", q.ID, actor(p), op, q.AgentID, now().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return r.conversation(p, q.ID)
	case "conversation.events", "task.list":
		id := q.ID
		if id == "" {
			id = q.ConversationID
		}
		if err := r.member(p, id, false); err != nil {
			return nil, err
		}
		if op == "task.list" {
			return r.conversationTasks(id, q.After, q.Limit)
		}
		return r.conversationEvents(id, q.After, q.Limit)
	case "conversation.get":
		if err := r.member(p, q.ID, false); err != nil {
			return nil, err
		}
		return r.conversation(p, q.ID)
	case "conversation.list":
		limit := q.Limit
		if limit == 0 {
			limit = 50
		}
		if limit < 1 || limit > 100 {
			return nil, model.Err(400, "limit must be 1..100")
		}
		rows, err := r.db.Query("SELECT id FROM conversations WHERE id>? AND (? OR owner=? OR EXISTS(SELECT 1 FROM conversation_participants p WHERE p.conversation_id=conversations.id AND p.agent_id=?)) ORDER BY id LIMIT ?", q.After, p.Admin, p.AgentID, p.AgentID, limit)
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		out := []model.Conversation{}
		for _, id := range ids {
			c, e := r.conversation(p, id)
			if e != nil {
				return nil, e
			}
			out = append(out, c)
		}
		return out, nil
	default:
		return nil, model.Err(404, "collaboration operation not found")
	}
}

func (r *Relay) conversationTasks(id, after string, limit int) (any, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, model.Err(400, "limit must be 1..100")
	}
	rows, err := r.db.Query("SELECT id,state,recipient,json_extract(body,'$.dependsOn') FROM tasks WHERE json_extract(body,'$.conversationId')=? AND id>? ORDER BY id LIMIT ?", id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var tid, state, to string
		var deps sql.NullString
		if err = rows.Scan(&tid, &state, &to, &deps); err != nil {
			return nil, err
		}
		dependencies := []string{}
		if deps.Valid {
			if err = json.Unmarshal([]byte(deps.String), &dependencies); err != nil {
				return nil, err
			}
		}
		out = append(out, map[string]any{"taskId": tid, "state": state, "to": to, "dependsOn": dependencies})
	}
	return out, rows.Err()
}
func (r *Relay) conversationEvents(id, after string, limit int) (any, error) {
	n := int64(0)
	if after != "" {
		var err error
		n, err = strconv.ParseInt(after, 10, 64)
		if err != nil || n < 0 {
			return nil, model.Err(400, "invalid cursor")
		}
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, model.Err(400, "limit must be 1..100")
	}
	rows, err := r.db.Query("SELECT seq,actor,operation,resource,time FROM conversation_events WHERE conversation_id=? AND seq>? ORDER BY seq LIMIT ?", id, n, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var seq int64
		var actor, op, resource, tm string
		if err = rows.Scan(&seq, &actor, &op, &resource, &tm); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"seq": seq, "actor": actor, "operation": op, "resource": resource, "time": tm})
	}
	return out, rows.Err()
}
