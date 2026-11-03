package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"meshra/internal/model"
)

// A proposal has no library entry or binding until an operator approves it.
type skillProposalBase struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type toolSkillProposeArgs struct {
	Name         string             `json:"name"`
	Description  string             `json:"description"`
	Instructions string             `json:"instructions"`
	BasedOn      *skillProposalBase `json:"basedOn,omitempty"`
}

type skillProposal struct {
	ID             string             `json:"id"`
	ProfileID      string             `json:"profileId"`
	TaskID         string             `json:"taskId"`
	ConversationID string             `json:"conversationId"`
	Name           string             `json:"name"`
	Description    string             `json:"description"`
	Instructions   string             `json:"instructions"`
	BasedOn        *skillProposalBase `json:"basedOn,omitempty"`
	Status         string             `json:"status"`
	CreatedAt      string             `json:"createdAt"`
	ReviewedAt     string             `json:"reviewedAt,omitempty"`
	ReviewedBy     string             `json:"reviewedBy,omitempty"`
	SkillID        string             `json:"skillId,omitempty"`
}

func validateSkillProposal(q *toolSkillProposeArgs) error {
	if err := validateReviewedSkill(q.Name, q.Description, q.Instructions); err != nil {
		return err
	}
	if strings.TrimSpace(q.Instructions) == "" {
		return model.Err(400, "skill proposal instructions are required")
	}
	if q.BasedOn != nil && (!profileIdentifier(q.BasedOn.ID) || q.BasedOn.Version < 1) {
		return model.Err(400, "basedOn requires a skill id and explicit version")
	}
	return nil
}

// skillProposeForTool must be called only from the broker mutation path.
func (r *Relay) skillProposeForTool(ctx context.Context, b *toolBroker, q toolSkillProposeArgs) (model.ToolResult, error) {
	if err := validateSkillProposal(&q); err != nil {
		return model.ToolResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	task, _, _, err := b.activeLocked()
	if err != nil {
		return model.ToolResult{}, err
	}
	if err = ctx.Err(); err != nil {
		return model.ToolResult{}, err
	}
	if q.BasedOn != nil {
		base, err := r.readPinnedSkill(b.snapshot, q.BasedOn.ID)
		if err != nil {
			return model.ToolResult{}, err
		}
		if base.Version != q.BasedOn.Version {
			return model.ToolResult{}, model.Err(403, "base version not admitted for this turn")
		}
	}
	value := skillProposal{ID: randomID(), ProfileID: b.snapshot.Profile.ID, TaskID: task.ID, ConversationID: task.ConversationID, Name: q.Name, Description: q.Description, Instructions: redactSecrets(q.Instructions), BasedOn: q.BasedOn, Status: "pending", CreatedAt: now().Format(time.RFC3339Nano)}
	body, err := json.Marshal(value)
	if err != nil {
		return model.ToolResult{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ToolResult{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO skill_proposals(id,profile_id,task_id,status,body) VALUES(?,?,?,?,?)", value.ID, value.ProfileID, value.TaskID, value.Status, string(body)); err != nil {
		return model.ToolResult{}, err
	}
	if err = insertAutomationEvent(tx, "skills.proposed", value.ID, value.ProfileID, map[string]any{"taskId": value.TaskID}); err != nil {
		return model.ToolResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.ToolResult{}, err
	}
	return toolJSON(value)
}

func (r *Relay) skillProposalOperation(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	switch operation {
	case "skills.proposals.list":
		var q struct {
			After string `json:"after"`
			Limit int    `json:"limit"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if q.Limit == 0 {
			q.Limit = 50
		}
		if q.Limit < 1 || q.Limit > 100 {
			return nil, model.Err(400, "limit must be 1..100")
		}
		rows, err := r.db.QueryContext(ctx, "SELECT body FROM skill_proposals WHERE id>? ORDER BY id LIMIT ?", q.After, q.Limit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		page := model.AutomationPage[skillProposal]{Items: []skillProposal{}}
		for rows.Next() {
			var body string
			var value skillProposal
			if err = rows.Scan(&body); err != nil {
				return nil, err
			}
			if err = json.Unmarshal([]byte(body), &value); err != nil {
				return nil, err
			}
			if len(page.Items) == q.Limit {
				page.HasMore = true
				break
			}
			page.Items = append(page.Items, value)
		}
		if page.HasMore {
			page.NextAfter = page.Items[len(page.Items)-1].ID
		}
		return page, rows.Err()
	case "skills.proposals.review":
		var q struct {
			ID       string `json:"id"`
			Decision string `json:"decision"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if !profileIdentifier(q.ID) || (q.Decision != "approve" && q.Decision != "reject") {
			return nil, model.Err(400, "invalid proposal review")
		}
		var body string
		err := r.db.QueryRowContext(ctx, "SELECT body FROM skill_proposals WHERE id=?", q.ID).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.Err(404, "skill proposal not found")
		}
		if err != nil {
			return nil, err
		}
		var value skillProposal
		if err = json.Unmarshal([]byte(body), &value); err != nil {
			return nil, err
		}
		if value.Status != "pending" {
			return nil, model.Err(409, "skill proposal already reviewed")
		}
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		value.ReviewedAt = now().Format(time.RFC3339Nano)
		value.ReviewedBy = "admin"
		value.Status = "rejected"
		if q.Decision == "approve" {
			value.Status = "approved"
			value.SkillID = randomID()
			skill := localSkillVersion{ID: value.SkillID, Version: 1, Revision: 1, Name: value.Name, Description: value.Description, Markdown: value.Instructions}
			content, err := json.Marshal(struct {
				Markdown string
				Assets   []string
			}{skill.Markdown, skill.AssetAttachmentIDs})
			if err != nil {
				return nil, err
			}
			skill.SHA256 = tokenHash(string(content))
			skillBody, err := json.Marshal(skill)
			if err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO local_skills(id,revision,name,description) VALUES(?,?,?,?)", skill.ID, 1, skill.Name, skill.Description); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO local_skill_versions(skill_id,revision,sha256,body) VALUES(?,?,?,?)", skill.ID, 1, skill.SHA256, string(skillBody)); err != nil {
				return nil, err
			}
			if err = insertAutomationEvent(tx, "skills.saved", skill.ID, "", map[string]any{"revision": 1, "proposalId": value.ID}); err != nil {
				return nil, err
			}
		}
		updated, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, "UPDATE skill_proposals SET status=?,body=? WHERE id=? AND status='pending'", value.Status, string(updated), value.ID)
		if err != nil {
			return nil, err
		}
		if n, err := result.RowsAffected(); err != nil {
			return nil, err
		} else if n != 1 {
			return nil, model.Err(409, "skill proposal already reviewed")
		}
		if err = insertAutomationEvent(tx, "skills.proposalReviewed", value.ID, value.ProfileID, map[string]any{"taskId": value.TaskID, "decision": q.Decision, "skillId": value.SkillID}); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return value, nil
	}
	return nil, model.Err(404, "unknown skill proposal operation")
}
