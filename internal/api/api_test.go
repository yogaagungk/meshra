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
	deadline    time.Time
	taskReads   int
	cancelCalls int
	admitted    chan struct{}
	denyStatus  bool
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
	if f.denyStatus {
		return model.Agent{}, model.Err(403, "status not allowed")
	}
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
	if f.admitted != nil {
		close(f.admitted)
	}
	return model.Task{ID: "task-1", State: "queued", From: p.AgentID, To: r.To, Text: r.Text, Type: r.Type, Deadline: f.deadline}, nil
}
func (f *fakeService) Task(p model.Principal, id string) (model.Task, error) {
	if id == "private" {
		return model.Task{}, model.Err(403, "task is private")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taskReads++
	return model.Task{ID: id, State: f.taskState, Deadline: f.deadline, Result: "reply"}, nil
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
	return model.Task{ID: id, State: "cancelled"}, nil
}
func (f *fakeService) Retry(p model.Principal, id string) (model.Task, error) {
	return model.Task{ID: id, State: "queued"}, nil
}
func (f *fakeService) Trace(p model.Principal, id string) (model.Trace, error) {
	task, err := f.Task(p, id)
	if err != nil {
		return model.Trace{}, err
	}
	events, err := f.Events(p, id, 0)
	return model.Trace{Task: task, Events: events}, err
}
func (f *fakeService) Metrics(p model.Principal) (model.Metrics, error) {
	return model.Metrics{Queued: 2}, nil
}
func (f *fakeService) Doctor(p model.Principal) (model.Diagnostics, error) {
	return model.Diagnostics{Healthy: true, Checks: []model.Diagnostic{}}, nil
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
	for _, path := range []string{"/v1/agents", "/v1/identity", "/v1/tasks/t/events?follow=true", "/v1/tasks/t/trace", "/v1/metrics", "/v1/doctor", "/mcp", "/unknown"} {
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
func TestHTTPServiceErrorsAndStreamAuthorization(t *testing.T) {
	f := &fakeService{}
	h := New(f)
	w := httpCall(t, h, "GET", "/v1/agents/missing", "agent", "", nil)
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
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"protocolVersion":"2025-11-25"`) || !strings.Contains(w.Body.String(), `"version":"0.2.0"`) {
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
	names := map[string]bool{}
	for _, tool := range listed.Result.Tools {
		names[tool.Name] = true
		if tool.InputSchema["type"] != "object" || tool.InputSchema["properties"] == nil || tool.InputSchema["additionalProperties"] != false {
			t.Fatalf("invalid schema %+v", tool)
		}
	}
	for _, name := range []string{"meshra_agents_list", "meshra_agent_status", "meshra_send", "meshra_request", "meshra_task_status", "meshra_task_events", "meshra_task_cancel", "meshra_agent_register", "meshra_task_trace", "meshra_task_retry", "meshra_metrics", "meshra_doctor"} {
		if !names[name] {
			t.Errorf("missing compatible tool %s", name)
		}
	}
	for _, tc := range []struct{ name, args string }{
		{"meshra_agents_list", `{}`}, {"meshra_agent_status", `{"id":"claude:one"}`}, {"meshra_send", `{"to":"claude:one","text":"hi"}`}, {"meshra_request", `{"to":"claude:two","text":"review","parentTaskId":"parent"}`}, {"meshra_task_status", `{"taskId":"task-1"}`}, {"meshra_task_events", `{"taskId":"task-1","after":0}`}, {"meshra_task_cancel", `{"taskId":"task-1"}`},
		{"meshra_task_trace", `{"taskId":"task-1"}`},
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

func TestRequestWaitRequiresExplicitBound(t *testing.T) {
	f := &fakeService{}
	for _, tc := range []struct{ query, body string }{
		{"wait=invalid", `{"to":"claude:one","text":"hello","timeoutSeconds":1}`},
		{"wait=true", `{"to":"claude:one","text":"hello"}`},
		{"wait=true", `{"to":"claude:one","text":"hello","timeoutSeconds":0}`},
		{"wait=true", `{"to":"claude:one","text":"hello","timeoutSeconds":86401}`},
	} {
		w := httpCall(t, New(f), "POST", "/v1/request?"+tc.query, "agent", tc.body, nil)
		if w.Code != 400 {
			t.Fatalf("invalid wait accepted: %d %s", w.Code, w.Body.String())
		}
	}
	if len(f.sent) != 0 {
		t.Fatal("invalid wait admitted work")
	}
	w := httpCall(t, New(f), "POST", "/v1/request?wait=false", "agent", `{"to":"claude:one","text":"hello"}`, nil)
	if w.Code != 202 || f.taskReads != 0 {
		t.Fatal("normal request no longer asynchronous", w.Code)
	}
}

func TestRequestWaitAuthorizesStatusBeforeAdmission(t *testing.T) {
	f := &fakeService{denyStatus: true}
	w := httpCall(t, New(f), "POST", "/v1/request?wait=true", "agent", `{"to":"claude:one","text":"hello","timeoutSeconds":1}`, nil)
	if w.Code != 403 || len(f.sent) != 0 {
		t.Fatal("wait admitted work without status access", w.Code, w.Body.String())
	}
	w = httpCall(t, New(f), "POST", "/v1/request", "agent", `{"to":"claude:one","text":"hello"}`, nil)
	if w.Code != 202 || len(f.sent) != 1 {
		t.Fatal("async request incorrectly requires status permission", w.Code)
	}
}

func TestRequestWaitTerminalAndPersistedDeadline(t *testing.T) {
	for _, state := range []string{"completed", "failed", "cancelled", "timed_out", "running"} {
		t.Run(state, func(t *testing.T) {
			f := &fakeService{taskState: state, deadline: time.Now().Add(20 * time.Millisecond)}
			// The persisted deadline (which may be capped by a parent or come from
			// a duplicate admission) takes precedence over this new timeout.
			w := httpCall(t, New(f), "POST", "/v1/request?wait=true", "agent", `{"to":"claude:one","text":"hello","timeoutSeconds":86400}`, nil)
			want, waitStatus := 200, "terminal"
			if state == "running" {
				want, waitStatus = 202, "deadline"
			}
			var task model.Task
			if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
				t.Fatal(err)
			}
			if w.Code != want || task.State != state || task.Result != "reply" || w.Header().Get("X-Meshra-Wait-Status") != waitStatus {
				t.Fatalf("%d %s %s", w.Code, w.Header(), w.Body.String())
			}
			if f.cancelCalls != 0 || len(f.sent) != 1 {
				t.Fatal("wait altered durable task")
			}
		})
	}
}

func TestRequestWaitDisconnectDoesNotCancel(t *testing.T) {
	f := &fakeService{taskState: "running", deadline: time.Now().Add(time.Hour), admitted: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/request?wait=true", strings.NewReader(`{"to":"claude:one","text":"hello","timeoutSeconds":3600}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer agent")
	r.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		New(f).ServeHTTP(httptest.NewRecorder(), r)
		close(done)
	}()
	<-f.admitted
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wait ignored disconnect")
	}
	if f.cancelCalls != 0 || len(f.sent) != 1 {
		t.Fatal("disconnect cancelled or lost durable task")
	}
}

func TestOperatorAccessAndTrace(t *testing.T) {
	f := &fakeService{taskState: "completed", events: []model.Event{{ID: 1, TaskID: "t", Type: "completed"}}}
	h := New(f)
	for _, path := range []string{"/v1/metrics", "/v1/doctor"} {
		if w := httpCall(t, h, "GET", path, "agent", "", nil); w.Code != 403 {
			t.Fatal("agent accessed admin endpoint", path, w.Code)
		}
		if w := httpCall(t, h, "GET", path, "admin", "", nil); w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		if w := httpCall(t, h, "POST", path, "admin", "", nil); w.Code != 405 {
			t.Fatal(path, w.Code)
		}
	}
	w := httpCall(t, h, "GET", "/v1/tasks/t/trace", "agent", "", nil)
	var trace model.Trace
	if err := json.Unmarshal(w.Body.Bytes(), &trace); err != nil || w.Code != 200 || trace.Task.ID != "t" || len(trace.Events) != 1 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	if w := httpCall(t, h, "GET", "/v1/tasks/private/trace", "agent", "", nil); w.Code != 403 {
		t.Fatal("trace bypassed task authorization", w.Code)
	}
	result := rpc(t, h, 1, "meshra_task_trace", `{"taskId":"private"}`)
	if !bytes.Contains(result["result"], []byte(`"isError":true`)) {
		t.Fatal("MCP trace bypassed task authorization")
	}
	for _, tc := range []struct{ name, args string }{
		{"meshra_agent_register", `{"harness":"omp","name":"new","cwd":"/tmp"}`},
		{"meshra_task_retry", `{"taskId":"t"}`},
		{"meshra_metrics", `{}`}, {"meshra_doctor", `{}`},
	} {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tc.name, tc.args)
		for _, token := range []string{"agent", "admin"} {
			w := httpCall(t, h, "POST", "/mcp", token, body, nil)
			want := `"isError":false`
			if token == "agent" {
				want = `"isError":true`
			}
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("%s %s: %s", token, tc.name, w.Body.String())
			}
		}
	}
}
