package relay

import (
	"encoding/json"

	"meshra/internal/model"
)

func (r *Relay) automationUsage(p model.Principal, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	var q struct {
		ProfileID      string `json:"profileId"`
		BindingID      string `json:"bindingId"`
		ConversationID string `json:"conversationId"`
		Harness        string `json:"harness"`
		Limit          int    `json:"limit"`
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
	r.mu.Lock()
	defer r.mu.Unlock()
	type row struct {
		TaskID             string   `json:"taskId"`
		Attempt            int      `json:"attempt"`
		StartedAt          string   `json:"startedAt"`
		Harness            string   `json:"harness"`
		ConfiguredModel    string   `json:"configuredModel"`
		ConfiguredProvider string   `json:"configuredProvider"`
		ProfileID          *string  `json:"profileId"`
		ProfileRevision    *int     `json:"profileRevision"`
		BindingID          *string  `json:"bindingId"`
		ReportedModel      *string  `json:"reportedModel"`
		ReportedProvider   *string  `json:"reportedProvider"`
		Input              *int64   `json:"inputTokens"`
		Output             *int64   `json:"outputTokens"`
		Cached             *int64   `json:"cachedInputTokens"`
		Cost               *float64 `json:"reportedCost"`
		Currency           *string  `json:"currency"`
		InputBytes         int64    `json:"inputBytes"`
		OutputBytes        int64    `json:"outputBytes"`
		DurationMS         int64    `json:"durationMs"`
		State              string   `json:"state"`
		Source             string   `json:"source"`
		Partial            bool     `json:"partial"`
		Stale              bool     `json:"stale"`
	}
	rows, err := r.db.Query(`SELECT u.task_id,u.attempt,u.started_at,u.harness,u.model,u.provider,u.profile_id,u.profile_revision,u.binding_id,u.reported_model,u.reported_provider,u.input_tokens,u.output_tokens,u.cached_input_tokens,u.reported_cost,u.reported_currency,u.input_bytes,u.output_bytes,u.duration_ms,u.state,EXISTS(SELECT 1 FROM profile_bindings b WHERE b.id=u.binding_id AND b.generation<(SELECT max(n.generation) FROM profile_bindings n WHERE n.profile_id=b.profile_id AND n.conversation_id=b.conversation_id)) FROM run_usage u WHERE (?='' OR u.profile_id=?) AND (?='' OR u.binding_id=?) AND (?='' OR u.conversation_id=?) AND (?='' OR u.harness=?) ORDER BY u.started_at DESC,u.task_id,u.attempt DESC LIMIT ?`, q.ProfileID, q.ProfileID, q.BindingID, q.BindingID, q.ConversationID, q.ConversationID, q.Harness, q.Harness, q.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []row{}
	more := false
	for rows.Next() {
		var v row
		if err = rows.Scan(&v.TaskID, &v.Attempt, &v.StartedAt, &v.Harness, &v.ConfiguredModel, &v.ConfiguredProvider, &v.ProfileID, &v.ProfileRevision, &v.BindingID, &v.ReportedModel, &v.ReportedProvider, &v.Input, &v.Output, &v.Cached, &v.Cost, &v.Currency, &v.InputBytes, &v.OutputBytes, &v.DurationMS, &v.State, &v.Stale); err != nil {
			return nil, err
		}
		if len(items) == q.Limit {
			more = true
			break
		}
		v.Source = "unreported"
		if v.Input != nil || v.Output != nil || v.Cached != nil || v.ReportedModel != nil || v.ReportedProvider != nil || v.Cost != nil {
			v.Source = "native"
		}
		v.Partial = v.Input == nil || v.Output == nil || v.Cached == nil
		items = append(items, v)
	}
	return map[string]any{"items": items, "hasMore": more, "retentionDays": 30}, rows.Err()
}
