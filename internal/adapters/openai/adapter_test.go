package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

type fixture struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	heads  []http.Header
	calls  int
}

func newFixture(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, call int)) *fixture {
	t.Helper()
	f := &fixture{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, MaxResponseBytes+1))
		f.mu.Lock()
		f.bodies = append(f.bodies, raw)
		f.heads = append(f.heads, r.Header.Clone())
		f.calls++
		call := f.calls - 1
		f.mu.Unlock()
		handler(w, r, call)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fixture) request(t *testing.T, index int) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.bodies) {
		t.Fatalf("request %d was never sent", index)
	}
	var request recordedRequest
	if err := json.Unmarshal(f.bodies[index], &request); err != nil {
		t.Fatalf("request %d is not valid JSON: %v", index, err)
	}
	return request
}

func (f *fixture) header(t *testing.T, index int) http.Header {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.heads) {
		t.Fatalf("request %d was never sent", index)
	}
	return f.heads[index]
}

type recordedRequest struct {
	Model    string           `json:"model"`
	Messages []recordedMsg    `json:"messages"`
	Tools    []toolDefinition `json:"tools"`
	Stream   bool             `json:"stream"`
}
type recordedMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type fakeExecutor struct {
	mu         sync.Mutex
	definition model.ToolDefinition
	result     model.ToolResult
	err        error
	calls      []model.ToolCall
}

func (f *fakeExecutor) Definitions() []model.ToolDefinition {
	return []model.ToolDefinition{f.definition}
}
func (f *fakeExecutor) Call(ctx context.Context, call model.ToolCall) (model.ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.result, f.err
}
func (f *fakeExecutor) executed() []model.ToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.ToolCall(nil), f.calls...)
}

type recorder struct {
	mu     sync.Mutex
	events map[string][]string
}

func newRecorder() *recorder { return &recorder{events: map[string][]string{}} }
func (r *recorder) emit(kind, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[kind] = append(r.events[kind], data)
}
func (r *recorder) event(t *testing.T, kind string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	values := r.events[kind]
	if len(values) == 0 {
		t.Fatalf("no %s event was emitted", kind)
	}
	return values[len(values)-1]
}
func (r *recorder) count(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events[kind])
}

func testAgent() model.Agent {
	return model.Agent{ID: "openai:agent-1", Harness: "openai", Name: "agent-1", Config: model.Config{Provider: "endpoint-1", Model: "test-model"}}
}

func testRuntime(t *testing.T, cfg Config) model.Runtime {
	t.Helper()
	adapter := New(func(context.Context, string) (Config, error) { return cfg, nil })
	runtime, err := adapter.Factory(context.Background(), testAgent(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

func workspaceTool() model.ToolDefinition {
	return model.ToolDefinition{Name: "meshra_workspace_read", Description: "Read a scoped file", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string","minLength":1,"maxLength":64}}}`)}
}

func toolReply(id, name, arguments string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-%s","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, id, id, name, arguments)
}

func textReply(text string) string {
	body, _ := json.Marshal(text)
	return fmt.Sprintf(`{"id":"chatcmpl-final","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`, body)
}

func TestToolRoundTripDispatchesThroughExecutor(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "file contents"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		if call == 0 {
			fmt.Fprint(w, toolReply("call-1", "meshra_workspace_read", `{"path":"notes.md"}`))
			return
		}
		fmt.Fprint(w, textReply("done"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, APIKey: "fixture-key", SupportsTools: true})
	events := newRecorder()
	result, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{ID: "task-1", Text: "read notes"}, Tools: executor}, events.emit)
	if err != nil {
		t.Fatal(err)
	}
	if result != "done" {
		t.Fatalf("unexpected result %q", result)
	}
	calls := executor.executed()
	if len(calls) != 1 {
		t.Fatalf("expected one tool execution, got %d", len(calls))
	}
	if calls[0].Name != "meshra_workspace_read" || calls[0].ID != "call-1" {
		t.Fatalf("unexpected tool call %+v", calls[0])
	}
	var arguments map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &arguments); err != nil {
		t.Fatal(err)
	}
	if arguments["path"] != "notes.md" {
		t.Fatalf("unexpected tool arguments %s", calls[0].Arguments)
	}
	second := f.request(t, 1)
	if len(second.Messages) != 3 {
		t.Fatalf("expected user, assistant and tool messages, got %d", len(second.Messages))
	}
	if second.Messages[1].Role != "assistant" || len(second.Messages[1].ToolCalls) != 1 {
		t.Fatalf("the assistant tool call was not returned to the model: %+v", second.Messages[1])
	}
	if second.Messages[2].Role != "tool" || second.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("the tool result was not returned to the model: %+v", second.Messages[2])
	}
	if !strings.Contains(string(second.Messages[2].Content), "file contents") {
		t.Fatalf("the tool result text was not delivered: %s", second.Messages[2].Content)
	}
	if len(second.Tools) != 1 || second.Tools[0].Function.Name != "meshra_workspace_read" {
		t.Fatalf("the admitted catalog was not offered: %+v", second.Tools)
	}
	if second.Stream {
		t.Fatal("the adapter must request a non-streaming completion")
	}
	if second.Model != "test-model" {
		t.Fatalf("unexpected model %q", second.Model)
	}
}

func TestUnknownToolIsNotExecuted(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "never"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			fmt.Fprint(w, toolReply("call-1", "meshra_workspace_delete_everything", `{}`))
			return
		}
		fmt.Fprint(w, textReply("stopped"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	result, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != "stopped" {
		t.Fatalf("unexpected result %q", result)
	}
	if len(executor.executed()) != 0 {
		t.Fatal("an unknown tool name reached the executor")
	}
	second := f.request(t, 1)
	if !strings.Contains(string(second.Messages[2].Content), "unknown tool") {
		t.Fatalf("the refusal was not reported to the model: %s", second.Messages[2].Content)
	}
}

func TestInvalidArgumentsAreNotExecuted(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "never"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			// The required field is missing and an undeclared field is present.
			fmt.Fprint(w, toolReply("call-1", "meshra_workspace_read", `{"other":"x"}`))
			return
		}
		fmt.Fprint(w, textReply("stopped"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, nil); err != nil {
		t.Fatal(err)
	}
	if len(executor.executed()) != 0 {
		t.Fatal("invalid tool arguments reached the executor")
	}
	second := f.request(t, 1)
	if !strings.Contains(string(second.Messages[2].Content), "invalid tool arguments") {
		t.Fatalf("the refusal was not reported to the model: %s", second.Messages[2].Content)
	}
}

func TestUsageTotalsAggregate(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "ok"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			fmt.Fprint(w, `{"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"meshra_workspace_read","arguments":"{\"path\":\"a\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`)
			return
		}
		fmt.Fprint(w, `{"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":20,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`)
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	events := newRecorder()
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, events.emit); err != nil {
		t.Fatal(err)
	}
	var usage struct {
		Input  *int64 `json:"inputTokens"`
		Output *int64 `json:"outputTokens"`
		Cached *int64 `json:"cachedInputTokens"`
		Model  string `json:"model"`
	}
	if err := json.Unmarshal([]byte(events.event(t, "usage")), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.Input == nil || *usage.Input != 30 {
		t.Fatalf("input tokens were not aggregated: %+v", usage.Input)
	}
	if usage.Output == nil || *usage.Output != 12 {
		t.Fatalf("output tokens were not aggregated: %+v", usage.Output)
	}
	if usage.Cached == nil || *usage.Cached != 7 {
		t.Fatalf("cached tokens were not aggregated: %+v", usage.Cached)
	}
	if usage.Model != "test-model" {
		t.Fatalf("the reported model was not attributed: %q", usage.Model)
	}
}

func TestUnreportedUsageStaysUnknown(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		fmt.Fprint(w, `{"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"done"}}]}`)
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL})
	events := newRecorder()
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, events.emit); err != nil {
		t.Fatal(err)
	}
	if events.count("usage") != 1 {
		t.Fatalf("the reported model attribution was not recorded: %d usage events", events.count("usage"))
	}
	var usage map[string]any
	if err := json.Unmarshal([]byte(events.event(t, "usage")), &usage); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"inputTokens", "outputTokens", "cachedInputTokens"} {
		if _, ok := usage[field]; ok {
			t.Fatalf("an unreported usage value was recorded as %v: %s", usage[field], events.event(t, "usage"))
		}
	}
}

func TestOversizedResponseIsBounded(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		chunk := strings.Repeat("a", 1024*1024)
		for range 9 {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL})
	_, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatalf("an oversized response was not refused: %v", err)
	}
}

func TestResponseWithoutChoicesFails(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		fmt.Fprint(w, `{"model":"test-model","choices":[]}`)
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, nil); err == nil {
		t.Fatal("a response without choices must fail the turn")
	}
}

func TestInvalidJSONResponseFails(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		fmt.Fprint(w, `{"choices":[`)
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, nil); err == nil {
		t.Fatal("an invalid JSON response must fail the turn")
	}
}

func TestCancellationStopsPromptly(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	runtime := testRuntime(t, Config{BaseURL: f.server.URL})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Run(ctx, model.Turn{Task: model.Task{Text: "go"}}, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled turn must fail")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected cancellation error %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled turn did not stop promptly")
	}
}

func TestFailedRequestIsNotReplayed(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"provider unavailable"}}`)
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, nil); err == nil {
		t.Fatal("a failed request must fail the turn")
	}
	if f.count() != 1 {
		t.Fatalf("an uncertain request was replayed %d times", f.count())
	}
}

func TestToolCallLimitIsEnforced(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "ok"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		fmt.Fprint(w, toolReply(fmt.Sprintf("call-%d", call), "meshra_workspace_read", `{"path":"a"}`))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	_, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, nil)
	if err == nil || !strings.Contains(err.Error(), "tool call limit") {
		t.Fatalf("the tool call limit was not enforced: %v", err)
	}
	if len(executor.executed()) != MaxToolCalls {
		t.Fatalf("expected %d tool executions, got %d", MaxToolCalls, len(executor.executed()))
	}
}

func TestImageToolResultRequiresDeclaredSupport(t *testing.T) {
	image := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	for _, tc := range []struct {
		name    string
		images  bool
		expects string
	}{
		{name: "declared", images: true, expects: "image_url"},
		{name: "undeclared", images: false, expects: "does not declare image support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "image", MimeType: "image/png", Data: image}}}}
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
				if call == 0 {
					fmt.Fprint(w, toolReply("call-1", "meshra_workspace_read", `{"path":"a"}`))
					return
				}
				fmt.Fprint(w, textReply("done"))
			})
			runtime := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true, SupportsImages: tc.images})
			if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, nil); err != nil {
				t.Fatal(err)
			}
			second := f.request(t, 1)
			if !strings.Contains(string(second.Messages[2].Content), tc.expects) {
				t.Fatalf("unexpected image delivery: %s", second.Messages[2].Content)
			}
		})
	}
}

func TestImageToolResultOverFrameBoundIsRefused(t *testing.T) {
	image := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", MaxFrameBytes+1024)))
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "image", MimeType: "image/png", Data: image}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			fmt.Fprint(w, toolReply("call-1", "meshra_workspace_read", `{"path":"a"}`))
			return
		}
		fmt.Fprint(w, textReply("done"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true, SupportsImages: true})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, nil); err != nil {
		t.Fatal(err)
	}
	second := f.request(t, 1)
	if !strings.Contains(string(second.Messages[2].Content), "1 MiB") {
		t.Fatalf("an oversized tool result was delivered: %s", second.Messages[2].Content)
	}
}

func TestMarkupToolResultIsNotDelivered(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", MimeType: "text/html", Text: "<script>alert(1)</script>"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			fmt.Fprint(w, toolReply("call-1", "meshra_workspace_read", `{"path":"a"}`))
			return
		}
		fmt.Fprint(w, textReply("done"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, nil); err != nil {
		t.Fatal(err)
	}
	second := f.request(t, 1)
	if strings.Contains(string(second.Messages[2].Content), "<script>") {
		t.Fatalf("markup was delivered to the model: %s", second.Messages[2].Content)
	}
}

func TestAPIKeyComesFromConfiguration(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		fmt.Fprint(w, textReply("done"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, APIKey: "fixture-key"})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.header(t, 0).Get("Authorization"); got != "Bearer fixture-key" {
		t.Fatalf("the configured credential was not used: %q", got)
	}
}

func TestCredentialIsNeverReadFromTheEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "environment-secret")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:1/v1")
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		fmt.Fprint(w, textReply("done"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.header(t, 0).Get("Authorization"); got != "" {
		t.Fatalf("the adapter read a credential from the environment: %q", got)
	}
}

func TestToolCallsAreNeverConcurrent(t *testing.T) {
	executor := &concurrencyExecutor{definition: workspaceTool()}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			// Two tool calls in one reply: the adapter dispatches them one at a time.
			fmt.Fprint(w, `{"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"meshra_workspace_read","arguments":"{\"path\":\"a\"}"}},{"id":"call-2","type":"function","function":{"name":"meshra_workspace_read","arguments":"{\"path\":\"b\"}"}}]}}]}`)
			return
		}
		fmt.Fprint(w, textReply("done"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, nil); err != nil {
		t.Fatal(err)
	}
	if executor.peak() != 1 {
		t.Fatalf("tool calls overlapped: %d concurrent dispatches", executor.peak())
	}
	if executor.count() != 2 {
		t.Fatalf("expected two tool executions, got %d", executor.count())
	}
}

type concurrencyExecutor struct {
	mu         sync.Mutex
	definition model.ToolDefinition
	active     int
	highest    int
	total      int
}

func (c *concurrencyExecutor) Definitions() []model.ToolDefinition {
	return []model.ToolDefinition{c.definition}
}
func (c *concurrencyExecutor) Call(ctx context.Context, call model.ToolCall) (model.ToolResult, error) {
	c.mu.Lock()
	c.active++
	c.total++
	if c.active > c.highest {
		c.highest = c.active
	}
	c.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "ok"}}}, nil
}
func (c *concurrencyExecutor) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.highest
}
func (c *concurrencyExecutor) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func TestSessionIdentityIsStableAndFresh(t *testing.T) {
	adapter := New(func(context.Context, string) (Config, error) { return Config{BaseURL: "http://127.0.0.1:1/v1"}, nil })
	agent := testAgent()
	agent.SessionID = "openai-openai-agent-1-abc123"
	bound, err := adapter.Factory(context.Background(), agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if bound.SessionID() != "openai-openai-agent-1-abc123" {
		t.Fatalf("a released identity was replaced: %q", bound.SessionID())
	}
	unbound := testAgent()
	first, err := adapter.Factory(context.Background(), unbound, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := adapter.Factory(context.Background(), unbound, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.SessionID() == "" || second.SessionID() == "" {
		t.Fatal("an unbound agent must still receive a turn identity")
	}
	if first.SessionID() == second.SessionID() {
		t.Fatalf("two runtimes of one agent share an identity: %q", first.SessionID())
	}
}

func TestSuppliedEndpointClientAndURLsAreUsed(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		switch r.URL.Path {
		case "/custom/chat":
			fmt.Fprint(w, textReply("done"))
		case "/custom/models":
			fmt.Fprint(w, `{"data":[{"id":"model-a"}]}`)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			fmt.Fprint(w, textReply("done"))
		}
	})
	used := 0
	client := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		used++
		return f.server.Client().Transport.RoundTrip(request)
	})}
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, ChatCompletionsURL: f.server.URL + "/custom/chat", ModelsURL: f.server.URL + "/custom/models", HTTPClient: client})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if used != 1 {
		t.Fatalf("the supplied client was not used: %d requests", used)
	}
	models, err := ListModels(context.Background(), nil, Config{BaseURL: f.server.URL, ModelsURL: f.server.URL + "/custom/models", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "model-a" {
		t.Fatalf("unexpected model list %+v", models)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (r roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return r(request) }

func TestListModelsAcceptsStringsAndObjects(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("a listing without a credential sent %q", got)
		}
		fmt.Fprint(w, `{"data":["model-b",{"id":"model-a","name":"Model A"},{"model":"model-c"}],"models":[{"id":"model-a"}]}`)
	})
	models, err := ListModels(context.Background(), f.server.Client(), Config{BaseURL: f.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("unexpected model list %+v", models)
	}
	if models[0].ID != "model-a" || models[1].ID != "model-b" || models[2].ID != "model-c" {
		t.Fatalf("the model list is not sorted by identifier: %+v", models)
	}
	if models[0].Name != "Model A" || models[2].Name != "model-c" {
		t.Fatalf("unexpected model names: %+v", models)
	}
}

func TestListModelsResponseIsBounded(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		chunk := strings.Repeat("a", 1024*1024)
		for range 3 {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	})
	if _, err := ListModels(context.Background(), f.server.Client(), Config{BaseURL: f.server.URL}); err == nil || !strings.Contains(err.Error(), "2 MiB") {
		t.Fatalf("an oversized model list was not refused: %v", err)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    string
		rejects bool
	}{
		{raw: "http://127.0.0.1:11434", want: "http://127.0.0.1:11434/v1"},
		{raw: "http://127.0.0.1:11434/", want: "http://127.0.0.1:11434/v1"},
		{raw: "http://127.0.0.1:11434/v1", want: "http://127.0.0.1:11434/v1"},
		{raw: "http://127.0.0.1:11434/v1/", want: "http://127.0.0.1:11434/v1"},
		{raw: "https://api.example.com/openai/v1", want: "https://api.example.com/openai/v1"},
		{raw: "", rejects: true},
		{raw: "ftp://example.com", rejects: true},
		{raw: "http://user:pass@example.com", rejects: true},
		{raw: "http://example.com?token=x", rejects: true},
		{raw: "http://example.com#frag", rejects: true},
	} {
		got, err := NormalizeBaseURL(tc.raw)
		if tc.rejects {
			if err == nil {
				t.Fatalf("%q was accepted as %q", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q was refused: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("%q normalized to %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestNormalizedBaseURLIsUsedForRequests(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		fmt.Fprint(w, textReply("done"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL + "/v1"})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestListModels(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-b"},{"id":"model-a"}]}`)
	})
	models, err := ListModels(context.Background(), f.server.Client(), Config{BaseURL: f.server.URL, APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "model-a" || models[1].ID != "model-b" {
		t.Fatalf("unexpected model list %+v", models)
	}
	if got := f.header(t, 0).Get("Authorization"); got != "Bearer fixture-key" {
		t.Fatalf("the configured credential was not used: %q", got)
	}
}

func TestFactoryRequiresEndpointAndModel(t *testing.T) {
	adapter := New(func(context.Context, string) (Config, error) { return Config{BaseURL: "http://127.0.0.1:1/v1"}, nil })
	agent := testAgent()
	agent.Config.Provider = ""
	if _, err := adapter.Factory(context.Background(), agent, nil); err == nil {
		t.Fatal("an agent without an endpoint identifier must be refused")
	}
	agent = testAgent()
	agent.Config.Model = ""
	if _, err := adapter.Factory(context.Background(), agent, nil); err == nil {
		t.Fatal("an agent without a model must be refused")
	}
}

func TestDraftTurnIsToolFree(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "never"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		fmt.Fprint(w, textReply("draft"))
	})
	adapter := New(func(context.Context, string) (Config, error) {
		return Config{BaseURL: f.server.URL, SupportsTools: true}, nil
	})
	agent := testAgent()
	agent.GenerationDraft = true
	runtime, err := adapter.Factory(context.Background(), agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "draft"}, Tools: executor}, nil); err != nil {
		t.Fatal(err)
	}
	if len(executor.executed()) != 0 {
		t.Fatal("a draft turn executed a tool")
	}
	if len(f.request(t, 0).Tools) != 0 {
		t.Fatal("a draft turn offered tools to the model")
	}
}
