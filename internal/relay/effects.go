package relay

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"meshra/internal/model"
)

// effectView is one externally visible action, reported independently of the
// task that requested it. A failed continuation never hides a completed effect,
// and an unknown outcome is never presented as success or as "not executed".
//
// Status values:
//
//	intended   recorded, not yet dispatched
//	dispatched handed to the provider or tool, outcome not yet recorded
//	completed  the provider or tool acknowledged success
//	rejected   definitely not executed (refused before or by the provider)
//	cancelled  withdrawn before dispatch
//	uncertain  dispatch may have happened; never replayed automatically
type effectView struct {
	ID            string    `json:"id"`
	Source        string    `json:"source"`
	TaskID        string    `json:"taskId"`
	ProfileID     string    `json:"profileId,omitempty"`
	Operation     string    `json:"operation"`
	Status        string    `json:"status"`
	RequestDigest string    `json:"requestDigest,omitempty"`
	PolicyRev     int       `json:"policyRevision,omitempty"`
	ProviderRef   string    `json:"providerRef,omitempty"`
	Idempotency   string    `json:"idempotency"`
	DuplicateRisk bool      `json:"duplicateRisk,omitempty"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

const noProviderIdempotency = "none: the provider endpoint offers no idempotency key; a local record is not provider-side exactly-once"

func toolEffectStatus(state string) string {
	switch state {
	case "pending", "approved", "auto_approved", "waiting_question":
		return "intended"
	case "claimed":
		return "dispatched"
	case "completed":
		return "completed"
	case "uncertain":
		return "uncertain"
	case "cancelled":
		return "cancelled"
	}
	return "rejected"
}

func deliveryEffectStatus(state string) string {
	switch state {
	case "pending":
		return "intended"
	case "sending":
		return "dispatched"
	case "sent":
		return "completed"
	case "uncertain_delivery":
		return "uncertain"
	}
	return "rejected"
}

// effectsList merges mutating tool invocations and connector deliveries into
// one read model. It never changes state.
func (r *Relay) effectsList(_ context.Context, payload json.RawMessage) (any, error) {
	var q struct {
		TaskID string `json:"taskId,omitempty"`
		Status string `json:"status,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	if err := decodeAutomation(payload, &q); err != nil {
		return nil, err
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 500 {
		return nil, model.Err(400, "limit must be 1..500")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	views := []effectView{}
	rows, err := r.db.Query("SELECT state,body FROM tool_invocations WHERE (?='' OR task_id=?)", q.TaskID, q.TaskID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var column, body string
		if err = rows.Scan(&column, &body); err != nil {
			rows.Close()
			return nil, err
		}
		var inv toolInvocation
		if json.Unmarshal([]byte(body), &inv) != nil {
			continue
		}
		// The state column is authoritative: crash recovery updates it without
		// rewriting the stored body.
		status := toolEffectStatus(column)
		// Rows from before the marker existed only qualify once they may have run.
		if !inv.Mutation && status != "uncertain" {
			continue
		}
		view := effectView{ID: inv.ID, Source: "tool", TaskID: inv.TaskID, ProfileID: inv.ProfileID, Operation: inv.Name, Status: status, RequestDigest: inv.ActionHash, PolicyRev: inv.PolicyRevision, Idempotency: noProviderIdempotency, Error: inv.Error, CreatedAt: inv.CreatedAt}
		view.DuplicateRisk = status == "uncertain"
		views = append(views, view)
	}
	rows.Close()
	drows, err := r.db.Query("SELECT id,task_id,state,remote_id,body FROM connector_outbox WHERE (?='' OR task_id=?)", q.TaskID, q.TaskID)
	if err != nil {
		return nil, err
	}
	for drows.Next() {
		var id, task, state, remote, body string
		if err = drows.Scan(&id, &task, &state, &remote, &body); err != nil {
			drows.Close()
			return nil, err
		}
		var d connectorDelivery
		if json.Unmarshal([]byte(body), &d) != nil {
			continue
		}
		status := deliveryEffectStatus(state)
		views = append(views, effectView{ID: id, Source: "connector", TaskID: task, Operation: "connector.reply", Status: status, RequestDigest: tokenHash(d.ConnectorID + "\x00" + d.ThreadKey + "\x00" + d.Text), ProviderRef: remote, Idempotency: noProviderIdempotency, DuplicateRisk: status == "uncertain", Error: d.Error, CreatedAt: d.CreatedAt})
	}
	drows.Close()
	out := views[:0]
	for _, v := range views {
		if q.Status == "" || strings.EqualFold(q.Status, v.Status) {
			out = append(out, v)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return map[string]any{"effects": out}, nil
}
