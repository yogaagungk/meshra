package relay

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"meshra/internal/model"
	"strings"
)

func (r *Relay) Console(p model.Principal, op string, q model.ConsoleRequest) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	if op == "quota" {
		return r.quota(q)
	}
	if op == "models" {
		return r.discoverModels(q.Harness)
	}
	if op == "workspaces" || op == "workspaces/add" || op == "workspaces/remove" {
		return r.workspaceOperation(op, q)
	}
	if strings.HasPrefix(op, "workspace/") {
		return r.inspectWorkspace(op, q)
	}
	if op == "chat-create" {
		return r.createHarnessChat(p, q)
	}
	if op == "directories" {
		return browseDirectories(q)
	}
	if op == "directories/create" {
		return createDirectory(q)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	limit := q.Limit
	if limit == 0 {
		limit = 40
	}
	if limit < 1 || limit > 100 || q.Before < 0 || q.After < 0 {
		return nil, model.Err(400, "invalid page bounds")
	}
	if op == "preferences" || op == "preferences/save" || op == "resources" || op == "usage" {
		return r.personalLocked(op, q)
	}
	if op == "ai-status" {
		return r.aiStatus(q)
	}
	if op == "usage-history" {
		return r.usageHistory(q)
	}
	switch op {
	case "harnesses":
		out := []map[string]string{}
		for _, h := range []string{"codex", "claude", "opencode", "omp", "openai", "acp"} {
			if adapter, ok := r.adapters[h]; ok && adapter.Factory != nil {
				out = append(out, map[string]string{"id": h})
			}
		}
		return out, nil
	case "chat-state":
		if err := r.member(p, q.ConversationID, false); err != nil {
			return nil, err
		}
		var id, state string
		var attempts int
		err := r.db.QueryRow(`SELECT id,state,coalesce(json_extract(body,'$.attempts'),0) FROM tasks WHERE json_extract(body,'$.conversationId')=? ORDER BY seq DESC LIMIT 1`, q.ConversationID).Scan(&id, &state, &attempts)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		return map[string]any{"taskId": id, "state": state, "canRegenerate": state == "completed" && attempts < 3}, nil
	case "regenerate":
		return r.regenerateChat(q.TaskID)
	case "context-check":
		if len(q.Recipients) > 16 || len(q.ContextRefs) > 16 {
			return nil, model.Err(400, "too many recipients or references")
		}
		denied := []string{}
		for _, id := range q.Recipients {
			if _, err := r.agent(id); err != nil {
				return nil, err
			}
			for _, ref := range q.ContextRefs {
				if _, err := r.getContext(model.Principal{AgentID: id}, ref.ContextID, ref.Version, false); err != nil {
					denied = append(denied, id)
					break
				}
			}
		}
		return map[string]any{"denied": denied}, nil
	case "send":
		if err := r.member(p, q.ConversationID, false); err != nil {
			return nil, err
		}
		if len(q.Recipients) < 1 || len(q.Recipients) > 16 || len(q.IdempotencyKey) < 8 || len(q.IdempotencyKey) > 160 {
			return nil, model.Err(400, "select 1..16 recipients and provide an idempotency key")
		}
		if strings.TrimSpace(q.Text) == "" {
			return nil, model.Err(400, "message cannot be empty")
		}
		seen := map[string]bool{}
		for _, id := range q.Recipients {
			if seen[id] {
				return nil, model.Err(400, "duplicate recipient")
			}
			seen[id] = true
		}
		tasks := []model.Task{}
		failures := map[string]string{}
		for _, id := range q.Recipients {
			task, err := r.admitLocked(p, model.SendRequest{From: "human:operator", To: id, Text: q.Text, Type: "request", ConversationID: q.ConversationID, ContextRefs: q.ContextRefs, IdempotencyKey: tokenHash(q.ConversationID + "\x00" + q.IdempotencyKey + "\x00" + id)}, nil)
			if err != nil {
				if e, ok := err.(*model.Error); ok {
					failures[id] = e.Message
				} else {
					failures[id] = "admission storage failure; retry with the same message key"
				}
				continue
			}
			tasks = append(tasks, task)
		}
		return map[string]any{"tasks": tasks, "errors": failures}, nil
	case "timeline", "tasks":
		if op == "timeline" {
			if err := r.member(p, q.ConversationID, false); err != nil {
				return nil, err
			}
		}
		rows, err := r.db.Query(`SELECT seq,body FROM tasks WHERE (?='' OR json_extract(body,'$.conversationId')=?) AND (?=0 OR seq<?) AND (?='' OR state=?) AND (instr(body,?)>0) ORDER BY seq DESC LIMIT ?`, q.ConversationID, q.ConversationID, q.Before, q.Before, q.Status, q.Status, q.Keyword, limit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		page := model.ConsolePage{Items: []model.ConsoleTask{}}
		bytes := 0
		for rows.Next() {
			var item model.ConsoleTask
			var body string
			if err = rows.Scan(&item.Seq, &body); err != nil {
				return nil, err
			}
			if err = json.Unmarshal([]byte(body), &item.Task); err != nil {
				return nil, err
			}
			if len(page.Items) > 0 && bytes+len(body) > 1024*1024 {
				page.HasMore = true
				break
			}
			bytes += len(body)
			page.Items = append(page.Items, item)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		if len(page.Items) > limit {
			page.HasMore = true
			page.Items = page.Items[:limit]
		}
		if len(page.Items) > 0 {
			page.NextBefore = page.Items[len(page.Items)-1].Seq
		}
		return page, nil
	case "watermark":
		var seq int64
		err := r.db.QueryRow("SELECT coalesce(max(id),0) FROM events").Scan(&seq)
		return seq, err
	case "automation-events":
		// Feature events carry resource-change metadata only: no task text, no
		// tool arguments and no secret material.
		after := q.After
		rows, err := r.db.Query(`SELECT seq,kind,resource_id,profile_id,data,time FROM automation_events WHERE seq>? ORDER BY seq LIMIT 100`, after)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var seq int64
			var kind, resource, profile, data, tm string
			if err = rows.Scan(&seq, &kind, &resource, &profile, &data, &tm); err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"id": seq, "type": kind, "resourceId": resource, "profileId": profile, "data": data, "time": tm})
		}
		return out, rows.Err()
	case "feed":
		if err := r.member(p, q.ConversationID, false); err != nil {
			return nil, err
		}
		rows, err := r.db.Query(`SELECT e.id,e.task_id,e.kind,e.time FROM events e JOIN tasks t ON t.id=e.task_id WHERE json_extract(t.body,'$.conversationId')=? AND e.id>? ORDER BY e.id LIMIT 256`, q.ConversationID, q.After)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var seq int64
			var task, kind, tm string
			if err = rows.Scan(&seq, &task, &kind, &tm); err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"id": seq, "taskId": task, "type": kind, "time": tm})
		}
		return out, rows.Err()
	case "conversations":
		rows, err := r.db.Query(`SELECT c.id,COALESCE((SELECT max(seq) FROM tasks WHERE json_extract(body,'$.conversationId')=c.id),0) recent FROM conversations c WHERE instr(lower(c.title),lower(?))>0 ORDER BY recent DESC,c.created_at DESC,c.id LIMIT ?`, q.Keyword, limit)
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			var seq int64
			if err = rows.Scan(&id, &seq); err != nil {
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
		out := []map[string]any{}
		for _, id := range ids {
			c, err := r.conversation(p, id)
			if err != nil {
				return nil, err
			}
			preview := ""
			updated := c.CreatedAt
			state := ""
			var body string
			err = r.db.QueryRow("SELECT body FROM tasks WHERE json_extract(body,'$.conversationId')=? ORDER BY seq DESC LIMIT 1", id).Scan(&body)
			if err == nil {
				var t model.Task
				if err = json.Unmarshal([]byte(body), &t); err != nil {
					return nil, err
				}
				preview = truncate(t.Text, 120)
				updated = t.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
				state = t.State
			} else if err != sql.ErrNoRows {
				return nil, err
			}
			out = append(out, map[string]any{"conversation": c, "preview": preview, "updatedAt": updated, "state": state})
		}
		return out, nil
	default:
		return nil, model.Err(404, fmt.Sprintf("unknown console operation %s", op))
	}
}
