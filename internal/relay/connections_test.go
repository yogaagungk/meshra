package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

func connectionsCall(t *testing.T, r *Relay, principal model.Principal, operation string, payload any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return r.connectionsOperation(context.Background(), principal, operation, raw)
}

func connectionsEndpointPayload() map[string]any {
	return map[string]any{
		"endpoint": map[string]any{
			"name":              "Local model",
			"baseUrl":           "http://127.0.0.1:11434/v1",
			"allowInsecureHttp": true,
			"enabled":           true,
		},
	}
}

func TestConnectionOperationsRequireAdmin(t *testing.T) {
	r, _ := openTest(t, nil)
	worker := model.Principal{AgentID: "omp:worker"}
	if _, err := connectionsCall(t, r, worker, "connections.list", map[string]any{}); err == nil {
		t.Fatal("an unprivileged principal listed connections")
	} else {
		requireStatus(t, err, 403)
	}
	if _, err := connectionsCall(t, r, worker, "connections.saveEndpoint", connectionsEndpointPayload()); err == nil {
		t.Fatal("an unprivileged principal saved an endpoint")
	} else {
		requireStatus(t, err, 403)
	}
	if _, err := connectionsCall(t, r, worker, "connections.remove", map[string]any{"id": "abc", "expectedRevision": 1}); err == nil {
		t.Fatal("an unprivileged principal removed a connection")
	} else {
		requireStatus(t, err, 403)
	}
}

func TestEndpointURLValidation(t *testing.T) {
	r, _ := openTest(t, nil)
	rejected := []string{
		"https://user:password@example.com/v1",
		"https://example.com/v1?token=abc",
		"https://example.com/v1#fragment",
		"http://example.com/v1",
		"https://example.com/v1/" + strings.Repeat("x", 2100),
		"file:///etc/passwd",
	}
	for _, base := range rejected {
		payload := connectionsEndpointPayload()
		payload["endpoint"].(map[string]any)["baseUrl"] = base
		if _, err := connectionsCall(t, r, admin, "connections.saveEndpoint", payload); err == nil {
			t.Fatalf("accepted an invalid base URL: %s", base)
		} else {
			requireStatus(t, err, 400)
		}
	}
	// Plain HTTP needs the explicit local confirmation.
	payload := connectionsEndpointPayload()
	payload["endpoint"].(map[string]any)["allowInsecureHttp"] = false
	if _, err := connectionsCall(t, r, admin, "connections.saveEndpoint", payload); err == nil {
		t.Fatal("accepted plain HTTP without the local confirmation")
	} else {
		requireStatus(t, err, 400)
	}
	// A confirmed local endpoint is accepted and stored normalized.
	payload = connectionsEndpointPayload()
	payload["endpoint"].(map[string]any)["baseUrl"] = "HTTP://127.0.0.1:11434/v1/"
	result, err := connectionsCall(t, r, admin, "connections.saveEndpoint", payload)
	if err != nil {
		t.Fatal(err)
	}
	view := result.(ProviderEndpointView)
	if view.BaseURL != "http://127.0.0.1:11434/v1" {
		t.Fatalf("base URL was not normalized: %q", view.BaseURL)
	}
	if view.Revision != 1 || !view.Enabled {
		t.Fatalf("unexpected endpoint: %+v", view)
	}
	origin, err := EndpointOriginOf(view.ProviderEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if origin.Scheme != "http" || origin.Host != "127.0.0.1" || origin.Port != "11434" {
		t.Fatalf("unexpected origin: %+v", origin)
	}
	joined, err := EndpointURL(view.BaseURL, "chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if joined != "http://127.0.0.1:11434/v1/chat/completions" {
		t.Fatalf("unexpected joined URL: %q", joined)
	}
	if _, err = EndpointURL(view.BaseURL, "../escape"); err == nil {
		t.Fatal("a traversing path was accepted")
	}
}

func TestEndpointSaveGetDisableAndRemove(t *testing.T) {
	r, _ := openTest(t, nil)
	result, err := connectionsCall(t, r, admin, "connections.saveEndpoint", connectionsEndpointPayload())
	if err != nil {
		t.Fatal(err)
	}
	view := result.(ProviderEndpointView)
	if view.SecretStatus != "missing" {
		t.Fatalf("unexpected secret status: %q", view.SecretStatus)
	}
	// An update without the expected revision is a conflict.
	payload := connectionsEndpointPayload()
	payload["endpoint"].(map[string]any)["id"] = view.ID
	payload["expectedRevision"] = 99
	if _, err = connectionsCall(t, r, admin, "connections.saveEndpoint", payload); err == nil {
		t.Fatal("a stale revision was accepted")
	} else {
		requireStatus(t, err, 409)
	}
	payload["expectedRevision"] = view.Revision
	result, err = connectionsCall(t, r, admin, "connections.saveEndpoint", payload)
	if err != nil {
		t.Fatal(err)
	}
	updated := result.(ProviderEndpointView)
	if updated.Revision != view.Revision+1 {
		t.Fatalf("revision did not advance: %d", updated.Revision)
	}
	got, err := connectionsCall(t, r, admin, "connections.get", map[string]any{"id": view.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got.(ProviderEndpointView).Revision != updated.Revision {
		t.Fatalf("get returned a stale revision: %+v", got)
	}
	list, err := connectionsCall(t, r, admin, "connections.list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.(ConnectionList).Endpoints) != 1 {
		t.Fatalf("unexpected list: %+v", list)
	}
	disabled, err := connectionsCall(t, r, admin, "connections.disable", map[string]any{"id": view.ID, "expectedRevision": updated.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.(ProviderEndpointView).Enabled {
		t.Fatal("the endpoint was not disabled")
	}
	if _, err = connectionsCall(t, r, admin, "connections.remove", map[string]any{"id": view.ID, "expectedRevision": updated.Revision}); err == nil {
		t.Fatal("a stale revision removed the endpoint")
	} else {
		requireStatus(t, err, 409)
	}
	if _, err = connectionsCall(t, r, admin, "connections.remove", map[string]any{"id": view.ID, "expectedRevision": updated.Revision + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = connectionsCall(t, r, admin, "connections.get", map[string]any{"id": view.ID}); err == nil {
		t.Fatal("the removed endpoint was still readable")
	} else {
		requireStatus(t, err, 404)
	}
}

func TestEndpointRemovalBlockedWhenReferenced(t *testing.T) {
	r, _ := openTest(t, nil)
	result, err := connectionsCall(t, r, admin, "connections.saveEndpoint", connectionsEndpointPayload())
	if err != nil {
		t.Fatal(err)
	}
	view := result.(ProviderEndpointView)
	body, err := json.Marshal(map[string]any{"id": "omp:referencing", "harness": "omp", "config": map[string]any{"provider": view.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.db.Exec("INSERT INTO agents(id,body) VALUES(?,?)", "omp:referencing", string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = connectionsCall(t, r, admin, "connections.remove", map[string]any{"id": view.ID, "expectedRevision": view.Revision}); err == nil {
		t.Fatal("a referenced endpoint was removed")
	} else {
		requireStatus(t, err, 409)
	}
	if _, err = r.db.Exec("DELETE FROM agents WHERE id=?", "omp:referencing"); err != nil {
		t.Fatal(err)
	}
	if _, err = connectionsCall(t, r, admin, "connections.remove", map[string]any{"id": view.ID, "expectedRevision": view.Revision}); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointModelListingUsesTheConfiguredOrigin(t *testing.T) {
	r, _ := openTest(t, nil)
	r.SetMCPURL("http://127.0.0.1:1/mcp")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"listed-model","name":"Listed model"},{"model":"alias-model"},{"id":"listed-model"}]}`)
	}))
	defer server.Close()
	result, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{
		"endpoint": map[string]any{
			"name":              "Fixture",
			"baseUrl":           server.URL,
			"allowInsecureHttp": true,
			"enabled":           true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	view := result.(ProviderEndpointView)
	if _, err := networkCall(t, r, admin, "network.grant", map[string]any{"connectionId": view.ID, "origin": server.URL, "allowPrivate": true}); err != nil {
		t.Fatal(err)
	}
	options, err := connectionsCall(t, r, admin, "connections.models", map[string]any{"id": view.ID})
	if err != nil {
		t.Fatal(err)
	}
	models := options.([]model.ModelOption)
	if len(models) != 2 || models[0].ID != "alias-model" || models[1].ID != "listed-model" {
		t.Fatalf("unexpected model listing: %+v", models)
	}
	if models[1].Name != "Listed model" || models[0].Name != "alias-model" {
		t.Fatalf("unexpected model names: %+v", models)
	}
	// A disabled endpoint refuses new use.
	if _, err = connectionsCall(t, r, admin, "connections.disable", map[string]any{"id": view.ID, "expectedRevision": view.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err = connectionsCall(t, r, admin, "connections.models", map[string]any{"id": view.ID}); err == nil {
		t.Fatal("a disabled endpoint listed models")
	} else {
		requireStatus(t, err, 409)
	}
}

func TestLocalScanProbesOnlyTheReviewedLoopbackEndpoints(t *testing.T) {
	r, _ := openTest(t, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"local-model"},{"id":"another-model"}]}`)
	}))
	defer server.Close()
	original := localModelProbeURLs
	localModelProbeURLs = []string{server.URL + "/v1/models"}
	defer func() { localModelProbeURLs = original }()
	results, err := r.scanLocalConnections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("the probe scanned more than the reviewed endpoint: %+v", results)
	}
	if results[0].Status != "available" || len(results[0].Models) != 2 || results[0].Models[0] != "another-model" {
		t.Fatalf("unexpected probe result: %+v", results[0])
	}
	// A failing endpoint is reported, not hidden.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer failing.Close()
	localModelProbeURLs = []string{failing.URL + "/v1/models"}
	results, err = r.scanLocalConnections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "error" {
		t.Fatalf("unexpected probe status: %+v", results[0])
	}
	// An unreachable endpoint is reported as unreachable.
	localModelProbeURLs = []string{"http://127.0.0.1:1/v1/models"}
	results, err = r.scanLocalConnections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "unreachable" {
		t.Fatalf("unexpected probe status: %+v", results[0])
	}
	// The operation is operator only.
	if _, err = connectionsCall(t, r, model.Principal{AgentID: "omp:worker"}, "connections.scanLocal", map[string]any{}); err == nil {
		t.Fatal("an unprivileged principal ran the local probe")
	} else {
		requireStatus(t, err, 403)
	}
}

func TestEndpointDiscoveryCancelsWhenDisabled(t *testing.T) {
	r, _ := openTest(t, nil)
	r.SetMCPURL("http://127.0.0.1:1/mcp")
	started, cancelled := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	saved, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"endpoint": map[string]any{"name": "Cancellable discovery", "baseUrl": server.URL, "allowInsecureHttp": true, "enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	view := saved.(ProviderEndpointView)
	if _, err = networkCall(t, r, admin, "network.grant", map[string]any{"connectionId": view.ID, "origin": server.URL, "allowPrivate": true}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"id": view.ID})
	result := make(chan error, 1)
	go func() { _, err := r.listEndpointModels(context.Background(), payload); result <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("discovery did not reach the explicitly granted fixture")
	}
	if _, err = connectionsCall(t, r, admin, "connections.disable", map[string]any{"id": view.ID, "expectedRevision": view.Revision}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("disabled discovery returned a success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disable did not cancel pending discovery")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream discovery remained active after disable")
	}
}

func TestEndpointAPIFormat(t *testing.T) {
	r, _ := openTest(t, nil)
	save := func(format string, extra map[string]any) (ProviderEndpointView, error) {
		endpoint := map[string]any{"name": "Provider", "baseUrl": "https://api.example.com", "enabled": true, "supportsTools": true, "supportsImages": true}
		if format != "" {
			endpoint["apiFormat"] = format
		}
		for k, v := range extra {
			endpoint[k] = v
		}
		result, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"endpoint": endpoint})
		if err != nil {
			return ProviderEndpointView{}, err
		}
		return result.(ProviderEndpointView), nil
	}
	if view, err := save("", nil); err != nil || view.APIFormat != "openai" {
		t.Fatalf("the default format must be openai: %+v %v", view, err)
	}
	if view, err := save("anthropic", nil); err != nil || view.APIFormat != "anthropic" || !view.SupportsTools {
		t.Fatalf("anthropic endpoint was not stored: %+v %v", view, err)
	}
	view, err := save("typesafe", nil)
	if err != nil || view.APIFormat != "typesafe" || view.SupportsTools || view.SupportsImages {
		t.Fatalf("typesafe must be stored without tool or image support: %+v %v", view, err)
	}
	if _, err := save("gemini", nil); err == nil {
		t.Fatal("an unknown API format was accepted")
	} else {
		requireStatus(t, err, 400)
	}
	// A partial edit that omits apiFormat keeps the stored protocol.
	result, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"expectedRevision": view.Revision, "endpoint": map[string]any{"id": view.ID, "name": "Renamed"}})
	if err != nil {
		t.Fatal(err)
	}
	if kept := result.(ProviderEndpointView); kept.APIFormat != "typesafe" || kept.Name != "Renamed" {
		t.Fatalf("a partial edit changed the protocol: %+v", kept)
	}
}

// The OpenCode Go gateway lists models of several protocols and types under one
// root. Each connection format offers only the models it can run.
func TestModelListingOpenCodeGoFiltersByProtocol(t *testing.T) {
	body := []byte(`{"object":"list","data":[
		{"id":"kimi-k3","name":"Kimi K3","type":"chat","api":{"chat-completions":{}}},
		{"id":"glm-5.3","name":"GLM 5.3","type":"chat","api":{"chat-completions":{}}},
		{"id":"minimax-m3","name":"MiniMax M3","type":"chat","api":{"messages":{}}},
		{"id":"gpt-6-luna","name":"GPT 6 Luna","type":"chat","api":{"responses":{}}},
		{"id":"gemini-4-argon","type":"chat","api":{"generate-content":{}}},
		{"id":"embed-1","type":"embedding","api":{"chat-completions":{}}}]}`)
	ids := func(format string) []string {
		options, err := parseModelOptions(body, format)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, option := range options {
			out = append(out, option.ID)
		}
		return out
	}
	if got := strings.Join(ids("openai"), ","); got != "glm-5.3,kimi-k3" {
		t.Fatalf("openai format must list chat-completions models only: %s", got)
	}
	if got := strings.Join(ids("anthropic"), ","); got != "minimax-m3" {
		t.Fatalf("anthropic format must list messages models only: %s", got)
	}
	options, _ := parseModelOptions(body, "")
	if options[1].Name != "Kimi K3" {
		t.Fatalf("display names must be kept: %+v", options)
	}
}

func TestModelListingTypeSafeShape(t *testing.T) {
	body := []byte(`{"models":[{"name":"jev-latest","description":"General-purpose system one model.","release_date":"2026-09-15"},{"name":"jev-1","description":"x","release_date":"2026-01-01"}]}`)
	options, err := parseModelOptions(body, "typesafe")
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 2 || options[0].ID != "jev-1" || options[1].ID != "jev-latest" {
		t.Fatalf("TypeSafe models must be listed by name: %+v", options)
	}
}

// Test API for model discovery: the exact request each provider format sends.
func TestEndpointModelListingRequestPerFormat(t *testing.T) {
	cases := []struct {
		format, header, value, absent, query, response string
		want                                           string
	}{
		{"openai", "Authorization", "Bearer sk-test", "x-api-key", "", `{"data":[{"id":"kimi-k3","api":{"chat-completions":{}}}]}`, "kimi-k3"},
		{"anthropic", "x-api-key", "sk-test", "Authorization", "limit=1000", `{"data":[{"id":"claude-x","display_name":"Claude X"}]}`, "claude-x"},
		{"typesafe", "Authorization", "Bearer sk-test", "x-api-key", "", `{"models":[{"name":"jev-latest","description":"d","release_date":"2026-09-15"}]}`, "jev-latest"},
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			r, _ := openTest(t, nil)
			r.SetMCPURL("http://127.0.0.1:1/mcp")
			var seen http.Header
			var query string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				seen, query = request.Header.Clone(), request.URL.RawQuery
				if request.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				io.WriteString(w, c.response)
			}))
			defer server.Close()
			result, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"apiKey": "sk-test", "endpoint": map[string]any{"name": "Fixture", "baseUrl": server.URL, "allowInsecureHttp": true, "enabled": true, "apiFormat": c.format}})
			if err != nil {
				t.Fatal(err)
			}
			view := result.(ProviderEndpointView)
			if _, err := networkCall(t, r, admin, "network.grant", map[string]any{"connectionId": view.ID, "origin": server.URL, "allowPrivate": true}); err != nil {
				t.Fatal(err)
			}
			options, err := connectionsCall(t, r, admin, "connections.models", map[string]any{"id": view.ID})
			if err != nil {
				t.Fatal(err)
			}
			models := options.([]model.ModelOption)
			if len(models) != 1 || models[0].ID != c.want {
				t.Fatalf("unexpected models: %+v", models)
			}
			if seen.Get(c.header) != c.value || seen.Get(c.absent) != "" || query != c.query {
				t.Fatalf("unexpected request: headers=%v query=%q", seen, query)
			}
			if c.format == "anthropic" && seen.Get("anthropic-version") == "" {
				t.Fatal("anthropic-version header missing")
			}
		})
	}
}

func TestEndpointModelListingReportsHTTPStatus(t *testing.T) {
	r, _ := openTest(t, nil)
	r.SetMCPURL("http://127.0.0.1:1/mcp")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"detail":{"message":"Must supply an API key!"}}`)
	}))
	defer server.Close()
	result, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"endpoint": map[string]any{"name": "Fixture", "baseUrl": server.URL, "allowInsecureHttp": true, "enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	view := result.(ProviderEndpointView)
	if _, err := networkCall(t, r, admin, "network.grant", map[string]any{"connectionId": view.ID, "origin": server.URL, "allowPrivate": true}); err != nil {
		t.Fatal(err)
	}
	if _, err = connectionsCall(t, r, admin, "connections.models", map[string]any{"id": view.ID}); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("the HTTP status must be reported: %v", err)
	}
}
