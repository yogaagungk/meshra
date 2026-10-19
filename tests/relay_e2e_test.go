package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/api"
	"meshra/internal/model"
	"meshra/internal/relay"
)

type echoRuntime struct {
	id       string
	mu       *sync.Mutex
	received map[string]int
	agent    string
}

func (r *echoRuntime) SessionID() string { return r.id }
func (r *echoRuntime) Close() error      { return nil }
func (r *echoRuntime) Run(ctx context.Context, turn model.Turn, e model.Emit) (string, error) {
	r.mu.Lock()
	r.received[r.agent]++
	r.mu.Unlock()
	if turn.Task.Text == "stream" {
		for range 300 {
			e("delta", "chunk")
		}
	}
	return "reply:" + r.agent + ":" + turn.Task.Text, nil
}
func httpJSON(t *testing.T, client *http.Client, url, token, method, path string, input, output any, want int) {
	t.Helper()
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d want %d: %s", method, path, resp.StatusCode, want, b)
	}
	if output != nil {
		if err = json.Unmarshal(b, output); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
	}
}
func TestLocalEndToEndIdentityMCPAndStreamReplay(t *testing.T) {
	var mu sync.Mutex
	received := map[string]int{}
	adapters := map[string]model.Adapter{}
	for _, h := range []string{"omp", "claude", "codex", "opencode"} {
		adapters[h] = model.Adapter{Factory: func(_ context.Context, a model.Agent, _ model.Emit) (model.Runtime, error) {
			return &echoRuntime{id: "native-" + a.ID, mu: &mu, received: received, agent: a.ID}, nil
		}}
	}
	dir := t.TempDir()
	r, err := relay.Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	server := httptest.NewServer(api.New(r))
	defer server.Close()
	r.SetMCPURL(server.URL + "/mcp")
	client := server.Client()
	client.Timeout = 5 * time.Second
	b, err := os.ReadFile(filepath.Join(dir, "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	admin := string(b)
	for _, name := range []string{"claude:exampleone", "claude:exampletwo", "omp:exampleworker", "codex:reviewer", "opencode:testengine"} {
		parts := strings.Split(name, ":")
		var a model.Agent
		httpJSON(t, client, server.URL, admin, "POST", "/v1/agents", model.CreateAgent{Harness: parts[0], Name: parts[1], Cwd: t.TempDir(), Policy: model.Policy{Destinations: []string{"claude:exampleone", "codex:reviewer"}, Operations: []string{"list", "send", "request", "status", "events", "cancel"}, Delegate: true}}, &a, 201)
		if a.ID != name {
			t.Fatalf("identity changed: %+v", a)
		}
	}
	for _, name := range []string{"claude:exampleone", "claude:exampletwo", "omp:exampleworker"} {
		httpJSON(t, client, server.URL, admin, "POST", "/v1/agents/"+name+"/start", nil, nil, 200)
	}
	token, err := r.Token(model.Principal{Admin: true}, "omp:exampleworker")
	if err != nil {
		t.Fatal(err)
	}
	var task model.Task
	httpJSON(t, client, server.URL, token, "POST", "/v1/request", model.SendRequest{To: "claude:exampleone", Text: "stream"}, &task, 202)
	httpJSON(t, client, server.URL, token, "POST", "/v1/send", model.SendRequest{From: "claude:exampletwo", To: "claude:exampleone", Text: "impersonate"}, nil, 403)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		httpJSON(t, client, server.URL, token, "GET", "/v1/tasks/"+task.ID, nil, &task, 200)
		if model.Terminal(task.State) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if task.State != "completed" || task.Result != "reply:claude:exampleone:stream" || task.OriginAgentID != "omp:exampleworker" || task.OriginSessionID != "native-omp:exampleworker" {
		t.Fatalf("incorrect reply: %+v", task)
	}
	mu.Lock()
	wrong := received["claude:exampletwo"]
	mu.Unlock()
	if wrong != 0 {
		t.Fatal("wallet received prod's message")
	}
	req, err := http.NewRequest("GET", server.URL+"/v1/tasks/"+task.ID+"/events?follow=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || strings.Count(string(stream), `"type":"delta"`) != 300 {
		t.Fatalf("SSE lost completed-task pages: status=%d delta=%d", resp.StatusCode, strings.Count(string(stream), `"type":"delta"`))
	}
	var rpc map[string]any
	httpJSON(t, client, server.URL, token, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}, &rpc, 200)
	result, ok := rpc["result"].(map[string]any)
	if !ok {
		t.Fatalf("MCP tools/list failed: %+v", rpc)
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 7 {
		t.Fatalf("MCP tools missing: %+v", result)
	}
	httpJSON(t, client, server.URL, token, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "meshra_task_status", "arguments": map[string]any{"taskId": task.ID}}}, &rpc, 200)
	result, ok = rpc["result"].(map[string]any)
	if !ok || result["isError"] == true {
		t.Fatalf("MCP authenticated task lookup failed: %+v", rpc)
	}
	httpJSON(t, client, server.URL, "incorrect", "GET", "/v1/agents", nil, nil, 401)
	walletToken, err := r.Token(model.Principal{Admin: true}, "claude:exampletwo")
	if err != nil {
		t.Fatal(err)
	}
	httpJSON(t, client, server.URL, walletToken, "GET", "/v1/tasks/"+task.ID, nil, nil, 403)
}
