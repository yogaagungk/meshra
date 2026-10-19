package relay

import (
	"database/sql"
	"encoding/json"
	"meshra/internal/model"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (r *Relay) contextAccess(p model.Principal, op, id string, version int, write bool) error {
	if len(id) > 200 || !safeRegistryText(id) || redact(id) != id {
		return model.Err(400, "invalid context identifier")
	}

	var owner string
	err := r.db.QueryRow("SELECT owner FROM contexts WHERE id=?", id).Scan(&owner)
	allowed := err == nil && (p.Admin || owner == p.AgentID)
	if err == nil && !allowed && !write {
		var n int
		err = r.db.QueryRow("SELECT count(*) FROM context_acl WHERE context_id=? AND agent_id=?", id, p.AgentID).Scan(&n)
		allowed = err == nil && n == 1
	}
	outcome := "denied"
	if allowed {
		outcome = "allowed"
	}
	if _, e := r.db.Exec("INSERT INTO context_access_log(caller,operation,context_id,version,time,outcome) VALUES(?,?,?,?,?,?)", actor(p), op, id, version, now().Format(time.RFC3339Nano), outcome); e != nil {
		return e
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if !allowed {
		return model.Err(403, "context access denied")
	}
	return nil
}
func (r *Relay) getContext(p model.Principal, id string, version int, content bool) (model.SharedContext, error) {
	c := model.SharedContext{}
	if version < 1 {
		return c, model.Err(400, "exact positive context version required")
	}
	if err := r.contextAccess(p, "context.get", id, version, false); err != nil {
		return c, err
	}
	var bytes []byte
	err := r.db.QueryRow(`SELECT c.id,c.owner,c.title,c.type,c.scope,v.version,v.digest,a.size,a.content
 FROM contexts c JOIN context_versions v ON v.context_id=c.id JOIN artifacts a ON a.digest=v.digest
 WHERE c.id=? AND v.version=?`, id, version).Scan(&c.ID, &c.Owner, &c.Title, &c.Type, &c.Scope, &c.Version, &c.SHA256, &c.Size, &bytes)
	if err == sql.ErrNoRows {
		return c, model.Err(404, "context version not found")
	}
	if err != nil {
		return c, err
	}
	if len(bytes) != c.Size || tokenHash(string(bytes)) != c.SHA256 {
		return model.SharedContext{}, model.Err(500, "artifact integrity failure")
	}
	if content {
		s := string(bytes)
		c.Content = &s
	}
	return c, nil
}
func (r *Relay) contextOperation(p model.Principal, op string, q model.CollaborationRequest) (any, error) {
	switch op {
	case "context.publish":
		if q.ContextID == "" && q.Version != 0 && q.Version != 1 {
			return nil, model.Err(400, "new contexts start at version 1")
		}

		if len(q.Content) > r.options.MaxArtifactBytes {
			return nil, model.Err(413, "context exceeds configured artifact byte limit")
		}
		if q.Content == "" || !utf8.ValidString(q.Content) || redact(q.Content) != q.Content || strings.Contains(q.Content, "-----BEGIN ") {
			return nil, model.Err(400, "context must be nonempty UTF-8 without detected credentials or private keys")
		}
		if len(q.Title) > 512 || len(q.Type) > 100 || len(q.Scope) > 200 || redact(q.Title+q.Type+q.Scope) != q.Title+q.Type+q.Scope {
			return nil, model.Err(400, "invalid context metadata")
		}
		if len(q.IdempotencyKey) > 200 || redact(q.IdempotencyKey) != q.IdempotencyKey {
			return nil, model.Err(400, "invalid idempotency key")
		}
		b, _ := json.Marshal(q)
		hash := tokenHash(string(b))
		id := q.ContextID
		version := 1
		if id != "" {
			if q.IdempotencyKey != "" {
				return nil, model.Err(400, "version updates use explicit version, not idempotency key")
			}
			if err := r.contextAccess(p, op, id, q.Version, true); err != nil {
				return nil, err
			}
			if err := r.db.QueryRow("SELECT max(version)+1 FROM context_versions WHERE context_id=?", id).Scan(&version); err != nil {
				return nil, err
			}
			if q.Version != version {
				return nil, model.Err(409, "version must be next immutable version")
			}
		} else {
			if q.IdempotencyKey != "" {
				var old, oldhash string
				err := r.db.QueryRow("SELECT id,request_hash FROM contexts WHERE owner=? AND idempotency_key=?", actor(p), q.IdempotencyKey).Scan(&old, &oldhash)
				if err == nil {
					if oldhash != hash {
						return nil, model.Err(409, "idempotency key conflict")
					}
					if err := r.contextAccess(p, op, old, 1, true); err != nil {
						return nil, err
					}
					return r.getContext(p, old, 1, false)
				}
				if err != sql.ErrNoRows {
					return nil, err
				}
			}
			id = randomID()
		}
		digest := tokenHash(q.Content)
		tx, err := r.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if q.ContextID == "" {
			var key any
			if q.IdempotencyKey != "" {
				key = q.IdempotencyKey
			}
			if _, err = tx.Exec("INSERT INTO contexts VALUES(?,?,?,?,?,?,?)", id, actor(p), q.Title, q.Type, q.Scope, key, hash); err != nil {
				return nil, err
			}
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO artifacts VALUES(?,?,?)", digest, []byte(q.Content), len(q.Content)); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("INSERT INTO context_versions VALUES(?,?,?,?)", id, version, digest, now().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		if err = r.contextAccess(p, op, id, version, true); err != nil {
			return nil, err
		}
		return r.getContext(p, id, version, false)
	case "context.get":
		return r.getContext(p, q.ContextID, q.Version, true)
	case "context.grant", "context.revoke":
		if err := r.contextAccess(p, op, q.ContextID, 0, true); err != nil {
			return nil, err
		}
		if _, err := r.agent(q.AgentID); err != nil {
			return nil, err
		}
		query := "INSERT OR IGNORE INTO context_acl VALUES(?,?)"
		if op == "context.revoke" {
			query = "DELETE FROM context_acl WHERE context_id=? AND agent_id=?"
		}
		_, err := r.db.Exec(query, q.ContextID, q.AgentID)
		return map[string]bool{"updated": err == nil}, err
	case "context.audit":
		if err := r.contextAccess(p, op, q.ContextID, 0, true); err != nil {
			return nil, err
		}
		n := int64(0)
		if q.After != "" {
			var err error
			n, err = strconv.ParseInt(q.After, 10, 64)
			if err != nil || n < 0 {
				return nil, model.Err(400, "invalid cursor")
			}
		}
		limit := q.Limit
		if limit == 0 {
			limit = 50
		}
		if limit < 1 || limit > 100 {
			return nil, model.Err(400, "limit must be 1..100")
		}
		rows, err := r.db.Query("SELECT seq,caller,operation,version,time,outcome FROM context_access_log WHERE context_id=? AND seq>? ORDER BY seq LIMIT ?", q.ContextID, n, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var seq int64
			var version int
			var caller, operation, tm, outcome string
			if err = rows.Scan(&seq, &caller, &operation, &version, &tm, &outcome); err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"seq": seq, "caller": caller, "operation": operation, "version": version, "time": tm, "outcome": outcome})
		}
		return out, rows.Err()
	case "context.attach":
		if q.TargetTaskID != "" {
			if q.ConversationID != "" {
				return nil, model.Err(400, "exactly one binding target required")
			}
			t, err := r.task(q.TargetTaskID)
			if err != nil {
				return nil, err
			}
			if err = r.access(p, t, "cancel"); err != nil {
				return nil, err
			}
			if t.State != "queued" || t.ConversationID == "" {
				return nil, model.Err(409, "only queued collaboration tasks accept bindings")
			}
			refs := append([]model.ContextRef{}, t.ContextRefs...)
			refs = append(refs, model.ContextRef{ContextID: q.ContextID, Version: q.Version})
			if err = r.validateCollaboration(p, model.SendRequest{From: t.From, To: t.To, ConversationID: t.ConversationID, ContextRefs: refs, DependsOn: t.DependsOn}); err != nil {
				return nil, err
			}
			if err = r.contextAccess(p, op, q.ContextID, q.Version, false); err != nil {
				return nil, err
			}
			t.ContextRefs = refs
			if _, err = r.resolveTaskContext(t); err != nil {
				return nil, err
			}
			err = r.transition(t, "context.attached", "immutable context reference attached by caller")
			return t, err
		}
		if err := r.member(p, q.ConversationID, true); err != nil {
			return nil, err
		}
		if _, err := r.getContext(p, q.ContextID, q.Version, false); err != nil {
			return nil, err
		}
		if err := r.contextAccess(p, op, q.ContextID, q.Version, false); err != nil {
			return nil, err
		}
		_, err := r.db.Exec("INSERT OR IGNORE INTO context_bindings VALUES(?,?,?)", q.ConversationID, q.ContextID, q.Version)
		return map[string]bool{"attached": err == nil}, err
	case "context.search":
		limit := q.Limit
		if limit == 0 {
			limit = 50
		}
		if limit < 1 || limit > 100 {
			return nil, model.Err(400, "limit must be 1..100")
		}
		rows, err := r.db.Query(`SELECT c.id,max(v.version) FROM contexts c JOIN context_versions v ON v.context_id=c.id
 WHERE c.id>? AND (? OR c.owner=? OR EXISTS(SELECT 1 FROM context_acl a WHERE a.context_id=c.id AND a.agent_id=?))
 AND (?='' OR c.scope=?) AND (?='' OR c.type=?) AND instr(c.title,?)>0 GROUP BY c.id ORDER BY c.id LIMIT ?`, q.After, p.Admin, p.AgentID, p.AgentID, q.Scope, q.Scope, q.Type, q.Type, q.Keyword, limit)
		if err != nil {
			return nil, err
		}
		refs := []model.ContextRef{}
		for rows.Next() {
			var ref model.ContextRef
			if err = rows.Scan(&ref.ContextID, &ref.Version); err != nil {
				rows.Close()
				return nil, err
			}
			refs = append(refs, ref)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		out := []model.SharedContext{}
		for _, ref := range refs {
			c, e := r.getContext(p, ref.ContextID, ref.Version, false)
			if e != nil {
				return nil, e
			}
			out = append(out, c)
		}
		return out, nil
	default:
		return nil, model.Err(404, "context operation not found")
	}
}
