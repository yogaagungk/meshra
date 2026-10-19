package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/model"
)

type fakeService struct {
	mu          sync.Mutex
	sent        []model.SendRequest
	callers     []model.Principal
	taskState   string
	events      []model.Event
	finalEvents []model.Event
	eventReads  int
}

func (f *fakeService) Authenticate(token string) (model.Principal, error) {
	if token == "admin" {
		return model.Principal{Admin: true}, nil
	}
	if token != "agent" {
		return model.Principal{}, model.Err(401, "invalid token")
	}
	return model.Principal{AgentID: "omp:origin", SessionID: "native", TaskID: "parent"}, nil
}
func (f *fakeService) Create(p model.Principal, r model.CreateAgent) (model.Agent, error) {
	return model.Agent{ID: r.Harness + ":" + r.Name, Cwd: r.Cwd}, nil
}
func (f *fakeService) Agents(p model.Principal) ([]model.Agent, error) {
	return []model.Agent{{ID: "claude:one"}, {ID: "claude:two"}}, nil
}
func (f *fakeService) Agent(p model.Principal, id string) (model.Agent, error) {
	if id == "missing" {
		return model.Agent{}, model.Err(404, "agent not found")
	}
	return model.Agent{ID: id, State: "idle"}, nil
}
func (f *fakeService) Action(p model.Principal, id, action string) (model.Agent, error) {
	return model.Agent{ID: id, State: action}, nil
}
func (f *fakeService) Remove(p model.Principal, id string) error { return nil }
func (f *fakeService) Token(p model.Principal, id string) (string, error) {
	return "agent-credential", nil
}
func (f *fakeService) Send(p model.Principal, r model.SendRequest) (model.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, r)
	f.callers = append(f.callers, p)
	return model.Task{ID: "task-1", State: "queued", From: p.AgentID, To: r.To, Text: r.Text, Type: r.Type}, nil
}
func (f *fakeService) Task(p model.Principal, id string) (model.Task, error) {
	if id == "private" {
		return model.Task{}, model.Err(403, "task is private")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return model.Task{ID: id, State: f.taskState}, nil
}
func (f *fakeService) Events(p model.Principal, id string, after int64) ([]model.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventReads++
	all := f.events
	if f.eventReads > 1 {
		all = append(append([]model.Event{}, all...), f.finalEvents...)
	}
	out := []model.Event{}
	for _, e := range all {
		if e.ID > after {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeService) Cancel(p model.Principal, id string) (model.Task, error) {
	return model.Task{ID: id, State: "cancelled"}, nil
}
func (f *fakeService) Retry(p model.Principal, id string) (model.Task, error) {
	return model.Task{ID: id, State: "queued"}, nil
}
func httpCall(t *testing.T, h http.Handler, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestAuthenticationAndOrigin(t *testing.T) {
	h := New(&fakeService{})
	for _, path := range []string{"/v1/agents", "/v1/identity", "/v1/tasks/t/events?follow=true", "/mcp", "/unknown"} {
		for _, token := range []string{"", "wrong"} {
			w := httpCall(t, h, "GET", path, token, "", nil)
			if w.Code != 401 {
				t.Errorf("%s: status %d", path, w.Code)
			}
		}
		w := httpCall(t, h, "GET", path, "admin", "", map[string]string{"Origin": "http://evil.invalid"})
		if w.Code != 403 {
			t.Errorf("%s origin status %d", path, w.Code)
		}
	}
}
func TestStrictBoundedJSON(t *testing.T) {
	h := New(&fakeService{})
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"to":"claude:one","text":"hello","unknown":1}`, 400},
		{`{"to":"claude:one","text":"hello"} {}`, 400},
		{`{"to":`, 400},
		{`{"text":"` + strings.Repeat("x", maxBody) + `"}`, 413},
	} {
		w := httpCall(t, h, "POST", "/v1/send", "agent", tc.body, nil)
		if w.Code != tc.status {
			t.Errorf("status=%d want=%d: %s", w.Code, tc.status, w.Body.String())
		}
	}
	w := httpCall(t, h, "POST", "/v1/send", "agent", `{}`, map[string]string{"Content-Type": "text/plain"})
	if w.Code != 415 {
		t.Fatal(w.Code)
	}
}
func TestHTTPPrincipalAndServiceErrors(t *testing.T) {
	f := &fakeService{}
	h := New(f)
	w := httpCall(t, h, "POST", "/v1/request", "agent", `{"to":"claude:one","text":"hello","from":"omp:other"}`, nil)
	if w.Code != 403 || len(f.sent) != 0 {
		t.Fatal("impersonation reached service")
	}
	w = httpCall(t, h, "POST", "/v1/request", "agent", `{"to":"claude:one","text":"hello"}`, nil)
	if w.Code != 202 || len(f.sent) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.callers[0].AgentID != "omp:origin" || f.callers[0].TaskID != "parent" || f.sent[0].Type != "request" {
		t.Fatalf("lost identity/ancestry: %+v %+v", f.callers, f.sent)
	}
	w = httpCall(t, h, "GET", "/v1/agents/missing", "agent", "", nil)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "agent not found") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httpCall(t, h, "GET", "/v1/tasks/private/events?follow=true", "agent", "", nil)
	if w.Code != 403 || strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
		t.Fatal("unauthorized stream opened")
	}
}
func TestSSEReplayTerminalRaceAndReconnect(t *testing.T) {
	f := &fakeService{taskState: "completed", events: []model.Event{{ID: 1, Type: "queued"}, {ID: 2, Type: "delta", Data: "line\nnext"}}, finalEvents: []model.Event{{ID: 3, Type: "completed"}}}
	h := New(f)
	w := httpCall(t, h, "GET", "/v1/tasks/t/events?follow=true&after=0", "agent", "", map[string]string{"Last-Event-ID": "1"})
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
		t.Fatal(w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "id: 1\n") || !strings.Contains(body, "id: 2\n") || !strings.Contains(body, "id: 3\n") || !strings.Contains(body, `line\nnext`) {
		t.Fatal(body)
	}
	w = httpCall(t, h, "GET", "/v1/tasks/t/events?follow=true", "agent", "", map[string]string{"Last-Event-ID": "2"})
	if strings.Contains(w.Body.String(), "id: 2\n") || strings.Count(w.Body.String(), "id: 3\n") != 1 {
		t.Fatal(w.Body.String())
	}
	w = httpCall(t, h, "GET", "/v1/tasks/t/events?after=3", "agent", "", nil)
	if w.Body.String() != "[]\n" {
		t.Fatal(w.Body.String())
	}
	w = httpCall(t, h, "GET", "/v1/tasks/t/events?after=-1", "agent", "", nil)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
func TestSSEContextCancellation(t *testing.T) {
	f := &fakeService{taskState: "running"}
	srv := httptest.NewServer(New(f))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/tasks/t/events?follow=true", nil)
	req.Header.Set("Authorization", "Bearer agent")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	defer resp.Body.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); done <- err }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream ignored cancellation")
	}
}
func rpc(t *testing.T, h http.Handler, id int, name string, args string) map[string]json.RawMessage {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
	w := httpCall(t, h, "POST", "/mcp", "agent", body, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestMCPToolsAndIdentity(t *testing.T) {
	f := &fakeService{}
	h := New(f)
	w := httpCall(t, h, "POST", "/mcp", "agent", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"protocolVersion":"2025-11-25"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httpCall(t, h, "POST", "/mcp", "agent", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if w.Code != 202 || w.Body.Len() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httpCall(t, h, "POST", "/mcp", "agent", `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`, nil)
	var listed struct {
		Result struct {
			Tools []toolDefinition `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Result.Tools) != 7 {
		t.Fatal(w.Body.String())
	}
	for _, tool := range listed.Result.Tools {
		if tool.InputSchema["type"] != "object" || tool.InputSchema["properties"] == nil || tool.InputSchema["additionalProperties"] != false {
			t.Fatalf("invalid schema %+v", tool)
		}
	}
	for _, tc := range []struct{ name, args string }{
		{"meshra_agents_list", `{}`}, {"meshra_agent_status", `{"id":"claude:one"}`}, {"meshra_send", `{"to":"claude:one","text":"hi"}`}, {"meshra_request", `{"to":"claude:two","text":"review","parentTaskId":"parent"}`}, {"meshra_task_status", `{"taskId":"task-1"}`}, {"meshra_task_events", `{"taskId":"task-1","after":0}`}, {"meshra_task_cancel", `{"taskId":"task-1"}`},
	} {
		result := rpc(t, h, 2, tc.name, tc.args)
		if !bytes.Contains(result["result"], []byte(`"isError":false`)) {
			t.Fatalf("%s: %s", tc.name, result["result"])
		}
	}
	if len(f.sent) != 2 || f.sent[1].Type != "request" || f.sent[1].From != "" || f.callers[1].AgentID != "omp:origin" {
		t.Fatalf("wrong request principal: %+v %+v", f.sent, f.callers)
	}
	result := rpc(t, h, 3, "meshra_send", `{"to":"claude:one","text":"hi","from":"omp:evil"}`)
	if !bytes.Contains(result["result"], []byte(`"isError":true`)) || len(f.sent) != 2 {
		t.Fatal("MCP impersonation accepted")
	}
	result = rpc(t, h, 4, "meshra_task_status", `{"taskId":"private"}`)
	if !bytes.Contains(result["result"], []byte(`"isError":true`)) {
		t.Fatal("service authorization error lost")
	}
	w = httpCall(t, h, "GET", "/mcp", "agent", "", nil)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	w = httpCall(t, h, "POST", "/mcp", "agent", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "future"})
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
