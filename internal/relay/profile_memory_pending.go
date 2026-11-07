package relay

import (
	"strings"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"meshra/internal/model"
)

// Memory is deliberately bounded independently of the prompt injection limit.
const profileMemoryBudget = 256 * 1024

type pendingProfileMemory struct {
	ID               string `json:"id"`
	ProfileID        string `json:"profileId"`
	ConversationID   string `json:"conversationId"`
	MemoryID         string `json:"memoryId,omitempty"`
	ExpectedRevision int    `json:"expectedRevision,omitempty"`
	Content          string `json:"content"`
	Pinned           bool   `json:"pinned"`
	SourceTaskID     string `json:"sourceTaskId,omitempty"`
	CreatedAt        string `json:"createdAt"`
}

// normalizeMemory makes duplicate detection insensitive to case and whitespace.
func normalizeMemory(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// checkProfileMemoryBounds applies identical duplicate and size rules whether
// review is on or off and whether the writer is an operator or a model. Existing
// over-limit data is never discarded: an edit that does not grow the memory is
// always allowed so it can be corrected.
func (r *Relay) checkProfileMemoryBounds(m profileMemory, reviewing string) error {
	want := normalizeMemory(m.Content)
	rows, err := r.db.Query(`SELECT content FROM profile_memories WHERE profile_id=? AND scope=? AND coalesce(conversation_id,'')=? AND id<>?
 UNION ALL SELECT content FROM profile_memory_pending WHERE profile_id=? AND conversation_id=? AND id<>?`, m.ProfileID, m.Scope, m.ConversationID, m.ID, m.ProfileID, m.ConversationID, reviewing)
	if err != nil {
		return err
	}
	for rows.Next() {
		var existing string
		if err = rows.Scan(&existing); err != nil {
			rows.Close()
			return err
		}
		if normalizeMemory(existing) == want {
			rows.Close()
			return model.Err(409, "duplicate memory; consolidate existing memories instead")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var total, previous int
	if err = r.db.QueryRow(`SELECT coalesce(sum(length(CAST(content AS BLOB))),0) FROM profile_memories WHERE profile_id=? AND id<>?`, m.ProfileID, m.ID).Scan(&total); err != nil {
		return err
	}
	if m.ID != "" {
		_ = r.db.QueryRow(`SELECT length(CAST(content AS BLOB)) FROM profile_memories WHERE profile_id=? AND id=?`, m.ProfileID, m.ID).Scan(&previous)
	}
	if total+len(m.Content) > profileMemoryBudget && !(previous > 0 && len(m.Content) <= previous) {
		return model.Err(429, "profile memory byte budget reached; consolidate existing memories")
	}
	return nil
}

func (r *Relay) stageProfileMemory(ctx context.Context, m profileMemory, memoryID string, revision int) (any, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM profile_memory_pending WHERE profile_id=?`, m.ProfileID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 256 {
		return nil, model.Err(429, "pending memory capacity reached; review existing proposals")
	}
	pending := pendingProfileMemory{ID: randomID(), ProfileID: m.ProfileID, ConversationID: m.ConversationID, MemoryID: memoryID, ExpectedRevision: revision, Content: m.Content, Pinned: m.Pinned, SourceTaskID: m.SourceTaskID, CreatedAt: now().Format(time.RFC3339Nano)}
	_, err := r.db.ExecContext(ctx, `INSERT INTO profile_memory_pending(id,profile_id,conversation_id,memory_id,expected_revision,content,pinned,source_task_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, pending.ID, pending.ProfileID, pending.ConversationID, pending.MemoryID, pending.ExpectedRevision, pending.Content, pending.Pinned, pending.SourceTaskID, pending.CreatedAt)
	return pending, err
}

func (r *Relay) profileMemoryPendingOperation(ctx context.Context, p model.Principal, op string, raw json.RawMessage) (any, error) {
	if !p.Admin {
		return nil, model.Err(403, "memory review requires operator authority")
	}
	if op == "memory.pending.list" {
		var q struct {
			ProfileID string `json:"profileId"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if _, err := r.profileDataAccess(p, q.ProfileID, ""); err != nil {
			return nil, err
		}
		rows, err := r.db.QueryContext(ctx, `SELECT id,profile_id,conversation_id,memory_id,expected_revision,content,pinned,source_task_id,created_at FROM profile_memory_pending WHERE profile_id=? ORDER BY created_at DESC,id`, q.ProfileID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		items := []pendingProfileMemory{}
		for rows.Next() {
			var item pendingProfileMemory
			if err = rows.Scan(&item.ID, &item.ProfileID, &item.ConversationID, &item.MemoryID, &item.ExpectedRevision, &item.Content, &item.Pinned, &item.SourceTaskID, &item.CreatedAt); err != nil {
				return nil, err
			}
			item.Content = redact(item.Content)
			items = append(items, item)
		}
		return struct {
			Items []pendingProfileMemory `json:"items"`
		}{items}, rows.Err()
	}
	var q struct {
		ProfileID string  `json:"profileId"`
		ID        string  `json:"id"`
		Decision  string  `json:"decision"`
		Content   *string `json:"content,omitempty"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) || (q.Decision != "approve" && q.Decision != "reject") {
		return nil, model.Err(400, "id and approve or reject decision required")
	}
	if _, err := r.profileDataAccess(p, q.ProfileID, ""); err != nil {
		return nil, err
	}
	var item pendingProfileMemory
	err := r.db.QueryRowContext(ctx, `SELECT id,profile_id,conversation_id,memory_id,expected_revision,content,pinned,source_task_id,created_at FROM profile_memory_pending WHERE profile_id=? AND id=?`, q.ProfileID, q.ID).Scan(&item.ID, &item.ProfileID, &item.ConversationID, &item.MemoryID, &item.ExpectedRevision, &item.Content, &item.Pinned, &item.SourceTaskID, &item.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, model.Err(404, "pending memory not found")
	}
	if err != nil {
		return nil, err
	}
	if q.Decision == "reject" {
		_, err = r.db.ExecContext(ctx, `DELETE FROM profile_memory_pending WHERE profile_id=? AND id=?`, q.ProfileID, q.ID)
		return map[string]bool{"ok": err == nil}, err
	}
	if q.Content != nil {
		item.Content = *q.Content
	}
	save, err := json.Marshal(profileMemorySaveRequest{ProfileID: item.ProfileID, ID: item.MemoryID, ExpectedRevision: item.ExpectedRevision, Content: item.Content, Pinned: item.Pinned, Scope: "conversation", ConversationID: item.ConversationID, SourceTaskID: item.SourceTaskID})
	if err != nil {
		return nil, err
	}
	return r.profileMemoryOperationLocked(ctx, p, "memory.save", save, item.ID)
}
