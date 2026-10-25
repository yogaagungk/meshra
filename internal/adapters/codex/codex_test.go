package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/model"
)

func TestCodexTransportHelper(t *testing.T) {
	if os.Getenv("MESHRA_CODEX_HELPER") != "1" {
		return
	}
	mode := os.Getenv("MESHRA_MOCK_MODE")
	if mode == "startup-hang" {
		for {
			time.Sleep(time.Hour)
		}
	}
	session := "thread-" + filepath.Base(os.Getenv("CODEX_HOME"))[:12]
	send := func(v any) { raw, _ := json.Marshal(v); fmt.Println(string(raw)) }
	notify := func(method string, params any) { send(map[string]any{"method": method, "params": params}) }
	response := func(id any, result any) { send(map[string]any{"id": id, "result": result}) }
	initialized := false
	active := false
	record := os.Getenv("MESHRA_CODEX_RECORD")
	var recorder *os.File
	if record != "" {
		recorder, _ = os.Create(record)
		if recorder != nil {
			raw, _ := json.Marshal(map[string]any{"argv": os.Args})
			recorder.Write(append(raw, '\n'))
			defer recorder.Close()
		}
	}
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var cmd struct {
			ID     json.RawMessage            `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
			Result map[string]json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scan.Bytes(), &cmd) != nil {
			os.Exit(21)
		}
		if recorder != nil {
			recorder.Write(append([]byte(scan.Text()), '\n'))
		}
		switch cmd.Method {
		case "initialize":
			if mode == "profile-tools" || mode == "profile-native-enabled" || mode == "profile-old-version" || mode == "profile-no-dynamic" {
				var capabilities struct {
					ExperimentalAPI bool `json:"experimentalApi"`
				}
				if json.Unmarshal(cmd.Params["capabilities"], &capabilities) != nil || !capabilities.ExperimentalAPI {
					os.Exit(30)
				}
				version := "0.163.0"
				if mode == "profile-old-version" {
					version = "0.162.0"
				}
				response(cmd.ID, map[string]any{"userAgent": "meshra/" + version + " (Ubuntu 24.4.0; x86_64) herdr/0.9.5 (meshra; 0.1.0)"})
				continue
			}
			response(cmd.ID, map[string]any{"userAgent": "mock"})
		case "initialized":
			initialized = true
		case "account/rateLimits/read":
			response(cmd.ID, map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 25, "resetsAt": 1792434700}}})
		case "model/list":
			response(cmd.ID, map[string]any{"data": []map[string]any{{"id": "native-id", "model": "catalog-model", "displayName": "Catalog model"}, {"id": "hidden", "hidden": true}}})
		case "config/read":
			config := map[string]any{}
			if mode == "unsafe-config" {
				config["mcp_servers"] = map[string]any{"unsafe": map[string]any{"command": "untrusted"}}
			}
			if mode == "profile-tools" || mode == "profile-native-enabled" || mode == "profile-old-version" || mode == "profile-no-dynamic" {
				features := map[string]any{}
				for _, name := range nativeToolDisable {
					features[name] = mode == "profile-native-enabled"
				}
				config["features"] = features
			}
			response(cmd.ID, map[string]any{"config": config, "origins": map[string]any{}})
		case "thread/start", "thread/resume":
			if !initialized {
				os.Exit(22)
			}
			var sandbox, policy string
			_ = json.Unmarshal(cmd.Params["sandbox"], &sandbox)
			_ = json.Unmarshal(cmd.Params["approvalPolicy"], &policy)
			if sandbox != "read-only" || policy != "on-request" {
				os.Exit(23)
			}
			if mode == "profile-tools" || mode == "profile-native-enabled" {
				var tools []map[string]any
				if json.Unmarshal(cmd.Params["dynamicTools"], &tools) != nil || len(tools) != 1 {
					os.Exit(31)
				}
				if tools[0]["type"] != "function" || tools[0]["name"] != "meshra_history_search" {
					os.Exit(32)
				}
				schema, ok := tools[0]["inputSchema"].(map[string]any)
				if !ok {
					os.Exit(33)
				}
				if schema["type"] != "object" {
					os.Exit(34)
				}
			}
			if mode == "profile-no-dynamic" {
				// An app-server without the dynamic tool catalog refuses the request.
				send(map[string]any{"id": cmd.ID, "error": map[string]any{"code": -32602, "message": "unknown field `dynamicTools`, expected one of: cwd, model"}})
				continue
			}
			if cmd.Method == "thread/resume" {
				_ = json.Unmarshal(cmd.Params["threadId"], &session)
			}
			if mode == "wrong-session" {
				session = "other-thread"
			}
			response(cmd.ID, map[string]any{"thread": map[string]any{"id": session}})
		case "turn/start":
			if mode == "eof" {
				os.Exit(7)
			}
			var sandbox struct {
				Type    string `json:"type"`
				Network bool   `json:"networkAccess"`
			}
			_ = json.Unmarshal(cmd.Params["sandboxPolicy"], &sandbox)
			if sandbox.Type != "readOnly" || sandbox.Network {
				os.Exit(24)
			}
			active = true
			if mode == "stale-notifications" {
				// Resuming a thread can leave earlier-turn notifications ahead of
				// the new turn/start response; they are not the new turn's identity.
				notify("thread/tokenUsage/updated", map[string]any{"threadId": session, "turnId": "previous-turn"})
				notify("item/agentMessage/delta", map[string]any{"threadId": session, "turnId": "previous-turn", "itemId": "old-message", "delta": "stale text"})
				notify("turn/completed", map[string]any{"threadId": session, "turn": map[string]any{"id": "previous-turn", "status": "completed"}})
			}
			notify("turn/started", map[string]any{"threadId": session, "turn": map[string]any{"id": "turn-1", "status": "inProgress"}})
			// Notifications may race the turn/start response and must not get dropped.
			notify("item/agentMessage/delta", map[string]any{"threadId": session, "turnId": "turn-1", "itemId": "message-1", "delta": "hel"})
			response(cmd.ID, map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress"}})
			if mode == "cancel" {
				continue
			}
			if mode == "error" {
				notify("turn/completed", map[string]any{"threadId": session, "turn": map[string]any{"id": "turn-1", "status": "failed", "error": map[string]any{"message": "model access denied"}}})
				continue
			}
			if mode == "request-error" {
				send(map[string]any{"id": cmd.ID, "error": map[string]any{"code": -32000, "message": "turn rejected"}})
				continue
			}
			if mode == "profile-tools" || mode == "profile-native-enabled" {
				// The server asks this client to run the registered catalog tool.
				send(map[string]any{"id": 200, "method": "item/tool/call", "params": map[string]any{"threadId": session, "turnId": "turn-1", "callId": "call-1", "tool": "meshra_history_search", "arguments": map[string]any{"query": "meshra"}}})
				// A stale turn, an unknown tool and another thread must be refused.
				send(map[string]any{"id": 201, "method": "item/tool/call", "params": map[string]any{"threadId": session, "turnId": "previous-turn", "callId": "call-2", "tool": "meshra_history_search", "arguments": map[string]any{"query": "meshra"}}})
				send(map[string]any{"id": 202, "method": "item/tool/call", "params": map[string]any{"threadId": session, "turnId": "turn-1", "callId": "call-3", "tool": "meshra_unknown_tool", "arguments": map[string]any{}}})
				send(map[string]any{"id": 203, "method": "item/tool/call", "params": map[string]any{"threadId": "other-thread", "turnId": "turn-1", "callId": "call-4", "tool": "meshra_history_search", "arguments": map[string]any{"query": "meshra"}}})
				continue
			}
			send(map[string]any{"id": 100, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": session, "turnId": "turn-1", "itemId": "command-1", "command": "rm -rf example"}})
		case "turn/interrupt":
			if !active {
				os.Exit(25)
			}
			response(cmd.ID, map[string]any{})
			notify("turn/completed", map[string]any{"threadId": session, "turn": map[string]any{"id": "turn-1", "status": "interrupted"}})
		case "":
			if mode == "profile-tools" || mode == "profile-native-enabled" {
				raw, _ := json.Marshal(cmd.Result)
				switch string(cmd.ID) {
				case "200":
					var result struct {
						ContentItems []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"contentItems"`
						Success bool `json:"success"`
					}
					if json.Unmarshal(raw, &result) != nil || !result.Success || len(result.ContentItems) != 1 || result.ContentItems[0].Type != "inputText" || result.ContentItems[0].Text != "found" {
						os.Exit(40)
					}
					continue
				case "201", "202", "203":
					var result struct {
						ContentItems []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"contentItems"`
						Success bool `json:"success"`
					}
					if json.Unmarshal(raw, &result) != nil || result.Success || len(result.ContentItems) != 1 || result.ContentItems[0].Type != "inputText" {
						os.Exit(41)
					}
					if string(cmd.ID) == "203" {
						notify("item/agentMessage/delta", map[string]any{"threadId": session, "turnId": "turn-1", "itemId": "message-1", "delta": "lo"})
						notify("item/completed", map[string]any{"threadId": session, "turnId": "turn-1", "item": map[string]any{"id": "message-1", "type": "agentMessage", "text": "hello"}})
						notify("turn/completed", map[string]any{"threadId": session, "turn": map[string]any{"id": "turn-1", "status": "completed"}})
					}
					continue
				}
			}
			if string(cmd.ID) == "100" {
				var decision string
				_ = json.Unmarshal(cmd.Result["decision"], &decision)
				if decision != "decline" {
					os.Exit(26)
				}
				send(map[string]any{"id": 101, "method": "item/permissions/requestApproval", "params": map[string]any{"threadId": session, "turnId": "turn-1", "itemId": "permission-1"}})
			} else if string(cmd.ID) == "101" {
				var permissions map[string]any
				if json.Unmarshal(cmd.Result["permissions"], &permissions) != nil || len(permissions) != 0 {
					os.Exit(27)
				}
				if mode == "usage" {
					notify("thread/tokenUsage/updated", map[string]any{"threadId": session, "turnId": "turn-1", "tokenUsage": map[string]any{"modelContextWindow": 1000, "last": map[string]any{"inputTokens": 120, "outputTokens": 40, "cachedInputTokens": 80, "totalTokens": 160}}})
				}
				// A different thread's data is never attributed to this agent.
				notify("item/agentMessage/delta", map[string]any{"threadId": "unowned-thread", "turnId": "turn-1", "itemId": "other", "delta": "must not appear"})
				notify("item/agentMessage/delta", map[string]any{"threadId": session, "turnId": "turn-1", "itemId": "message-1", "delta": "lo"})
				notify("item/completed", map[string]any{"threadId": session, "turnId": "turn-1", "item": map[string]any{"id": "message-1", "type": "agentMessage", "text": "hello"}})
				notify("turn/completed", map[string]any{"threadId": session, "turn": map[string]any{"id": "turn-1", "status": "completed"}})
			}
		}
	}
	os.Exit(0)
}
func mockCodex(t *testing.T, mode string) {
	t.Helper()
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec " + strconv.Quote(executable) + " -test.run '^TestCodexTransportHelper$' -- \"$@\"\n"
	if err = os.WriteFile(filepath.Join(root, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MESHRA_CODEX_HELPER", "1")
	t.Setenv("MESHRA_MOCK_MODE", mode)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
}
func testAgent(t *testing.T, name string) model.Agent {
	return model.Agent{ID: "codex:" + name, Cwd: t.TempDir()}
}
func TestCompletionApprovalAndEarlyNotifications(t *testing.T) {
	mockCodex(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events := []string{}
	emit := func(kind, data string) { events = append(events, kind+":"+data) }
	rt, err := New().Factory(ctx, testAgent(t, "one"), emit)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	got, err := rt.Run(ctx, model.Turn{Task: model.Task{ID: "task-1", Text: "hello"}}, emit)
	if err != nil || got != "hello" {
		t.Fatalf("result %q, error %v", got, err)
	}
	if len(events) != 5 || events[0] != "session:"+rt.SessionID() || events[1] != "delta:hel" || !strings.HasPrefix(events[2], "approval_required:") || !strings.HasPrefix(events[3], "approval_required:") || events[4] != "delta:lo" {
		t.Fatalf("events: %v", events)
	}
}

func TestStartResponseOwnsTurnDespiteStaleNotifications(t *testing.T) {
	mockCodex(t, "stale-notifications")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := New().Factory(ctx, testAgent(t, "resumed"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	var deltas strings.Builder
	got, err := rt.Run(ctx, model.Turn{Task: model.Task{Text: "new turn"}}, func(kind, data string) {
		if kind == "delta" {
			deltas.WriteString(data)
		}
	})
	if err != nil || got != "hello" || deltas.String() != "hello" {
		t.Fatalf("stale turn contaminated current response: result=%q deltas=%q err=%v", got, deltas.String(), err)
	}
}
func TestErrorsEOFAndCancellation(t *testing.T) {
	for _, mode := range []string{"error", "request-error", "eof", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			mockCodex(t, mode)
			lifetime, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			rt, err := New().Factory(lifetime, testAgent(t, "one"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			ctx, cancel := context.WithTimeout(lifetime, 100*time.Millisecond)
			defer cancel()
			if _, err = rt.Run(ctx, model.Turn{Task: model.Task{Text: "hello"}}, nil); err == nil {
				t.Fatal("expected failure")
			}
			if mode == "error" && !strings.Contains(err.Error(), "model access denied") {
				t.Fatal(err)
			}
			if mode == "cancel" && ctx.Err() == nil {
				t.Fatalf("expected context cancellation, got %v", err)
			}
		})
	}
}
func TestIndependentNativeOwnershipAndResume(t *testing.T) {
	mockCodex(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := testAgent(t, "one")
	b := testAgent(t, "two")
	first, err := New().Factory(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Factory(ctx, b, nil)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	defer second.Close()
	if first.SessionID() == second.SessionID() {
		t.Fatal("identities share native session")
	}
	if duplicate, err := New().Factory(ctx, a, nil); err == nil {
		duplicate.Close()
		t.Fatal("duplicate native owner")
	}
	a.SessionID = first.SessionID()
	first.Close()
	resumed, err := New().Factory(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.SessionID() != a.SessionID {
		t.Fatal("resume changed session")
	}
	resumed.Close()
	t.Setenv("MESHRA_MOCK_MODE", "wrong-session")
	if wrong, err := New().Factory(ctx, a, nil); err == nil {
		wrong.Close()
		t.Fatal("accepted wrong session")
	}
}
func TestRealSmoke(t *testing.T) {
	if os.Getenv("MESHRA_REAL_CODEX") != "1" {
		t.Skip("opt in with MESHRA_REAL_CODEX=1; uses native authentication and may incur costs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := testAgent(t, "smoke-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	a.Config = model.Config{Model: os.Getenv("MESHRA_CODEX_MODEL"), Provider: os.Getenv("MESHRA_CODEX_PROVIDER")}
	rt, err := New().Factory(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	result, err := rt.Run(ctx, model.Turn{Task: model.Task{ID: "real-smoke", Text: "Reply with exactly MESHRA_OK, using no tools."}}, nil)
	if err != nil || !strings.Contains(result, "MESHRA_OK") {
		t.Fatalf("result %q, error %v", result, err)
	}
	a.SessionID = rt.SessionID()
	rt.Close()
	resumed, err := New().Factory(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.SessionID() != a.SessionID {
		t.Fatal("resume identity changed")
	}
}

func TestRejectsExternalToolConfiguration(t *testing.T) {
	mockCodex(t, "unsafe-config")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if rt, err := New().Factory(ctx, testAgent(t, "one"), nil); err == nil {
		rt.Close()
		t.Fatal("accepted external MCP server outside read-only sandbox")
	} else if !strings.Contains(err.Error(), "mcp_servers") {
		t.Fatal(err)
	}
}

func TestFactoryStartupCancellation(t *testing.T) {
	mockCodex(t, "startup-hang")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if rt, err := New().Factory(ctx, testAgent(t, "one"), nil); err == nil {
		rt.Close()
		t.Fatal("startup without initialize response succeeded")
	}
}

func TestChatRuntimeAlongsideDurableIdentity(t *testing.T) {
	mockCodex(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent := testAgent(t, "chat")
	durable, err := New().Factory(ctx, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	session := durable.SessionID()
	agent.Ephemeral = true
	for i := 0; i < 3; i++ {
		chat, err := New().Factory(ctx, agent, nil)
		if err != nil {
			t.Fatal("fresh chat could not start alongside idle durable session", err)
		}
		answer, runErr := chat.Run(ctx, model.Turn{Task: model.Task{ID: "chat-task", Text: "hello"}}, func(string, string) {})
		closeErr := chat.Close()
		if runErr != nil || answer != "hello" || closeErr != nil {
			t.Fatal(answer, runErr, closeErr)
		}
	}
	if durable.SessionID() != session {
		t.Fatal("chat replaced durable session")
	}
	root, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	leftovers, err := filepath.Glob(filepath.Join(root, "meshra", "runtimes", "codex", "chat-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatal("temporary native state retained", leftovers, err)
	}
}

func TestNativeModelCatalogProtocol(t *testing.T) {
	mockCodex(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := New().Factory(ctx, testAgent(t, "catalog"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	items, err := rt.(model.ModelCatalog).Models(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "catalog-model" {
		t.Fatal(items, err)
	}
}

func TestGenerationDraftFailsClosedBeforeNativeStartup(t *testing.T) {
	// No executable can start here. A draft must return the specific unavailable
	// error, not fall through to the ordinary read-only factory or spawn a turn.
	t.Setenv("PATH", t.TempDir())
	a := testAgent(t, "draft")
	a.GenerationDraft, a.Ephemeral = true, true
	adapter := New()
	if adapter.Capabilities["toolFreeDraft"] {
		t.Fatal("Codex advertised an unenforceable tool-free mode")
	}
	rt, err := adapter.Factory(context.Background(), a, nil)
	if rt != nil {
		rt.Close()
		t.Fatal("Codex draft started without native deny-all support")
	}
	var unavailable *model.Error
	if !errors.As(err, &unavailable) || unavailable.Status != 503 || !strings.Contains(unavailable.Message, "empty native tool catalog") {
		t.Fatalf("expected actionable unavailable error, got %v", err)
	}
}

// fakeBroker is a stand-in for the Meshra tool broker. It records every call
// the adapter makes so a test can prove that only an authorized call ran.
type fakeBroker struct {
	mu          sync.Mutex
	definitions []model.ToolDefinition
	calls       []model.ToolCall
	result      model.ToolResult
	err         error
}

func (f *fakeBroker) Definitions() []model.ToolDefinition { return f.definitions }
func (f *fakeBroker) Call(ctx context.Context, call model.ToolCall) (model.ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.err != nil {
		return model.ToolResult{}, f.err
	}
	return f.result, nil
}
func (f *fakeBroker) recorded() []model.ToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.ToolCall(nil), f.calls...)
}

func profileCatalog() []model.ToolDefinition {
	return []model.ToolDefinition{{Name: "meshra_history_search", Description: "search", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}}
}

func recordedFrames(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frames := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var frame map[string]any
		if err = json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func TestProfileBrokerCatalogServesToolsAndRefusesWrongCalls(t *testing.T) {
	mockCodex(t, "profile-tools")
	record := filepath.Join(t.TempDir(), "frames.jsonl")
	t.Setenv("MESHRA_CODEX_RECORD", record)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	definitions := profileCatalog()
	agent := testAgent(t, "profile")
	agent.ToolDefinitions = definitions
	broker := &fakeBroker{definitions: definitions, result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "found"}}}}
	rt, err := New().Factory(ctx, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	got, err := rt.Run(ctx, model.Turn{Task: model.Task{ID: "task-1", Text: "hi"}, Tools: broker}, nil)
	if err != nil || got != "hello" {
		t.Fatalf("result %q, error %v", got, err)
	}
	calls := broker.recorded()
	if len(calls) != 1 {
		t.Fatalf("broker calls %d, want exactly the authorized call", len(calls))
	}
	if calls[0].Name != "meshra_history_search" || calls[0].ID != "call-1" {
		t.Fatalf("unexpected broker call %+v", calls[0])
	}
	if string(calls[0].Arguments) != `{"query":"meshra"}` {
		t.Fatalf("unexpected broker arguments %s", calls[0].Arguments)
	}
	frames := recordedFrames(t, record)
	initialize := map[string]any{}
	started := map[string]any{}
	for _, frame := range frames {
		switch frame["method"] {
		case "initialize":
			initialize = frame["params"].(map[string]any)
		case "thread/start":
			started = frame["params"].(map[string]any)
		}
	}
	capabilities, ok := initialize["capabilities"].(map[string]any)
	if !ok || capabilities["experimentalApi"] != true {
		t.Fatalf("initialize did not opt into the experimental dynamic tool API: %v", initialize)
	}
	tools, ok := started["dynamicTools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("thread/start did not register the broker catalog: %v", started)
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "meshra_history_search" || tool["description"] != "search" {
		t.Fatalf("unexpected registered tool %v", tool)
	}
	schema, ok := tool["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("registered tool lost its input schema: %v", tool)
	}
	cwd, _ := started["cwd"].(string)
	if cwd == "" || cwd == agent.Cwd {
		t.Fatalf("profile thread kept the operator workspace as native working directory: %q", cwd)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil || len(entries) != 0 {
		t.Fatalf("profile native working directory is not an empty private directory: %v %v", entries, err)
	}
	argv := []any{}
	for _, frame := range frames {
		if values, ok := frame["argv"].([]any); ok {
			argv = values
		}
	}
	disabled := map[string]bool{}
	for index, value := range argv {
		if value == "--disable" && index+1 < len(argv) {
			disabled[argv[index+1].(string)] = true
		}
	}
	for _, name := range nativeToolDisable {
		if !disabled[name] {
			t.Fatalf("native feature %q was not disabled for a profile-managed turn", name)
		}
	}
}

func TestProfileTurnWithoutBrokerExecutorIsRefused(t *testing.T) {
	mockCodex(t, "profile-tools")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent := testAgent(t, "profile")
	agent.ToolDefinitions = profileCatalog()
	rt, err := New().Factory(ctx, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	_, err = rt.Run(ctx, model.Turn{Task: model.Task{ID: "task-1", Text: "hi"}}, nil)
	var unavailable *model.Error
	if !errors.As(err, &unavailable) || unavailable.Status != 503 || !strings.Contains(unavailable.Message, "broker executor") {
		t.Fatalf("expected an actionable unavailable error, got %v", err)
	}
}

func TestProfileRefusesWhenNativeToolsStayEnabled(t *testing.T) {
	mockCodex(t, "profile-native-enabled")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent := testAgent(t, "profile")
	agent.ToolDefinitions = profileCatalog()
	rt, err := New().Factory(ctx, agent, nil)
	if rt != nil {
		rt.Close()
		t.Fatal("profile automation started while a native tool surface was still enabled")
	}
	var unavailable *model.Error
	if !errors.As(err, &unavailable) || unavailable.Status != 503 || !strings.Contains(unavailable.Message, "native feature") {
		t.Fatalf("expected an actionable unavailable error, got %v", err)
	}
}

func TestLegacyTurnKeepsNoBrokerSurface(t *testing.T) {
	mockCodex(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt, err := New().Factory(ctx, testAgent(t, "legacy"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	broker := &fakeBroker{definitions: profileCatalog()}
	_, err = rt.Run(ctx, model.Turn{Task: model.Task{ID: "task-1", Text: "hi"}, Tools: broker}, nil)
	var unavailable *model.Error
	if !errors.As(err, &unavailable) || unavailable.Status != 503 {
		t.Fatalf("expected a broker request on a legacy thread to be refused, got %v", err)
	}
	if len(broker.recorded()) != 0 {
		t.Fatal("a legacy thread reached the broker")
	}
}

func TestProfileRefusesAppServerWithoutDynamicToolCatalog(t *testing.T) {
	mockCodex(t, "profile-no-dynamic")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent := testAgent(t, "profile")
	agent.ToolDefinitions = profileCatalog()
	rt, err := New().Factory(ctx, agent, nil)
	if rt != nil {
		rt.Close()
		t.Fatal("profile automation started on an app-server without the dynamic tool catalog")
	}
	var unavailable *model.Error
	if !errors.As(err, &unavailable) || unavailable.Status != 503 || !strings.Contains(unavailable.Message, "dynamic tool catalog") {
		t.Fatalf("expected an actionable unavailable error, got %v", err)
	}
}

func TestProfileRefusesAppServerOlderThanTheDynamicToolCatalog(t *testing.T) {
	mockCodex(t, "profile-old-version")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent := testAgent(t, "profile")
	agent.ToolDefinitions = profileCatalog()
	rt, err := New().Factory(ctx, agent, nil)
	if rt != nil {
		rt.Close()
		t.Fatal("profile automation started on an unverified app-server version")
	}
	var unavailable *model.Error
	if !errors.As(err, &unavailable) || unavailable.Status != 503 || !strings.Contains(unavailable.Message, "0.163.0") {
		t.Fatalf("expected an actionable unavailable error, got %v", err)
	}
}
