package relay

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
)

type profileSessionSearchRequest struct {
	ProfileID string `json:"profileId,omitempty"`
	Query     string `json:"query"`
	After     string `json:"after,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type profileSessionHit struct {
	ConversationID string `json:"conversationId"`
	TaskID         string `json:"taskId"`
	Excerpt        string `json:"excerpt"`
	Timestamp      string `json:"timestamp"`
}

// profileSessionSearch runs under r.mu, after the active turn has been checked.
// Identity and conversation authorization come from stored bindings, never the query.
func (r *Relay) profileSessionSearch(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	var q profileSessionSearchRequest
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
	s, err := r.profileDataAccess(p, q.ProfileID, "")
	if err != nil {
		return nil, err
	}
	retention := r.options.Retention
	if retention == 0 {
		retention = 30 * 24 * time.Hour
	}
	cutoff := ""
	if retention > 0 {
		cutoff = now().Add(-retention).UTC().Format(time.RFC3339Nano)
	}
	// External turns may only recall their own conversation; otherwise the model
	// could relay private transcript content to an external audience.
	phrase := `"` + strings.ReplaceAll(q.Query, `"`, `""`) + `"`
	rows, err := r.db.QueryContext(ctx, `SELECT c.id,t.id,t.seq,snippet(profile_history,-1,'','',' … ',64),coalesce(json_extract(t.body,'$.updatedAt'),'')
 FROM profile_history
 JOIN tasks t ON t.id=profile_history.task_id
 JOIN profile_task_snapshots s ON s.task_id=t.id AND s.profile_id=profile_history.profile_id
 JOIN conversations c ON c.id=profile_history.conversation_id AND c.id=json_extract(t.body,'$.conversationId')
 JOIN profile_conversations pc ON pc.conversation_id=c.id AND pc.profile_id=s.profile_id
 JOIN profile_bindings b ON b.id=(SELECT id FROM profile_bindings WHERE profile_id=s.profile_id AND conversation_id=c.id ORDER BY generation DESC LIMIT 1)
 LEFT JOIN profile_context_epochs e ON e.binding_id=b.id
 WHERE profile_history MATCH ? AND s.profile_id=? AND t.state='completed'
 AND (? OR c.owner=b.agent_id OR EXISTS(SELECT 1 FROM conversation_participants cp WHERE cp.conversation_id=c.id AND cp.agent_id=b.agent_id))
 AND (?=0 OR c.id=?)
 AND t.seq>coalesce(e.reset_after_seq,0) AND (?=0 OR t.seq<?) AND (?=0 OR t.seq<?)
 AND (?='' OR julianday(json_extract(t.body,'$.updatedAt'))>=julianday(?))
 ORDER BY t.seq DESC LIMIT ?`, phrase, s.profile.ID, p.Admin, s.external, s.conversationID, s.before, s.before, after, after, cutoff, cutoff, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := model.AutomationPage[profileSessionHit]{Items: []profileSessionHit{}}
	for rows.Next() {
		var hit profileSessionHit
		var seq int64
		if err = rows.Scan(&hit.ConversationID, &hit.TaskID, &seq, &hit.Excerpt, &hit.Timestamp); err != nil {
			return nil, err
		}
		if len(page.Items) == limit {
			page.HasMore = true
			break
		}
		hit.Excerpt = boundedChatText(redact(hit.Excerpt), 400)
		page.Items = append(page.Items, hit)
		page.NextAfter = strconv.FormatInt(seq, 10)
	}
	if !page.HasMore {
		page.NextAfter = ""
	}
	return page, rows.Err()
}
