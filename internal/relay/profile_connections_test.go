package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"meshra/internal/adapters/openai"
	"meshra/internal/model"
)

func TestAdmittedEndpointPinsOriginAndRevokesCredentialReplacement(t *testing.T) {
	r, _ := openTest(t, nil)
	r.SetMCPURL("http://127.0.0.1:1/mcp")
	var firstCalls, secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		firstCalls.Add(1)
		if request.Header.Get("Authorization") != "Bearer fixture-first-credential" {
			t.Error("wrong inference credential reached admitted origin")
		}
		_, _ = io.WriteString(w, `{"model":"fixture","choices":[{"message":{"role":"assistant","content":"admitted origin"},"finish_reason":"stop"}]}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		secondCalls.Add(1)
		_, _ = io.WriteString(w, `{"model":"fixture","choices":[{"message":{"role":"assistant","content":"replacement origin"},"finish_reason":"stop"}]}`)
	}))
	defer second.Close()
	saved, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"endpoint": map[string]any{"name": "Pinned endpoint", "baseUrl": first.URL, "allowInsecureHttp": true, "enabled": true}, "apiKey": "fixture-first-credential"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := saved.(ProviderEndpointView)
	for _, origin := range []string{first.URL, second.URL} {
		if _, err := networkCall(t, r, admin, "network.grant", map[string]any{"connectionId": endpoint.ID, "origin": origin, "allowPrivate": true}); err != nil {
			t.Fatal(err)
		}
	}
	provider := model.ProfileProvider{Harness: "openai", ConnectionID: endpoint.ID, Config: model.Config{Provider: endpoint.ID, Model: "fixture"}}
	snapshot := model.ProfileTaskSnapshot{Profile: model.Profile{Binding: provider}, Binding: model.ProfileBinding{Harness: "openai", Config: provider.Config, ConnectionID: endpoint.ID}}
	r.mu.Lock()
	err = r.captureProfileConnectionLocked(&snapshot)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	adapter := openai.New(r.EndpointRuntimeConfig)
	native, err := adapter.Factory(context.Background(), model.Agent{ID: "openai:snapshot", Config: provider.Config}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	// Both current settings and the runtime's initial cache are now insufficient:
	// the admitted configuration must remain authoritative across this edit.
	edited, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"expectedRevision": endpoint.Revision, "endpoint": map[string]any{"id": endpoint.ID, "name": "Replacement endpoint", "baseUrl": second.URL, "allowInsecureHttp": true, "enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	current := edited.(ProviderEndpointView)
	disabled, err := connectionsCall(t, r, admin, "connections.disable", map[string]any{"id": endpoint.ID, "expectedRevision": current.Revision})
	if err != nil {
		t.Fatal(err)
	}
	current = disabled.(ProviderEndpointView)
	_, err = r.EndpointRuntimeConfig(context.Background(), endpoint.ID)
	requireStatus(t, err, 409)
	admitted := context.WithValue(context.Background(), admittedConnectionKey{}, &snapshot)
	result, err := native.Run(admitted, model.Turn{Task: model.Task{ID: randomID(), Text: "Which origin?"}}, nil)
	if err != nil || result != "admitted origin" {
		t.Fatalf("admitted result=%q err=%v", result, err)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatal("admitted inference was redirected to edited configuration")
	}
	_, err = connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"expectedRevision": current.Revision, "endpoint": map[string]any{"id": endpoint.ID, "name": "Rotated endpoint", "baseUrl": second.URL, "allowInsecureHttp": true, "enabled": false}, "apiKey": "fixture-replacement-credential"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = native.Run(admitted, model.Turn{Task: model.Task{ID: randomID(), Text: "Must not dispatch after credential replacement"}}, nil)
	if err == nil {
		t.Fatal("replaced credential retained admitted inference authority")
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatal("credential replacement dispatched an upstream request")
	}
}

func TestTextOnlyEndpointCompletesDefaultProfileAndRejectsToolGrants(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	r.SetMCPURL("http://127.0.0.1:1/mcp")
	r.mu.Lock()
	r.adapters["openai"] = openai.New(r.EndpointRuntimeConfig)
	r.mu.Unlock()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		var body struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Tools) != 0 {
			t.Error("text-only endpoint received a tool catalog")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Text-only response"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	saved, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"endpoint": map[string]any{"name": "Text-only endpoint", "baseUrl": upstream.URL, "allowInsecureHttp": true, "enabled": true, "supportsTools": false}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := saved.(ProviderEndpointView)
	if _, err = networkCall(t, r, admin, "network.grant", map[string]any{"connectionId": endpoint.ID, "origin": upstream.URL, "allowPrivate": true}); err != nil {
		t.Fatal(err)
	}
	request := profileStoreRequest(profileStoreWorkspace(t, r))
	request["binding"] = model.ProfileProvider{Harness: "openai", ConnectionID: endpoint.ID, Config: model.Config{Provider: endpoint.ID, Model: "text-only"}}
	profile := profileStoreCreate(t, r, request)
	conversation := continuityConversation(t, r, "chat", profile)
	task := continuitySend(t, r, profile, conversation, "Respond without tools")
	if completed := awaitTask(t, r, task.ID); completed.State != "completed" {
		t.Fatalf("text-only profile failed: %s", completed.Error)
	}
	if calls.Load() != 1 {
		t.Fatalf("plain inference calls=%d, want one", calls.Load())
	}
	request["capabilities"] = model.ProfileCapabilities{WorkspaceRead: true}
	requiresTools := profileStoreCreate(t, r, request)
	toolConversation := continuityConversation(t, r, "chat", requiresTools)
	payload, _ := json.Marshal(map[string]any{"id": requiresTools.ID, "conversationId": toolConversation.ID, "text": "Read a file", "idempotencyKey": "requires-tools"})
	_, err = r.profileSend(context.Background(), admin, payload)
	requireStatus(t, err, 400)
	if calls.Load() != 1 {
		t.Fatal("unsupported tool grant reached the text-only provider")
	}
}
