package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/model"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	executable := filepath.Join(dir, "acp-agent")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{ID: "acp-test", Revision: 1, Name: "test agent", Executable: executable, Enabled: true, CWD: dir}
}

type staticLauncher struct{ transport Transport }

func (l staticLauncher) Launch(ctx context.Context, config Config) (Transport, error) {
	return l.transport, nil
}

type recordingApprover struct {
	mu       sync.Mutex
	requests []PermissionRequest
	decision PermissionDecision
	err      error
}

func (a *recordingApprover) ApprovePermission(ctx context.Context, request PermissionRequest) (PermissionDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, request)
	if a.err != nil {
		return PermissionDecision{}, a.err
	}
	return a.decision, nil
}

func (a *recordingApprover) seen() []PermissionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]PermissionRequest, 0, len(a.requests))
	out = append(out, a.requests...)
	return out
}

type fakeExecutor struct {
	mu     sync.Mutex
	calls  []model.ToolCall
	result model.ToolResult
	err    error
}

func (f *fakeExecutor) Definitions() []model.ToolDefinition { return nil }

func (f *fakeExecutor) Call(ctx context.Context, call model.ToolCall) (model.ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.err != nil {
		return model.ToolResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeExecutor) seen() []model.ToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.ToolCall, 0, len(f.calls))
	out = append(out, f.calls...)
	return out
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) emit(kind, data string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, kind+":"+data)
}

func (l *eventLog) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.events))
	out = append(out, l.events...)
	return out
}

func startRuntime(t *testing.T, peer *fakePeer, client *pipeTransport, options Options) (model.Runtime, *eventLog) {
	t.Helper()
	config := testConfig(t)
	if options.Resolve == nil {
		options.Resolve = func(context.Context, string) (Config, error) { return config, nil }
	}
	if options.Launcher == nil {
		options.Launcher = staticLauncher{transport: client}
	}
	log := &eventLog{}
	adapter := New(options)
	runtime, err := adapter.Factory(context.Background(), model.Agent{ID: "acp:test", Harness: "acp", Cwd: config.CWD, Config: model.Config{Provider: "acp-test"}}, log.emit)
	if err != nil {
		t.Fatalf("start the ACP runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime, log
}

func initializePayload(protocol int) map[string]any {
	return map[string]any{"protocolVersion": protocol, "agentCapabilities": map[string]any{}, "authMethods": []any{}}
}

// TestACPRoundTripReturnsAssistantText proves the initialize, session/new and
// session/prompt round trip over the real JSONL framing.
func TestACPRoundTripReturnsAssistantText(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "sess_round_trip"}, nil
		case "session/prompt":
			p.notify("session/update", map[string]any{"sessionId": "sess_round_trip", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m1", "content": map[string]any{"type": "text", "text": "Hello from the peer."}}})
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	runtime, log := startRuntime(t, peer, client, Options{})
	if runtime.SessionID() != "sess_round_trip" {
		t.Fatalf("unexpected native session: %q", runtime.SessionID())
	}
	answer, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "hello"}, MCPURL: "http://127.0.0.1:1/mcp", Token: "secret-turn-token"}, nil)
	if err != nil {
		t.Fatalf("run the ACP turn: %v", err)
	}
	if answer != "Hello from the peer." {
		t.Fatalf("unexpected assistant text: %q", answer)
	}
	events := log.seen()
	if !containsEvent(events, "session:sess_round_trip") {
		t.Fatalf("the native session was not reported: %v", events)
	}
	for _, frame := range peer.frames() {
		if strings.Contains(string(frame), "secret-turn-token") || strings.Contains(string(frame), "/mcp") {
			t.Fatalf("a relay credential or legacy MCP route reached the agent: %s", frame)
		}
	}
}

// TestACPRejectsUnsupportedProtocolVersion proves that a draft protocol
// version is refused instead of being treated as v1.
func TestACPRejectsUnsupportedProtocolVersion(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		if request.Method == "initialize" {
			return initializePayload(2), nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	config := testConfig(t)
	adapter := New(Options{
		Resolve:  func(context.Context, string) (Config, error) { return config, nil },
		Launcher: staticLauncher{transport: client},
	})
	_, err := adapter.Factory(context.Background(), model.Agent{ID: "acp:test", Harness: "acp", Cwd: config.CWD, Config: model.Config{Provider: "acp-test"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported ACP protocol version 2") {
		t.Fatalf("the unsupported protocol version was not refused: %v", err)
	}
}

// TestACPPermissionUnknownOptionIsRejected proves that an unknown or
// persistent permission option is never approved automatically.
func TestACPPermissionUnknownOptionIsRejected(t *testing.T) {
	for _, option := range []map[string]any{
		{"optionId": "maybe", "name": "Maybe", "kind": "allow_maybe"},
		{"optionId": "always", "name": "Always", "kind": "allow_always"},
	} {
		peer, client := newFakePeer()
		approver := &recordingApprover{decision: PermissionDecision{Approved: true, OptionID: option["optionId"].(string)}}
		answers := make(chan rpcMessage, 1)
		peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
			switch request.Method {
			case "initialize":
				return initializePayload(ProtocolVersion), nil
			case "session/new":
				return map[string]any{"sessionId": "sess_permission"}, nil
			case "session/prompt":
				answer := p.request(t, "session/request_permission", map[string]any{
					"sessionId": "sess_permission",
					"toolCall":  map[string]any{"toolCallId": "call_1", "title": "Edit the file", "kind": "edit"},
					"options":   []any{option},
				})
				answers <- answer
				return map[string]any{"stopReason": "end_turn"}, nil
			}
			return nil, &rpcError{Code: -32601, Message: "unsupported method"}
		}
		runtime, _ := startRuntime(t, peer, client, Options{Approve: approver})
		if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "edit it"}}, nil); err != nil {
			t.Fatalf("run the ACP turn: %v", err)
		}
		answer := <-answers
		if answer.Error != nil {
			t.Fatalf("the permission request was not answered: %+v", answer.Error)
		}
		outcome := outcomeOf(t, answer.Result)
		if outcome["outcome"] == "selected" {
			t.Fatalf("an unknown permission option was selected: %v", outcome)
		}
		if len(approver.seen()) != 0 {
			t.Fatalf("an unknown permission option reached the operator approval path: %+v", approver.seen())
		}
		peer.transport.Close()
	}
}

// TestACPPermissionRequiresOneCallApproval proves that a client-owned
// operation needs one explicit operator approval and that a denial is
// answered with a rejection.
func TestACPPermissionRequiresOneCallApproval(t *testing.T) {
	peer, client := newFakePeer()
	approver := &recordingApprover{decision: PermissionDecision{Approved: true, OptionID: "once"}}
	answers := make(chan rpcMessage, 1)
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "sess_approved"}, nil
		case "session/prompt":
			answer := p.request(t, "session/request_permission", map[string]any{
				"sessionId": "sess_approved",
				"toolCall":  map[string]any{"toolCallId": "call_9", "title": "Read the file", "kind": "read"},
				"options": []any{
					map[string]any{"optionId": "once", "name": "Allow once", "kind": "allow_once"},
					map[string]any{"optionId": "never", "name": "Reject", "kind": "reject_once"},
				},
			})
			answers <- answer
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	runtime, _ := startRuntime(t, peer, client, Options{Approve: approver})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "read it"}}, nil); err != nil {
		t.Fatalf("run the ACP turn: %v", err)
	}
	outcome := outcomeOf(t, (<-answers).Result)
	if outcome["outcome"] != "selected" || outcome["optionId"] != "once" {
		t.Fatalf("the approved one-call option was not selected: %v", outcome)
	}
	requests := approver.seen()
	if len(requests) != 1 || requests[0].ToolCallID != "call_9" || requests[0].Kind != "read" {
		t.Fatalf("the operator did not receive the exact operation: %+v", requests)
	}
	peer.transport.Close()

	denied, deniedClient := newFakePeer()
	defer denied.transport.Close()
	denier := &recordingApprover{decision: PermissionDecision{}}
	deniedAnswers := make(chan rpcMessage, 1)
	denied.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "sess_denied"}, nil
		case "session/prompt":
			answer := p.request(t, "session/request_permission", map[string]any{
				"sessionId": "sess_denied",
				"toolCall":  map[string]any{"toolCallId": "call_2", "title": "Run the command", "kind": "execute"},
				"options": []any{
					map[string]any{"optionId": "once", "name": "Allow once", "kind": "allow_once"},
					map[string]any{"optionId": "never", "name": "Reject", "kind": "reject_once"},
				},
			})
			deniedAnswers <- answer
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	deniedRuntime, _ := startRuntime(t, denied, deniedClient, Options{Approve: denier})
	if _, err := deniedRuntime.Run(context.Background(), model.Turn{Task: model.Task{Text: "run it"}}, nil); err != nil {
		t.Fatalf("run the ACP turn: %v", err)
	}
	deniedOutcome := outcomeOf(t, (<-deniedAnswers).Result)
	if deniedOutcome["outcome"] != "selected" || deniedOutcome["optionId"] != "never" {
		t.Fatalf("the denied operation was not rejected: %v", deniedOutcome)
	}
}

// TestACPAgentOwnedExecutionIsRejected proves that an agent-owned execution
// is never granted, even with an offered one-call option.
func TestACPAgentOwnedExecutionIsRejected(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	approver := &recordingApprover{decision: PermissionDecision{Approved: true, OptionID: "once"}}
	answers := make(chan rpcMessage, 1)
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "sess_execute"}, nil
		case "session/prompt":
			answer := p.request(t, "session/request_permission", map[string]any{
				"sessionId": "sess_execute",
				"toolCall":  map[string]any{"toolCallId": "call_3", "title": "Run the command", "kind": "execute"},
				"options":   []any{map[string]any{"optionId": "once", "name": "Allow once", "kind": "allow_once"}},
			})
			answers <- answer
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	runtime, _ := startRuntime(t, peer, client, Options{Approve: approver})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "run it"}}, nil); err != nil {
		t.Fatalf("run the ACP turn: %v", err)
	}
	outcome := outcomeOf(t, (<-answers).Result)
	if outcome["outcome"] == "selected" {
		t.Fatalf("an agent-owned execution was granted: %v", outcome)
	}
	if len(approver.seen()) != 0 {
		t.Fatalf("an agent-owned execution reached the operator approval path: %+v", approver.seen())
	}
}

// TestACPOversizedFrameIsRefused proves that a frame above the bounded size
// ends the connection instead of being buffered or truncated.
func TestACPOversizedFrameIsRefused(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "sess_large"}, nil
		case "session/prompt":
			frame := append([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"pad":"`), []byte(strings.Repeat("x", 2*1024*1024))...)
			frame = append(frame, []byte(`"}}`+"\n")...)
			if err := p.transport.writeRaw(frame); err != nil {
				t.Errorf("write the oversized frame: %v", err)
			}
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	runtime, _ := startRuntime(t, peer, client, Options{})
	_, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "hello"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("the oversized frame was not refused: %v", err)
	}
}

// TestACPCancelledPromptStops proves that a cancelled turn cancels the
// running prompt and answers pending permission requests.
func TestACPCancelledPromptStops(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	release := make(chan struct{})
	answers := make(chan rpcMessage, 1)
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "sess_cancel"}, nil
		case "session/prompt":
			// The turn is cancelled while the agent is still working, so the
			// pending permission request belongs to a cancelled turn.
			time.Sleep(400 * time.Millisecond)
			answer := p.request(t, "session/request_permission", map[string]any{
				"sessionId": "sess_cancel",
				"toolCall":  map[string]any{"toolCallId": "call_4", "title": "Read the file", "kind": "read"},
				"options":   []any{map[string]any{"optionId": "once", "name": "Allow once", "kind": "allow_once"}},
			})
			answers <- answer
			<-release
			return map[string]any{"stopReason": "cancelled"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	approver := &recordingApprover{decision: PermissionDecision{Approved: true, OptionID: "once"}}
	runtime, _ := startRuntime(t, peer, client, Options{Approve: approver})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := runtime.Run(ctx, model.Turn{Task: model.Task{Text: "hello"}}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled prompt did not stop: %v", err)
	}
	if method := peer.awaitNotification(t); method != "session/cancel" {
		t.Fatalf("the agent was not asked to cancel: %q", method)
	}
	answer := <-answers
	if answer.Error != nil {
		t.Fatalf("the pending permission request was not answered: %+v", answer.Error)
	}
	outcome := outcomeOf(t, answer.Result)
	if outcome["outcome"] != "cancelled" {
		t.Fatalf("a cancelled turn did not answer with the cancelled outcome: %v", outcome)
	}
	close(release)
}

// TestACPUnsupportedClientMethodIsAnswered proves that an unsupported client
// method receives an explicit protocol error instead of hanging.
func TestACPUnsupportedClientMethodIsAnswered(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "sess_terminal"}, nil
		case "session/prompt":
			answer := p.request(t, "terminal/create", map[string]any{"sessionId": "sess_terminal", "command": "sh", "args": []any{"-c", "id"}})
			if answer.Error == nil || answer.Error.Code != -32601 {
				t.Errorf("an unsupported client method was not refused: %+v", answer)
			}
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	runtime, _ := startRuntime(t, peer, client, Options{})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "hello"}}, nil); err != nil {
		t.Fatalf("run the ACP turn: %v", err)
	}
}

// Line windows apply to broker text; foreign paths are rejected before dispatch.
func TestACPClientReadUsesTheBroker(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	executor := &fakeExecutor{result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: `{"text":"first\nsecond\nthird"}`}}}}
	answers := make(chan rpcMessage, 1)
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "sess_read"}, nil
		case "session/prompt":
			for _, path := range []string{"/etc/passwd", "/workspace/../outside"} {
				answer := p.request(t, "fs/read_text_file", map[string]any{"sessionId": "sess_read", "path": path})
				if answer.Error == nil || answer.Error.Code != -32602 {
					t.Errorf("foreign workspace read was not rejected: %+v", answer)
				}
			}
			answer := p.request(t, "fs/read_text_file", map[string]any{"sessionId": "sess_read", "path": "/workspace/a.txt", "line": 2, "limit": 1})
			answers <- answer
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	runtime, _ := startRuntime(t, peer, client, Options{})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "read"}, Tools: executor}, nil); err != nil {
		t.Fatalf("run the ACP turn: %v", err)
	}
	answer := <-answers
	if answer.Error != nil {
		t.Fatalf("the file read was refused: %+v", answer.Error)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(answer.Result, &result); err != nil {
		t.Fatalf("the file read result is unreadable: %v", err)
	}
	if result.Content != "second" {
		t.Fatalf("unexpected file content: %q", result.Content)
	}
}

// TestACPWithoutApproverDeniesPermission proves that a missing operator
// approval path denies every permission request.
func TestACPWithoutApproverDeniesPermission(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	answers := make(chan rpcMessage, 1)
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "sess_no_approver"}, nil
		case "session/prompt":
			answer := p.request(t, "session/request_permission", map[string]any{
				"sessionId": "sess_no_approver",
				"toolCall":  map[string]any{"toolCallId": "call_5", "title": "Read the file", "kind": "read"},
				"options":   []any{map[string]any{"optionId": "once", "name": "Allow once", "kind": "allow_once"}},
			})
			answers <- answer
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	runtime, _ := startRuntime(t, peer, client, Options{})
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "read"}}, nil); err != nil {
		t.Fatalf("run the ACP turn: %v", err)
	}
	outcome := outcomeOf(t, (<-answers).Result)
	if outcome["outcome"] == "selected" {
		t.Fatalf("a permission request was granted without an operator: %v", outcome)
	}
}

// TestACPDisabledConfigurationIsRefused proves that a disabled or unusable
// configuration never starts a process.
func TestACPDisabledConfigurationIsRefused(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	config := testConfig(t)
	config.Enabled = false
	adapter := New(Options{
		Resolve:  func(context.Context, string) (Config, error) { return config, nil },
		Launcher: staticLauncher{transport: client},
	})
	_, err := adapter.Factory(context.Background(), model.Agent{ID: "acp:test", Harness: "acp", Cwd: config.CWD, Config: model.Config{Provider: "acp-test"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("the disabled configuration was not refused: %v", err)
	}
}

// TestACPProcessLauncherSpeaksOverTheRepositoryTransport proves that the
// adapter works over the real bounded JSONL process transport with a script
// agent, without any installed ACP CLI.
func TestACPProcessLauncherSpeaksOverTheRepositoryTransport(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "acp-agent.sh")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"sess_process"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_process","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello from the process"}}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	config := Config{ID: "acp-process", Revision: 1, Name: "process agent", Executable: script, Enabled: true, CWD: dir}
	adapter := New(Options{
		Resolve:  func(context.Context, string) (Config, error) { return config, nil },
		Launcher: ProcessLauncher{Identity: "acp-process-test-" + filepath.Base(dir)},
	})
	runtime, err := adapter.Factory(context.Background(), model.Agent{ID: "acp:test", Harness: "acp", Cwd: dir, Config: model.Config{Provider: "acp-process"}}, nil)
	if err != nil {
		t.Fatalf("start the ACP runtime over the process transport: %v", err)
	}
	defer runtime.Close()
	answer, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{Text: "hello"}}, nil)
	if err != nil {
		t.Fatalf("run the ACP turn: %v", err)
	}
	if answer != "hello from the process" {
		t.Fatalf("unexpected assistant text: %q", answer)
	}
	if runtime.SessionID() != "sess_process" {
		t.Fatalf("unexpected native session: %q", runtime.SessionID())
	}
}

// TestACPPresetsMatchTheReviewedInstalledCLIs proves that the presets name
// the reviewed arguments and environment of the installed ACP CLIs.
func TestACPPresetsMatchTheReviewedInstalledCLIs(t *testing.T) {
	gemini, ok := PresetByName("gemini")
	if !ok || len(gemini.Args) != 1 || gemini.Args[0] != "--acp" || gemini.Executables[0] != "gemini" {
		t.Fatalf("unexpected gemini preset: %+v", gemini)
	}
	cursor, ok := PresetByName("cursor")
	if !ok || len(cursor.Args) != 1 || cursor.Args[0] != "acp" || len(cursor.Executables) != 2 || cursor.Executables[0] != "cursor-agent" || cursor.Executables[1] != "agent" {
		t.Fatalf("unexpected cursor preset: %+v", cursor)
	}
	cline, ok := PresetByName("cline")
	if !ok || len(cline.Args) != 1 || cline.Args[0] != "--acp" || cline.Env["CLINE_SESSION_BACKEND_MODE"] != "local" || cline.Env["CLINE_NO_AUTO_UPDATE"] != "1" {
		t.Fatalf("unexpected cline preset: %+v", cline)
	}
	if _, ok = PresetByName("unknown"); ok {
		t.Fatal("an unknown preset was accepted")
	}
	dir := t.TempDir()
	candidate := filepath.Join(dir, "gemini")
	if err := os.WriteFile(candidate, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	located, err := gemini.Locate([]string{dir})
	if err != nil || located != candidate {
		t.Fatalf("the preset did not locate the installed CLI: %q %v", located, err)
	}
	if _, err = gemini.Locate([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("a missing preset CLI was accepted")
	}
}

func TestPresetDiscoveryResolvesInstalledSymlinkWithoutExecution(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "package-cli.js")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nprintf launched > \"$0.launched\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "gemini")); err != nil {
		t.Fatal(err)
	}
	preset, _ := PresetByName("gemini")
	found, err := preset.Locate([]string{dir})
	if err != nil || found != target {
		t.Fatalf("installed symlink discovery: %q %v", found, err)
	}
	if _, err = os.Stat(target + ".launched"); !os.IsNotExist(err) {
		t.Fatalf("discovery executed the CLI: %v", err)
	}
	if err = os.Chmod(target, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = preset.Locate([]string{dir}); err == nil {
		t.Fatal("nonexecutable symlink target was advertised as installed")
	}
}

// TestACPProfileManagedAgentRequiresProviderOrigin proves that a
// profile-managed agent without a reviewed provider origin is refused.
func TestACPProfileManagedAgentRequiresProviderOrigin(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	config := testConfig(t)
	adapter := New(Options{
		Resolve:  func(context.Context, string) (Config, error) { return config, nil },
		Launcher: staticLauncher{transport: client},
	})
	_, err := adapter.Factory(context.Background(), model.Agent{ID: "acp:test", Harness: "acp", ProfileManaged: true, Cwd: config.CWD, Config: model.Config{Provider: "acp-test"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "operator-selected provider origin") {
		t.Fatalf("the unreviewed provider egress was not refused: %v", err)
	}
}

// TestACPDecodeStoredMatchesTheStoredConfiguration proves that a stored ACP
// configuration document decodes into the adapter configuration without a
// field-by-field mapping.
func TestACPDecodeStoredMatchesTheStoredConfiguration(t *testing.T) {
	stored := []byte(`{"id":"acp-1","revision":3,"name":"Gemini","preset":"gemini","executable":"/usr/bin/gemini","args":["--acp"],"runtimePaths":["/usr/lib"],"secretEnvRefs":["GEMINI_API_KEY=secret-1"],"readOnlyAuthFiles":["/home/user/.gemini/oauth"],"allowedOrigins":["https://generativelanguage.googleapis.com"],"enabled":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`)
	config, err := DecodeStored(stored)
	if err != nil {
		t.Fatalf("decode the stored configuration: %v", err)
	}
	if config.ID != "acp-1" || config.Revision != 3 || config.Preset != "gemini" || !config.Enabled {
		t.Fatalf("the stored configuration did not decode: %+v", config)
	}
	if len(config.Args) != 1 || config.Args[0] != "--acp" || len(config.AllowedOrigins) != 1 || config.AllowedOrigins[0] != "https://generativelanguage.googleapis.com" {
		t.Fatalf("the stored configuration did not decode: %+v", config)
	}
	if len(config.SecretEnvRefs) != 1 || len(config.ReadOnlyAuthFiles) != 1 || len(config.RuntimePaths) != 1 {
		t.Fatalf("the stored configuration did not decode: %+v", config)
	}
}

func outcomeOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var result struct {
		Outcome map[string]any `json:"outcome"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("the permission outcome is unreadable: %v", err)
	}
	if result.Outcome == nil {
		t.Fatalf("the permission outcome is missing: %s", raw)
	}
	return result.Outcome
}

func containsEvent(events []string, want string) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}
