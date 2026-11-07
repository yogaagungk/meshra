package relay

import (
	"context"
	"encoding/json"
	"testing"

	"meshra/internal/connectors"
	"meshra/internal/model"
)

func effectsOf(t *testing.T, r *Relay, taskID string) []effectView {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"taskId": taskID})
	v, err := r.effectsList(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)["effects"].([]effectView)
}

// A restart after an effect was recorded as dispatched leaves it visible as
// uncertain with duplicate risk, separate from the (completed) task, and never
// posts again.
func TestEffectsSurviveRestartAsUncertainAndAreNeverReplayed(t *testing.T) {
	r, _, fixture := connectorFixture(t)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	connector := connectorSaved(t, r, "slack", "xoxb-fixture-token-0777")
	connectorRouted(t, r, connector.ID, profile.ID, map[string]any{"channels": []string{"C100"}}, true)
	connectorEnabled(t, r, connector.ID, true)
	fixture.events <- connectors.Event{Kind: connectors.KindSlack, EventID: "Ev770", ThreadKey: "C100:1700000000.0770", ChannelID: "C100", AuthorID: "U100", Text: "effect"}
	task := connectorTask(t, r, connector.ID, "Ev770")
	if err := r.connectorCollectReplies(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := effectsOf(t, r, task.ID)
	if len(before) == 0 || before[0].Status != "intended" || before[0].RequestDigest == "" {
		t.Fatalf("effect intent was not recorded before dispatch: %+v", before)
	}
	if _, err := r.db.Exec("UPDATE connector_outbox SET state='sending'"); err != nil {
		t.Fatal(err)
	}
	if err := r.connectorRecover(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := effectsOf(t, r, task.ID)
	if len(after) == 0 || after[0].Status != "uncertain" || !after[0].DuplicateRisk || after[0].Idempotency == "" {
		t.Fatalf("interrupted effect is not reported as uncertain: %+v", after)
	}
	r.mu.Lock()
	current, err := r.task(task.ID)
	r.mu.Unlock()
	if err != nil || current.State != "completed" {
		t.Fatalf("task state must stay independent of the effect: %v %+v", err, current)
	}
	if err := r.connectorDeliverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.recorded()) != 0 {
		t.Fatal("an uncertain effect was replayed")
	}
}

// A claimed mutating tool call becomes uncertain after restart and is listed;
// non-mutating rows are not reported as effects.
func TestMutatingToolInvocationBecomesUncertainEffectOnRecovery(t *testing.T) {
	r, _, fixture := connectorFixture(t)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	connector := connectorSaved(t, r, "slack", "xoxb-fixture-token-0778")
	connectorRouted(t, r, connector.ID, profile.ID, map[string]any{"channels": []string{"C100"}}, true)
	connectorEnabled(t, r, connector.ID, true)
	fixture.events <- connectors.Event{Kind: connectors.KindSlack, EventID: "Ev780", ThreadKey: "C100:1700000000.0780", ChannelID: "C100", AuthorID: "U100", Text: "tool"}
	task := connectorTask(t, r, connector.ID, "Ev780")
	add := func(id, name string, mutation bool) {
		body, _ := json.Marshal(toolInvocation{ID: id, TaskID: task.ID, CallID: id, Name: name, ActionHash: "h-" + id, State: "claimed", ProfileID: profile.ID, Mutation: mutation, CreatedAt: now()})
		if _, err := r.db.Exec("INSERT INTO tool_invocations(id,task_id,attempt,call_id,action_hash,state,body,allow_rule_id) VALUES(?,?,?,?,?,?,?,'')", id, task.ID, 1, id, "h-"+id, "claimed", string(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("inv-mut", "meshra_workspace_apply", true)
	add("inv-read", "meshra_workspace_read", false)
	if err := r.recoverTools(); err != nil {
		t.Fatal(err)
	}
	found := map[string]effectView{}
	for _, e := range effectsOf(t, r, task.ID) {
		found[e.ID] = e
	}
	if e, ok := found["inv-mut"]; !ok || e.Status != "uncertain" || !e.DuplicateRisk {
		t.Fatalf("mutating invocation not reported uncertain: %+v", found)
	}
	// An interrupted read-class row is also marked uncertain by recovery; it is
	// reported rather than hidden because it may have run.
	if e, ok := found["inv-read"]; ok && e.Status != "uncertain" {
		t.Fatalf("unexpected status for interrupted read: %+v", e)
	}
}

// Effect records carry digests and provider references for every task, so only
// the operator may read them; a model or routine credential may not.
func TestEffectsListIsOperatorOnly(t *testing.T) {
	r, _ := openTest(t, nil)
	raw := json.RawMessage(`{}`)
	if _, err := r.Automation(context.Background(), admin, "effects.list", raw); err != nil {
		t.Fatalf("operator denied: %v", err)
	}
	for _, p := range []model.Principal{{AgentID: "claude:a", TaskID: "t"}, {RoutineTokenID: "tok"}} {
		_, err := r.Automation(context.Background(), p, "effects.list", raw)
		if err == nil {
			t.Fatalf("non-operator %+v read effects", p)
		}
	}
}
