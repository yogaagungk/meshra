package relay

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

func TestRoutineToolsCreateOnlyScopedDisabledDrafts(t *testing.T) {
	script := newBrokerScript(
		brokerStep{call: model.ToolCall{ID: "propose", Name: "meshra_routine_propose", Arguments: json.RawMessage(`{"name":"Review webhook draft","instruction":"Run only after human review","trigger":{"kind":"webhook"}}`)}},
		brokerStep{call: model.ToolCall{ID: "list", Name: "meshra_routine_list", Arguments: json.RawMessage(`{}`)}},
		brokerStep{call: model.ToolCall{ID: "widen", Name: "meshra_routine_propose", Arguments: json.RawMessage(`{"name":"Rejected widening","instruction":"Do not enable me","enabled":true}`)}},
	)
	r, _ := continuityOpenTest(t, script.run)
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	foreign := brokerProfile(t, r, model.ProfileCapabilities{})
	foreignRoutine := routineSave(t, r, map[string]any{"name": "FOREIGN-ROUTINE-CANARY", "profileId": foreign.ID, "instruction": "private foreign routine instructions", "trigger": map[string]any{"kind": "manual"}, "enabled": false})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, admin, profile, conversation, "Propose a draft, never enable it")
	proposed := script.await(t, task.ID, 0, 5*time.Second)
	if proposed.err != nil {
		t.Fatal(proposed.err)
	}
	var draft routineView
	if err := json.Unmarshal([]byte(brokerText(proposed.result)), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.ProfileID != profile.ID || draft.Enabled || draft.AutoStart || draft.WebhookSecret != "" {
		t.Fatalf("model proposal carried execution or credential authority: %+v", draft)
	}
	listed := script.await(t, task.ID, 1, 5*time.Second)
	if listed.err != nil {
		t.Fatal(listed.err)
	}
	text := brokerText(listed.result)
	if strings.Contains(text, foreignRoutine.ID) || strings.Contains(text, foreignRoutine.Name) || strings.Contains(text, foreign.ID) {
		t.Fatal("routine list exposed a foreign profile")
	}
	widened := script.await(t, task.ID, 2, 5*time.Second)
	requireStatus(t, widened.err, 400)
	if completed := awaitTask(t, r, task.ID); completed.State != "completed" {
		t.Fatal(completed.Error)
	}
	r.mu.Lock()
	var enabled bool
	var next, body string
	if err := r.db.QueryRow("SELECT enabled,next_due_at,body FROM routines WHERE id=?", draft.ID).Scan(&enabled, &next, &body); err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	var stored routineDefinition
	if err := json.Unmarshal([]byte(body), &stored); err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	var runs, secrets int
	err := r.db.QueryRow("SELECT count(*) FROM routine_runs").Scan(&runs)
	if err == nil {
		err = r.db.QueryRow("SELECT count(*) FROM automation_secrets").Scan(&secrets)
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if enabled || next != "" || stored.AutoStart || stored.WebhookSecretRef != "" || runs != 0 || secrets != 0 {
		t.Fatal("proposal scheduled work, enabled startup, or provisioned a credential")
	}
	_, err = routineCall(t, r, "routines.setEnabled", map[string]any{"id": draft.ID, "expectedRevision": draft.Revision, "enabled": true})
	requireStatus(t, err, 409)
}
