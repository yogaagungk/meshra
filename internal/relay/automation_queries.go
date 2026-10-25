package relay

import (
	"context"
	"encoding/json"

	"meshra/internal/model"
)

// Read-only metadata used to reconstruct console controls after a reload.
// Neither operation materializes a binding or broadens model tool authority.
func (r *Relay) automationBindingQuery(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (any, error) {
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
	if operation == "profiles.binding" {
		var q struct {
			ProfileID      string `json:"profileId"`
			ConversationID string `json:"conversationId"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if !profileIdentifier(q.ProfileID) || !profileIdentifier(q.ConversationID) {
			return nil, model.Err(400, "profileId and conversationId are required")
		}
		if _, err := r.profile(q.ProfileID); err != nil {
			return nil, err
		}
		return r.profileBinding(q.ProfileID, q.ConversationID)
	}
	var q struct {
		ProfileID string `json:"profileId"`
		After     string `json:"after"`
		Limit     int    `json:"limit"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ProfileID) {
		return nil, model.Err(400, "profileId is required")
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.Limit < 1 || q.Limit > 100 {
		return nil, model.Err(400, "limit must be 1..100")
	}
	if _, err := r.profile(q.ProfileID); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, "SELECT profile_id,skill_id,version,enabled,revision FROM profile_skill_bindings WHERE profile_id=? AND skill_id>? ORDER BY skill_id LIMIT ?", q.ProfileID, q.After, q.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := model.AutomationPage[profileSkillBinding]{Items: []profileSkillBinding{}}
	for rows.Next() {
		var value profileSkillBinding
		if err = rows.Scan(&value.ProfileID, &value.SkillID, &value.Version, &value.Enabled, &value.Revision); err != nil {
			return nil, err
		}
		if len(page.Items) == q.Limit {
			page.HasMore = true
			break
		}
		page.Items = append(page.Items, value)
	}
	return page, rows.Err()
}
