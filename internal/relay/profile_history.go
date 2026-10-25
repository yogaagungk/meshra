package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"meshra/internal/model"
)

type profileDataScope struct {
	profile        model.Profile
	binding        model.ProfileBinding
	conversationID string
	before         int64
	cutoff         int64
	external       bool
}

// profileDataAccess never accepts model-selected identity or session fields.
// Caller holds r.mu; all queries finish before a mutation transaction begins.
func (r *Relay) profileDataAccess(p model.Principal, profileID, conversationID string) (profileDataScope, error) {
	var s profileDataScope
	if !p.Admin {
		if p.AgentID == "" || p.SessionID == "" || p.TaskID == "" {
			return s, model.Err(403, "active profile turn required")
		}
		t, err := r.task(p.TaskID)
		if err != nil || t.State != "running" || t.To != p.AgentID || t.DestinationSessionID != p.SessionID || !t.Deadline.After(now()) {
			return s, model.Err(403, "profile turn is no longer active")
		}
		slot := r.slots[p.AgentID]
		if slot == nil || slot.taskID != t.ID || slot.runtime == nil || slot.runtime.SessionID() != p.SessionID {
			return s, model.Err(403, "profile turn session is no longer active")
		}
		snapshot, err := r.profileSnapshot(t)
		if err != nil || snapshot.Binding.AgentID != p.AgentID || snapshot.Binding.ProfileID != snapshot.Profile.ID || snapshot.Binding.ConversationID != t.ConversationID {
			return s, model.Err(403, "profile turn binding required")
		}
		if (profileID != "" && profileID != snapshot.Profile.ID) || (conversationID != "" && conversationID != t.ConversationID) {
			return s, model.Err(403, "profile turn scope mismatch")
		}
		profileID, conversationID = snapshot.Profile.ID, t.ConversationID
		s.external = snapshot.External
		if err = r.db.QueryRow("SELECT seq FROM tasks WHERE id=?", t.ID).Scan(&s.before); err != nil {
			return s, err
		}
	}
	if !profileIdentifier(profileID) || (conversationID != "" && !profileIdentifier(conversationID)) {
		return s, model.Err(400, "valid profileId and conversationId required")
	}
	var err error
	s.profile, err = r.profile(profileID)
	if err != nil {
		return s, err
	}
	s.conversationID = conversationID
	if conversationID == "" {
		return s, nil
	}
	if err = r.member(p, conversationID, false); err != nil {
		return s, err
	}
	var associated bool
	if err = r.db.QueryRow("SELECT EXISTS(SELECT 1 FROM profile_conversations WHERE profile_id=? AND conversation_id=?)", profileID, conversationID).Scan(&associated); err != nil {
		return s, err
	}
	if !associated {
		return s, model.Err(403, "profile is not associated with conversation")
	}
	s.binding, err = r.profileBinding(profileID, conversationID)
	if err != nil {
		return s, err
	}
	if !p.Admin && s.binding.AgentID != p.AgentID {
		return s, model.Err(403, "historical binding access denied")
	}
	err = r.db.QueryRow("SELECT reset_after_seq FROM profile_context_epochs WHERE binding_id=?", s.binding.ID).Scan(&s.cutoff)
	if err == sql.ErrNoRows {
		err = nil
	}
	return s, err
}

type profileHistorySearchRequest struct {
	ProfileID      string `json:"profileId"`
	ConversationID string `json:"conversationId"`
	Query          string `json:"query"`
	After          string `json:"after,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}
type profileHistoryReadRequest struct {
	ProfileID      string `json:"profileId"`
	ConversationID string `json:"conversationId"`
	TaskID         string `json:"taskId,omitempty"`
	Before         int64  `json:"before,omitempty"`
	Offset         int    `json:"offset,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}
type profileHistoryHit struct {
	TaskID  string `json:"taskId"`
	Excerpt string `json:"excerpt"`
}
type profileHistoryExchange struct {
	TaskID string `json:"taskId"`
	Seq    int64  `json:"seq"`
	Text   string `json:"text"`
	Result string `json:"result,omitempty"`
}
type profileHistoryPage struct {
	Items      []profileHistoryExchange `json:"items"`
	NextBefore int64                    `json:"nextBefore,omitempty"`
	HasMore    bool                     `json:"hasMore"`
}

func profileHistoryLimit(n int) (int, error) {
	if n == 0 {
		return 20, nil
	}
	if n < 1 || n > 20 {
		return 0, model.Err(400, "limit must be 1..20")
	}
	return n, nil
}

func (r *Relay) profileHistoryOperation(ctx context.Context, p model.Principal, op string, raw json.RawMessage) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch op {
	case "history.search":
		var q profileHistorySearchRequest
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		limit, err := profileHistoryLimit(q.Limit)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(q.Query) == "" || len(q.Query) > 4096 || !utf8.ValidString(q.Query) || strings.ContainsRune(q.Query, 0) {
			return nil, model.Err(400, "query must be 1..4096 UTF-8 bytes")
		}
		var after int64
		if q.After != "" {
			after, err = strconv.ParseInt(q.After, 10, 64)
			if err != nil || after < 1 {
				return nil, model.Err(400, "invalid history cursor")
			}
		}
		s, err := r.profileDataAccess(p, q.ProfileID, q.ConversationID)
		if err != nil {
			return nil, err
		}
		if s.conversationID == "" {
			return nil, model.Err(400, "conversationId required")
		}
		// Quote the entire input as one FTS phrase. Operators and punctuation are
		// data, not query syntax. Ownership is checked independently of the index.
		phrase := `"` + strings.ReplaceAll(q.Query, `"`, `""`) + `"`
		rows, err := r.db.QueryContext(ctx, `SELECT t.id,t.seq,snippet(profile_history,-1,'','',' … ',64)
 FROM profile_history JOIN tasks t ON t.id=profile_history.task_id JOIN profile_task_snapshots s ON s.task_id=t.id
 WHERE profile_history MATCH ? AND profile_history.profile_id=? AND profile_history.conversation_id=? AND s.profile_id=?
 AND json_extract(t.body,'$.conversationId')=? AND t.state='completed' AND t.seq>?
 AND (?=0 OR t.seq<?) AND (?=0 OR t.seq<?) ORDER BY t.seq DESC LIMIT ?`, phrase, s.profile.ID, s.conversationID, s.profile.ID, s.conversationID, s.cutoff, s.before, s.before, after, after, limit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		page := model.AutomationPage[profileHistoryHit]{Items: []profileHistoryHit{}}
		for rows.Next() {
			var id, excerpt string
			var seq int64
			if err = rows.Scan(&id, &seq, &excerpt); err != nil {
				return nil, err
			}
			if len(page.Items) == limit {
				page.HasMore = true
				break
			}
			page.Items = append(page.Items, profileHistoryHit{TaskID: id, Excerpt: boundedChatText(redact(excerpt), 400)})
			page.NextAfter = strconv.FormatInt(seq, 10)
		}
		if !page.HasMore {
			page.NextAfter = ""
		}
		return page, rows.Err()
	case "history.read":
		var q profileHistoryReadRequest
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		limit, err := profileHistoryLimit(q.Limit)
		if err != nil {
			return nil, err
		}
		if q.Before < 0 || q.Offset < 0 || q.Offset > 1000000 || (q.TaskID != "" && !profileIdentifier(q.TaskID)) {
			return nil, model.Err(400, "invalid history range")
		}
		s, err := r.profileDataAccess(p, q.ProfileID, q.ConversationID)
		if err != nil {
			return nil, err
		}
		if s.conversationID == "" {
			return nil, model.Err(400, "conversationId required")
		}
		rows, err := r.db.QueryContext(ctx, `SELECT t.id,t.seq,coalesce(json_extract(t.body,'$.text'),''),coalesce(json_extract(t.body,'$.result'),'')
 FROM tasks t JOIN profile_task_snapshots s ON s.task_id=t.id WHERE s.profile_id=? AND json_extract(t.body,'$.conversationId')=?
 AND t.state='completed' AND t.seq>? AND (?=0 OR t.seq<?) AND (?=0 OR t.seq<?) AND (?='' OR t.id=?)
 ORDER BY t.seq DESC LIMIT ? OFFSET ?`, s.profile.ID, s.conversationID, s.cutoff, s.before, s.before, q.Before, q.Before, q.TaskID, q.TaskID, limit+1, q.Offset)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		page := profileHistoryPage{Items: []profileHistoryExchange{}}
		used := 128 // reserve page keys and the numeric continuation cursor
		for rows.Next() {
			var item profileHistoryExchange
			if err = rows.Scan(&item.TaskID, &item.Seq, &item.Text, &item.Result); err != nil {
				return nil, err
			}
			if len(page.Items) == limit {
				page.HasMore = true
				break
			}
			// The cap includes JSON escaping. Keep complete exchanges together;
			// exceptionally large first exchanges get bounded UTF-8 excerpts.
			item.Text = boundedChatText(redact(item.Text), 16*1024)
			item.Result = boundedChatText(redact(item.Result), 16*1024)
			body, err := json.Marshal(item)
			if err != nil {
				return nil, err
			}
			if len(page.Items) == 0 {
				for used+len(body)+1 > 16*1024 {
					item.Text = boundedChatText(item.Text, len(item.Text)/2)
					item.Result = boundedChatText(item.Result, len(item.Result)/2)
					body, err = json.Marshal(item)
					if err != nil {
						return nil, err
					}
				}
			}
			if used+len(body)+1 > 16*1024 {
				page.HasMore = true
				break
			}
			used += len(body) + 1
			page.Items = append(page.Items, item)
			page.NextBefore = item.Seq
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		if q.TaskID != "" && len(page.Items) == 0 {
			return nil, model.Err(404, "history exchange unavailable")
		}
		if !page.HasMore {
			page.NextBefore = 0
		}
		return page, nil
	default:
		return nil, model.Err(404, "unknown history operation")
	}
}
