package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

func TestBrokerSessionSearchAcrossAuthorizedConversations(t *testing.T) {
	script := newBrokerPlan(func(turn model.Turn) []brokerStep {
		if !strings.Contains(turn.Task.Text, "search-now") {
			return nil
		}
		return []brokerStep{{call: model.ToolCall{ID: "session-search", Name: "meshra_session_search", Arguments: json.RawMessage(`{"query":"brokerrecall","limit":5}`)}}}
	})
	r, _ := continuityOpenTest(t, script.run)
	r.SetMCPURL("http://127.0.0.1:1/mcp")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		t.Error("fake harness unexpectedly called the endpoint")
	}))
	defer upstream.Close()
	r.mu.Lock()
	r.adapters["openai"] = r.adapters["omp"]
	r.mu.Unlock()
	saved, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"endpoint": map[string]any{"name": "Broker fixture", "baseUrl": upstream.URL, "allowInsecureHttp": true, "enabled": true, "supportsTools": true}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := saved.(ProviderEndpointView)
	if _, err = networkCall(t, r, admin, "network.grant", map[string]any{"connectionId": endpoint.ID, "origin": upstream.URL, "allowPrivate": true}); err != nil {
		t.Fatal(err)
	}
	request := profileStoreRequest(profileStoreWorkspace(t, r))
	request["binding"] = model.ProfileProvider{Harness: "openai", ConnectionID: endpoint.ID, Config: model.Config{Provider: endpoint.ID, Model: "fixture"}}
	profile := profileStoreCreate(t, r, request)
	other := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	one := continuityConversation(t, r, "chat", profile)
	two := continuityConversation(t, r, "chat", profile)
	private := continuityConversation(t, r, "chat", other)
	prior := awaitTask(t, r, continuitySend(t, r, profile, two, "brokerrecall from another conversation").ID)
	awaitTask(t, r, continuitySend(t, r, other, private, "brokerrecall private other profile").ID)
	current := continuitySend(t, r, profile, one, "search-now")
	awaitTask(t, r, current.ID)
	outcome := script.await(t, current.ID, 0, 10*time.Second)
	if outcome.err != nil {
		t.Fatalf("broker search failed: %v", outcome.err)
	}
	var page model.AutomationPage[profileSessionHit]
	if err = json.Unmarshal([]byte(brokerText(outcome.result)), &page); err != nil {
		t.Fatalf("invalid broker search result: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].TaskID != prior.ID || page.Items[0].ConversationID != two.ID {
		t.Fatalf("unexpected broker session results: %+v", page)
	}
}
