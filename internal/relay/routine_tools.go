package relay

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"meshra/internal/model"
)

type toolRoutineListArgs struct {
	After string `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type toolRoutineProposeArgs struct {
	Name           string          `json:"name"`
	Instruction    string          `json:"instruction"`
	Trigger        *routineTrigger `json:"trigger,omitempty"`
	TimeoutSeconds int             `json:"timeoutSeconds,omitempty"`
}

func validateRoutineProposal(q *toolRoutineProposeArgs) error {
	if strings.TrimSpace(q.Name) == "" || len(q.Name) > routineMaxNameBytes || !safeRegistryText(q.Name) || redact(q.Name) != q.Name {
		return model.Err(400, "routine proposal name must be1..128 safe bytes")
	}
	if strings.TrimSpace(q.Instruction) == "" || len(q.Instruction) > routineMaxInstruction || !utf8.ValidString(q.Instruction) {
		return model.Err(400, "routine proposal instruction must be1..65536 UTF-8 bytes")
	}
	if q.Trigger == nil {
		q.Trigger = &routineTrigger{Kind: "manual", TimeZone: "UTC"}
	}
	if err := q.Trigger.validate(); err != nil {
		return err
	}
	if q.Trigger.Kind == "once" {
		at, err := routineTriggerInstant(*q.Trigger)
		if err != nil {
			return err
		}
		if !at.After(now()) {
			return model.Err(400, "once proposal requires a future timestamp")
		}
	}
	if q.TimeoutSeconds == 0 {
		q.TimeoutSeconds = routineDefaultTimeout
	}
	if q.TimeoutSeconds < 1 || q.TimeoutSeconds > routineMaxTimeout {
		return model.Err(400, "routine proposal timeout must be1..86400seconds")
	}
	return nil
}

// Only current-profile metadata is exposed; webhook keys, token grants and
// unrelated profiles' routine instructions never enter the model catalog.
func (r *Relay) routineListForTool(ctx context.Context, b *toolBroker, q toolRoutineListArgs) (model.ToolResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, _, err := b.activeLocked(); err != nil {
		return model.ToolResult{}, err
	}
	rows, err := r.db.QueryContext(ctx, "SELECT body FROM routines WHERE json_extract(body,'$.profileId')=? AND id>? ORDER BY id LIMIT ?", b.snapshot.Profile.ID, q.After, q.Limit+1)
	if err != nil {
		return model.ToolResult{}, err
	}
	defer rows.Close()
	type summary struct {
		ID          string         `json:"id"`
		Revision    int            `json:"revision"`
		Name        string         `json:"name"`
		Instruction string         `json:"instructionExcerpt"`
		Trigger     routineTrigger `json:"trigger"`
		Enabled     bool           `json:"enabled"`
		AutoStart   bool           `json:"autoStart"`
	}
	page := model.AutomationPage[summary]{Items: []summary{}}
	for rows.Next() {
		var body string
		var definition routineDefinition
		if err = rows.Scan(&body); err != nil {
			return model.ToolResult{}, err
		}
		if err = json.Unmarshal([]byte(body), &definition); err != nil {
			return model.ToolResult{}, err
		}
		if len(page.Items) == q.Limit {
			page.HasMore = true
			break
		}
		page.Items = append(page.Items, summary{ID: definition.ID, Revision: definition.Revision, Name: definition.Name, Instruction: boundedChatText(redact(definition.Instruction), 400), Trigger: definition.Trigger, Enabled: definition.Enabled, AutoStart: definition.AutoStart})
		page.NextAfter = definition.ID
	}
	if err = rows.Err(); err != nil {
		return model.ToolResult{}, err
	}
	return toolJSON(page)
}

func (r *Relay) routineProposeForTool(ctx context.Context, b *toolBroker, q toolRoutineProposeArgs) (model.ToolResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, _, _, err := b.activeLocked()
	if err != nil {
		return model.ToolResult{}, err
	}
	if err = ctx.Err(); err != nil {
		return model.ToolResult{}, err
	}
	definition := routineDefinition{ID: randomID(), Revision: 1, Name: q.Name, ProfileID: b.snapshot.Profile.ID, Instruction: redact(q.Instruction), Trigger: *q.Trigger, TimeoutSeconds: q.TimeoutSeconds, Enabled: false, AutoStart: false}
	if err = r.validateRoutineDefinition(&definition, true); err != nil {
		return model.ToolResult{}, err
	}
	body, err := json.Marshal(definition)
	if err != nil {
		return model.ToolResult{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ToolResult{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO routines(id,revision,body,next_due_at,enabled) VALUES(?,?,?,'',0)", definition.ID, definition.Revision, string(body)); err != nil {
		return model.ToolResult{}, err
	}
	if err = insertAutomationEvent(tx, "routines.proposed", definition.ID, definition.ProfileID, map[string]any{"taskId": task.ID, "enabled": false, "autoStart": false}); err != nil {
		return model.ToolResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.ToolResult{}, err
	}
	return toolJSON(routineViewOf(definition, "", "missing"))
}
