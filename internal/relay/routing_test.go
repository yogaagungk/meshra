package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"meshra/internal/model"
)

func TestParseRouteAnswerIsStrictAndNeverBroadcasts(t *testing.T) {
	eligible := map[string]bool{"claude:a": true, "omp:b": true}
	ok := func(input string) routeAnswer {
		t.Helper()
		v, err := parseRouteAnswer(input, eligible)
		if err != nil {
			t.Fatalf("rejected %q: %v", input, err)
		}
		return v
	}
	if got := ok(`{"agentIds":["omp:b","omp:b","ghost"],"reason":"fits"}`); len(got.AgentIDs) != 1 || got.AgentIDs[0] != "omp:b" || got.Reason != "fits" {
		t.Fatalf("unknown or duplicate members were kept: %+v", got)
	}
	if got := ok("```json\n{\"agentIds\":[],\"reason\":\"ambiguous\"}\n```"); len(got.AgentIDs) != 0 {
		t.Fatalf("empty answer should select nobody: %+v", got)
	}
	for _, input := range []string{
		``, `null`, `[]`, `{}`, `{"agentIds":null}`,
		`{"agentIds":["ghost"],"reason":"x"}`,
		`{"agentIds":["claude:a"],"reason":"x","permissions":{}}`,
		`{"agentIds":["claude:a"]} {"agentIds":["omp:b"]}`,
		`{"agentIds":["claude:a"]} trailing`,
		strings.Repeat("x", 24577),
	} {
		if _, err := parseRouteAnswer(input, eligible); err == nil {
			t.Fatalf("accepted unusable routing answer: %.80s", input)
		}
	}
}

func TestLeadRoutingIsToolFreeNarrowingAndValidated(t *testing.T) {
	output := ""
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return output, nil })
	lead := createTest(t, r, "omp", "lead")
	worker := createTest(t, r, "omp", "worker")
	outsider := createTest(t, r, "omp", "outsider")
	v, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "routing"})
	if err != nil {
		t.Fatal(err)
	}
	conversation := v.(model.Conversation)
	for _, id := range []string{lead.ID, worker.ID} {
		if _, err = r.Collaborate(admin, "conversation.addParticipant", model.CollaborationRequest{ID: conversation.ID, AgentID: id}); err != nil {
			t.Fatal(err)
		}
	}
	call := func(op string, payload any) (any, error) {
		body, _ := json.Marshal(payload)
		return r.routingOperation(context.Background(), admin, op, body)
	}
	request := map[string]any{"conversationId": conversation.ID, "leadAgentId": lead.ID, "text": "please review the API"}

	// Harnesses that cannot prove tool-free operation are refused.
	_, err = call("routing.route", request)
	requireStatus(t, err, 503)
	r.mu.Lock()
	adapter := r.adapters["omp"]
	adapter.Capabilities["toolFreeDraft"] = true
	adapter.Factory = func(_ context.Context, agent model.Agent, _ model.Emit) (model.Runtime, error) {
		if !agent.GenerationDraft || len(agent.Policy.Destinations) != 0 || agent.Policy.Delegate {
			t.Errorf("router inherited execution authority: %+v", agent)
		}
		return &fakeRuntime{session: randomID(), run: func(context.Context, model.Turn, model.Emit) (string, error) { return output, nil }}, nil
	}
	r.adapters["omp"] = adapter
	r.mu.Unlock()

	// Only conversation members may lead; the lead must be a participant.
	_, err = call("routing.route", map[string]any{"conversationId": conversation.ID, "leadAgentId": outsider.ID, "text": "hi"})
	requireStatus(t, err, 400)
	_, err = call("routing.route", map[string]any{"conversationId": conversation.ID, "leadAgentId": lead.ID, "text": "token sk-ant-api03-" + strings.Repeat("a", 40)})
	if err == nil {
		t.Fatal("credential-like text was sent to a routing model")
	}

	output = `{"agentIds":["` + worker.ID + `","` + outsider.ID + `"],"reason":"API review"}`
	v, err = call("routing.route", request)
	if err != nil {
		t.Fatal(err)
	}
	taskID := v.(map[string]any)["taskId"].(string)
	awaitTask(t, r, taskID)
	v, err = call("routing.result", map[string]any{"taskId": taskID})
	if err != nil {
		t.Fatal(err)
	}
	result := v.(routeResult)
	if result.State != "completed" || result.Error != "" || len(result.AgentIDs) != 1 || result.AgentIDs[0] != worker.ID || result.Reason != "API review" {
		t.Fatalf("answer was not narrowed to participants: %+v", result)
	}

	output = "I think the worker should handle it."
	v, err = call("routing.route", request)
	if err != nil {
		t.Fatal(err)
	}
	taskID = v.(map[string]any)["taskId"].(string)
	awaitTask(t, r, taskID)
	v, err = call("routing.result", map[string]any{"taskId": taskID})
	if err != nil {
		t.Fatal(err)
	}
	if result = v.(routeResult); result.Error == "" || len(result.AgentIDs) != 0 {
		t.Fatalf("unreadable routing was repaired instead of surfaced: %+v", result)
	}
	_, err = call("routing.result", map[string]any{"taskId": "does-not-exist"})
	requireStatus(t, err, 404)
}
