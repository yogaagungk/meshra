package omp

import (
	"bufio"
	"context"
	"encoding/base64"
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

func TestOMPTransportHelper(t *testing.T) {
	if os.Getenv("MESHRA_OMP_HELPER") != "1" {
		return
	}
	mode := os.Getenv("MESHRA_MOCK_MODE")
	for _, arg := range os.Args {
		if arg == "--version" {
			if mode == "profile-version" {
				fmt.Println("omp/0.0.0")
			} else {
				fmt.Println(hostProtocolVersion)
			}
			os.Exit(0)
		}
	}
	if mode == "startup-hang" {
		for {
			time.Sleep(time.Hour)
		}
	}
	dir := ""
	resumed := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--session-dir=") {
			dir = strings.TrimPrefix(arg, "--session-dir=")
		}
		if strings.HasPrefix(arg, "--resume=") {
			resumed = strings.TrimPrefix(arg, "--resume=")
		}
	}
	if strings.HasPrefix(mode, "draft-") {
		for _, flag := range []string{"--no-tools", "--no-extensions", "--no-skills", "--no-rules", "--no-lsp", "--no-pty", "--no-title", "--no-ui", "--no-prewalk"} {
			found := false
			for _, arg := range os.Args {
				if arg == flag {
					found = true
				}
			}
			if !found {
				os.Exit(30)
			}
		}
	}
	session := "native-" + filepath.Base(filepath.Dir(dir))[:12]
	if resumed != "" {
		session = resumed
	}
	if mode == "wrong-session" {
		session = "other-session"
	}
	send := func(v any) { data, _ := json.Marshal(v); fmt.Println(string(data)) }
	versions := []int{1, 2}
	if mode == "profile-protocol" {
		versions = []int{1}
	}
	send(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": versions, "maxFrameBytes": 1048576})
	stateReads := 0
	hostCleared := false
	hostTools := []any{}
	hostSets, prompts := 0, 0
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var cmd map[string]any
		if json.Unmarshal(scan.Bytes(), &cmd) != nil {
			os.Exit(21)
		}
		id, kind := cmd["id"], cmd["type"]
		data := any(map[string]any{})
		if kind == "set_host_tools" {
			tools, ok := cmd["tools"].([]any)
			if !ok || (len(tools) != 0 && !strings.HasPrefix(mode, "profile-")) {
				os.Exit(28)
			}
			hostCleared = true
			hostTools = tools
			hostSets++
			data = map[string]any{"toolNames": []string{}}
			if mode == "draft-dirty-registration" {
				data = map[string]any{"toolNames": []string{"foreign_read"}}
			}
			if mode == "draft-missing-registration" {
				data = map[string]any{}
			}
			if strings.HasPrefix(mode, "profile-") {
				names := make([]string, 0, len(tools))
				for _, tool := range tools {
					d := tool.(map[string]any)
					if len(d) != 3 || d["name"] != "meshra_read" || d["description"] != "Read through the active broker" || d["parameters"] == nil {
						os.Exit(41)
					}
					names = append(names, d["name"].(string))
				}
				data = map[string]any{"toolNames": names}
				if mode == "profile-registration" {
					data = map[string]any{"toolNames": []string{"foreign"}}
				}
			}
		}
		if kind == "get_available_models" {
			data = map[string]any{"models": []map[string]string{{"id": "catalog-model", "name": "Catalog model", "provider": "catalog-provider"}}}
		}
		if kind == "get_state" {
			stateReads++
			data = map[string]any{"sessionId": session, "sessionFile": filepath.Join(dir, session+".jsonl"), "dumpTools": []any{}}
			if strings.HasPrefix(mode, "profile-") {
				data.(map[string]any)["dumpTools"] = hostTools
				if mode == "profile-native" && hostSets > 0 {
					data.(map[string]any)["dumpTools"] = append([]any{map[string]any{"name": "read"}}, hostTools...)
				}
				if mode == "profile-missing" && hostSets > 0 {
					delete(data.(map[string]any), "dumpTools")
				}
				if mode == "profile-schema" && hostSets > 0 {
					data.(map[string]any)["dumpTools"] = []any{map[string]any{"name": "meshra_read", "description": "Read through the active broker", "parameters": map[string]any{"type": "string"}}}
				}
			}
			switch mode {
			case "draft-missing-roster":
				delete(data.(map[string]any), "dumpTools")
			case "draft-null-roster":
				data.(map[string]any)["dumpTools"] = nil
			case "draft-native-tool":
				data.(map[string]any)["dumpTools"] = []any{map[string]any{"name": os.Getenv("MESHRA_MOCK_TOOL")}}
			case "draft-late-tool":
				if stateReads >= 3 {
					data.(map[string]any)["dumpTools"] = []any{map[string]any{"name": "read"}}
				}
			}
			if mode == "context" {
				data.(map[string]any)["contextUsage"] = map[string]any{"tokens": 160, "contextWindow": 1000}
			}
		}
		if kind == "prompt" {
			if strings.HasPrefix(mode, "profile-") {
				prompts++
				if hostSets != prompts+1 || (prompts == 1 && len(hostTools) != 1) || (prompts == 2 && len(hostTools) != 0) {
					os.Exit(42)
				}
				runHostProtocolPeer(send, scan, mode, id, prompts)
				continue
			}
			if strings.HasPrefix(mode, "draft-") && (!hostCleared || stateReads < 3) {
				os.Exit(29)
			}
			if mode == "draft-host-call" {
				send(map[string]any{"type": "host_tool_call", "id": "unsolicited", "toolName": "read", "toolCallId": "call-1", "arguments": map[string]any{"path": "/etc/passwd"}})
				continue
			}
			if mode == "eof" {
				os.Exit(7)
			}
			send(map[string]any{"type": "response", "id": "unrelated", "command": "prompt", "success": false, "error": "must ignore"})
			send(map[string]any{"type": "response", "id": id, "command": kind, "success": true})
			if mode == "cancel" {
				continue
			}
			if mode == "overflow" {
				send(map[string]any{"type": "rpc_frame_error"})
				continue
			}
			if mode == "bad-chunks" {
				send(map[string]any{"type": "rpc_chunk", "chunkId": "bad", "index": 0, "count": 2, "byteLength": 2, "data": "eA=="})
				send(map[string]any{"type": "session_settled"})
				continue
			}
			delta := map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "hello"}}
			if mode == "chunks" {
				raw, _ := json.Marshal(delta)
				middle := len(raw) / 2
				for index, part := range [][]byte{raw[:middle], raw[middle:]} {
					send(map[string]any{"type": "rpc_chunk", "chunkId": "chunk-1", "index": index, "count": 2, "byteLength": len(raw), "data": base64.StdEncoding.EncodeToString(part)})
				}
			} else {
				send(delta)
			}
			send(map[string]any{"type": "agent_end", "isTerminal": true})
			status := "completed"
			if mode == "error" {
				status = "error"
			}
			send(map[string]any{"type": "prompt_result", "id": id, "status": status, "agentInvoked": true, "sessionSettled": false, "error": map[string]any{"message": "provider rejected prompt"}})
			send(map[string]any{"type": "session_settled"})
			continue
		}
		send(map[string]any{"type": "response", "id": id, "command": kind, "success": true, "data": data})
	}
	os.Exit(0)
}

// This subprocess is a deterministic protocol peer, not a native-model smoke
// test. It checks the actual adapter transport and asynchronous broker routing.
func runHostProtocolPeer(send func(any), scan *bufio.Scanner, mode string, promptID any, prompt int) {
	receive := func(id string, wantError bool) {
		if !scan.Scan() {
			os.Exit(50)
		}
		var result struct {
			Type    string                     `json:"type"`
			ID      string                     `json:"id"`
			IsError *bool                      `json:"isError"`
			Result  map[string]json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scan.Bytes(), &result) != nil || result.Type != "host_tool_result" || result.ID != id || result.IsError == nil || *result.IsError != wantError ||
			result.Result["content"] == nil || result.Result["isError"] != nil {
			os.Exit(51)
		}
		var content []model.ToolContent
		if json.Unmarshal(result.Result["content"], &content) != nil || len(content) != 1 || content[0].Type != "text" || content[0].Text == "" {
			os.Exit(52)
		}
		if !wantError && content[0].Text != "broker:"+strings.Replace(id, "host", "call", 1) {
			os.Exit(53)
		}
	}
	call := func(id, callID, name, action string) {
		send(map[string]any{"type": "host_tool_call", "id": id, "toolCallId": callID, "toolName": name, "arguments": map[string]any{"action": action}})
	}
	if prompt == 1 {
		call("before-admission", "call-before", "meshra_read", "fast")
		receive("before-admission", true)
	}
	send(map[string]any{"type": "response", "id": promptID, "command": "prompt", "success": true})
	if prompt == 2 {
		call("after-clear", "call-after-clear", "meshra_read", "fast")
		receive("after-clear", true)
		call("host-1", "call-1", "meshra_read", "fast")
		receive("host-1", true)
	} else {
		switch mode {
		case "profile-wait", "profile-cancel":
			call("host-1", "call-1", "meshra_read", "wait")
			if mode == "profile-cancel" {
				for scan.Scan() {
				}
				os.Exit(0)
			}
			call("host-2", "call-2", "meshra_read", "fast")
			send(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "approval-waiting:"}})
			receive("host-2", false)
			send(map[string]any{"type": "host_tool_cancel", "id": "cancel-wrong", "targetId": "not-pending"})
			send(map[string]any{"type": "host_tool_cancel", "id": "invalid/id", "targetId": "host-1"})
			call("host-3", "call-3", "meshra_read", "still-waiting")
			receive("host-3", false)
			send(map[string]any{"type": "host_tool_cancel", "id": "cancel-right", "targetId": "host-1"})
			call("host-4", "call-4", "meshra_read", "confirm-cancel")
			receive("host-4", false)
		default:
			call("bad-tool", "bad-call-tool", "get_state", "fast")
			receive("bad-tool", true)
			call("bad/id", "bad-call-id", "meshra_read", "fast")
			receive("bad/id", true)
			call("bad-native-call", "bad/id", "meshra_read", "fast")
			receive("bad-native-call", true)
			send(map[string]any{"type": "host_tool_call", "id": "bad-arguments", "toolCallId": "bad-call-arguments", "toolName": "meshra_read", "arguments": nil})
			receive("bad-arguments", true)
			call("host-1", "call-1", "meshra_read", "fast")
			receive("host-1", mode == "profile-tool-error" || mode == "profile-tool-iserror")
			call("host-1", "call-new", "meshra_read", "fast")
			receive("host-1", true)
			call("host-new", "call-1", "meshra_read", "fast")
			receive("host-new", true)
		}
	}
	send(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "hello"}})
	send(map[string]any{"type": "prompt_result", "id": "wrong-prompt", "status": "error", "sessionSettled": true})
	send(map[string]any{"type": "prompt_result", "id": promptID, "status": "completed", "sessionSettled": true})
}

func mockOMP(t *testing.T, mode string) {
	t.Helper()
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec " + strconv.Quote(executable) + " -test.run '^TestOMPTransportHelper$' -- \"$@\"\n"
	if err = os.WriteFile(filepath.Join(root, "omp"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MESHRA_OMP_HELPER", "1")
	t.Setenv("MESHRA_MOCK_MODE", mode)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}
func testAgent(t *testing.T, name string) model.Agent {
	return model.Agent{ID: "omp:" + name, Cwd: t.TempDir()}
}

func TestProtocolCompletionAndChunks(t *testing.T) {
	for _, mode := range []string{"success", "chunks"} {
		t.Run(mode, func(t *testing.T) {
			mockOMP(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			events := []string{}
			runtime, err := New().Factory(ctx, testAgent(t, "one"), func(kind, data string) { events = append(events, kind+":"+data) })
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			got, err := runtime.Run(ctx, model.Turn{Task: model.Task{ID: "task-1", Text: "hello"}}, func(kind, data string) { events = append(events, kind+":"+data) })
			if err != nil || got != "hello" {
				t.Fatalf("got %q, %v", got, err)
			}
			if len(events) != 2 || events[0] != "session:"+runtime.SessionID() || events[1] != "delta:hello" {
				t.Fatalf("events: %v", events)
			}
		})
	}
}
func TestProtocolErrorsAndCancellation(t *testing.T) {
	for _, mode := range []string{"error", "eof", "overflow", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			mockOMP(t, mode)
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
				t.Fatal("expected terminal error")
			}
			if mode == "error" && !strings.Contains(err.Error(), "provider rejected") {
				t.Fatal(err)
			}
			if mode == "cancel" && ctx.Err() == nil {
				t.Fatalf("expected context cancellation, got %v", err)
			}
		})
	}
}
func TestIndependentNativeOwnershipAndResume(t *testing.T) {
	mockOMP(t, "success")
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
		t.Fatal("identities share a native session")
	}
	if duplicate, err := New().Factory(ctx, a, nil); err == nil {
		duplicate.Close()
		t.Fatal("duplicate native session owner admitted")
	}
	a.SessionID = first.SessionID()
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := New().Factory(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.SessionID() != a.SessionID {
		t.Fatal("wrong resumed native session")
	}
	t.Setenv("MESHRA_MOCK_MODE", "wrong-session")
	resumed.Close()
	if wrong, err := New().Factory(ctx, a, nil); err == nil {
		wrong.Close()
		t.Fatal("accepted wrong resumed session")
	}
}

func TestRealSmoke(t *testing.T) {
	if os.Getenv("MESHRA_REAL_OMP") != "1" {
		t.Skip("opt in with MESHRA_REAL_OMP=1; uses native authentication and may incur costs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := testAgent(t, "smoke-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	a.Config = model.Config{Model: os.Getenv("MESHRA_OMP_MODEL"), Provider: os.Getenv("MESHRA_OMP_PROVIDER")}
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

func TestFactoryStartupCancellation(t *testing.T) {
	mockOMP(t, "startup-hang")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if rt, err := New().Factory(ctx, testAgent(t, "one"), nil); err == nil {
		rt.Close()
		t.Fatal("startup without ready succeeded")
	}
}

func TestRejectsInterruptedChunkSequence(t *testing.T) {
	mockOMP(t, "bad-chunks")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rt, err := New().Factory(ctx, testAgent(t, "one"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err = rt.Run(ctx, model.Turn{Task: model.Task{Text: "hello"}}, nil); err == nil || !strings.Contains(err.Error(), "chunk sequence") {
		t.Fatalf("expected invalid chunk sequence, got %v", err)
	}
}

func TestChatRuntimeAlongsideDurableIdentity(t *testing.T) {
	mockOMP(t, "success")
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
	leftovers, err := filepath.Glob(filepath.Join(root, "meshra", "runtimes", "omp", "chat-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatal("temporary native state retained", leftovers, err)
	}
}

func TestNativeModelCatalogProtocol(t *testing.T) {
	mockOMP(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := New().Factory(ctx, testAgent(t, "catalog"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	items, err := rt.(model.ModelCatalog).Models(ctx)
	if err != nil || len(items) != 1 || items[0].Provider != "catalog-provider" {
		t.Fatal(items, err)
	}
}

func TestGenerationDraftRequiresDisposableEmptyWorkspace(t *testing.T) {
	for _, mode := range []string{"durable", "resume", "nonempty"} {
		t.Run(mode, func(t *testing.T) {
			a := testAgent(t, "draft")
			a.GenerationDraft, a.Ephemeral = true, true
			switch mode {
			case "durable":
				a.Ephemeral = false
			case "resume":
				a.SessionID = "existing"
			case "nonempty":
				if err := os.WriteFile(filepath.Join(a.Cwd, "private.txt"), []byte("canary"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			rt, err := New().Factory(context.Background(), a, nil)
			if rt != nil {
				rt.Close()
				t.Fatal("draft started with inherited state")
			}
			var unavailable *model.Error
			if !errors.As(err, &unavailable) || unavailable.Status != 503 {
				t.Fatalf("expected unavailable, got %v", err)
			}
		})
	}
}

func TestGenerationDraftDeniesNativeToolsBeforePrompt(t *testing.T) {
	for _, tool := range []string{"read", "glob", "grep", "bash", "mcp__foreign__read"} {
		t.Run(tool, func(t *testing.T) {
			mockOMP(t, "draft-native-tool")
			t.Setenv("MESHRA_MOCK_TOOL", tool)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a := testAgent(t, "draft")
			a.GenerationDraft, a.Ephemeral = true, true
			rt, err := New().Factory(ctx, a, nil)
			if rt != nil {
				rt.Close()
				t.Fatal("draft accepted a native tool")
			}
			var unavailable *model.Error
			if !errors.As(err, &unavailable) || unavailable.Status != 503 {
				t.Fatalf("expected unavailable, got %v", err)
			}
		})
	}
}

func TestGenerationDraftRejectsUnverifiableRosters(t *testing.T) {
	for _, mode := range []string{"draft-missing-roster", "draft-null-roster", "draft-dirty-registration", "draft-missing-registration"} {
		t.Run(mode, func(t *testing.T) {
			mockOMP(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a := testAgent(t, "draft")
			a.GenerationDraft, a.Ephemeral = true, true
			rt, err := New().Factory(ctx, a, nil)
			if rt != nil {
				rt.Close()
				t.Fatal("draft accepted an unverifiable roster")
			}
			var unavailable *model.Error
			if !errors.As(err, &unavailable) || unavailable.Status != 503 {
				t.Fatalf("expected unavailable, got %v", err)
			}
		})
	}
}

func TestGenerationDraftRechecksRosterAndDeniesHostCalls(t *testing.T) {
	for _, mode := range []string{"draft-success", "draft-late-tool", "draft-host-call"} {
		t.Run(mode, func(t *testing.T) {
			mockOMP(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a := testAgent(t, "draft")
			a.GenerationDraft, a.Ephemeral = true, true
			rt, err := New().Factory(ctx, a, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			answer, err := rt.Run(ctx, model.Turn{Task: model.Task{ID: "draft-task", Text: "Return a profile draft"}, MCPURL: "http://127.0.0.1:1/not-for-the-model"}, nil)
			if mode == "draft-success" {
				if err != nil || answer != "hello" {
					t.Fatalf("draft failed: %q, %v", answer, err)
				}
				return
			}
			var unavailable *model.Error
			if !errors.As(err, &unavailable) || unavailable.Status != 503 || answer != "" {
				t.Fatalf("draft tool denial failed: %q, %v", answer, err)
			}
		})
	}
}

func hostDefinitions() []model.ToolDefinition {
	return []model.ToolDefinition{{Name: "meshra_read", Description: "Read through the active broker", InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}},"required":["action"],"additionalProperties":false}`)}}
}

type protocolBroker struct {
	definitions []model.ToolDefinition
	mode        string
	mu          sync.Mutex
	calls       []model.ToolCall
	waiting     chan struct{}
	cancelled   chan struct{}
}

func newProtocolBroker(mode string) *protocolBroker {
	return &protocolBroker{definitions: hostDefinitions(), mode: mode, waiting: make(chan struct{}), cancelled: make(chan struct{})}
}

func (b *protocolBroker) Definitions() []model.ToolDefinition { return b.definitions }
func (b *protocolBroker) Call(ctx context.Context, call model.ToolCall) (model.ToolResult, error) {
	b.mu.Lock()
	b.calls = append(b.calls, call)
	b.mu.Unlock()
	var args struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return model.ToolResult{}, err
	}
	switch args.Action {
	case "wait":
		close(b.waiting)
		<-ctx.Done()
		close(b.cancelled)
		return model.ToolResult{}, ctx.Err()
	case "fast":
		if b.mode == "profile-wait" {
			select {
			case <-b.waiting:
			case <-ctx.Done():
				return model.ToolResult{}, ctx.Err()
			}
		}
	case "still-waiting":
		select {
		case <-b.cancelled:
			return model.ToolResult{}, errors.New("unrelated cancellation reached the pending tool")
		default:
		}
	case "confirm-cancel":
		select {
		case <-b.cancelled:
		case <-ctx.Done():
			return model.ToolResult{}, ctx.Err()
		}
	}
	if b.mode == "profile-tool-error" {
		return model.ToolResult{}, errors.New("broker rejected execution")
	}
	return model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "broker:" + call.ID}}, IsError: b.mode == "profile-tool-iserror"}, nil
}

func profileAgent(t *testing.T) model.Agent {
	a := testAgent(t, "profile")
	a.ProfileManaged = true
	a.ToolDefinitions = hostDefinitions()
	return a
}

func profileTurn(a model.Agent, rt model.Runtime, broker model.ToolExecutor) model.Turn {
	return model.Turn{Task: model.Task{ID: "task-profile", To: a.ID, DestinationSessionID: rt.SessionID(), Text: "Use the managed catalog"}, Tools: broker}
}

func TestHostToolsProtocolSetClearAndReplay(t *testing.T) {
	for _, mode := range []string{"profile-success", "profile-tool-error", "profile-tool-iserror"} {
		t.Run(mode, func(t *testing.T) {
			mockOMP(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a := profileAgent(t)
			rt, err := New().Factory(ctx, a, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			broker := newProtocolBroker(mode)
			got, err := rt.Run(ctx, profileTurn(a, rt, broker), nil)
			if err != nil || got != "hello" {
				t.Fatalf("first run %q: %v", got, err)
			}
			got, err = rt.Run(ctx, profileTurn(a, rt, nil), nil)
			if err != nil || got != "hello" {
				t.Fatalf("cleared run %q: %v", got, err)
			}
			broker.mu.Lock()
			defer broker.mu.Unlock()
			if len(broker.calls) != 1 || broker.calls[0].ID != "call-1" || broker.calls[0].Name != "meshra_read" || string(broker.calls[0].Arguments) != `{"action":"fast"}` {
				t.Fatalf("unexpected broker dispatch: %+v", broker.calls)
			}
		})
	}
}

func TestHostToolsReaderRemainsResponsiveDuringApproval(t *testing.T) {
	mockOMP(t, "profile-wait")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := profileAgent(t)
	rt, err := New().Factory(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	broker := newProtocolBroker("profile-wait")
	var deltas strings.Builder
	got, err := rt.Run(ctx, profileTurn(a, rt, broker), func(kind, value string) {
		if kind == "delta" {
			deltas.WriteString(value)
		}
	})
	if err != nil || got != "approval-waiting:hello" || deltas.String() != got {
		t.Fatalf("waiting approval stopped frames: %q, %q, %v", got, deltas.String(), err)
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.calls) != 4 {
		t.Fatalf("calls: %+v", broker.calls)
	}
	select {
	case <-broker.cancelled:
	default:
		t.Fatal("targetId did not cancel the pending tool")
	}
}

func TestHostToolsAttemptCancellation(t *testing.T) {
	mockOMP(t, "profile-cancel")
	lifetime, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	a := profileAgent(t)
	rt, err := New().Factory(lifetime, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	broker := newProtocolBroker("profile-cancel")
	ctx, cancel := context.WithTimeout(lifetime, 150*time.Millisecond)
	defer cancel()
	_, err = rt.Run(ctx, profileTurn(a, rt, broker), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
	select {
	case <-broker.cancelled:
	case <-lifetime.Done():
		t.Fatal("broker did not receive cancellation")
	}
}

func TestHostToolsRequireExactActiveTaskSessionCatalog(t *testing.T) {
	for _, mode := range []string{"task", "destination", "agent", "catalog"} {
		t.Run(mode, func(t *testing.T) {
			mockOMP(t, "profile-success")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a := profileAgent(t)
			rt, err := New().Factory(ctx, a, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			broker := newProtocolBroker("")
			turn := profileTurn(a, rt, broker)
			switch mode {
			case "task":
				turn.Task.ID = ""
			case "destination":
				turn.Task.DestinationSessionID = "different"
			case "agent":
				turn.Task.To = "omp:foreign"
			case "catalog":
				broker.definitions[0].Description = "Not the startup definition"
			}
			_, err = rt.Run(ctx, turn, nil)
			var unavailable *model.Error
			if !errors.As(err, &unavailable) || unavailable.Status != 503 {
				t.Fatalf("unsafe turn: %v", err)
			}
			broker.mu.Lock()
			defer broker.mu.Unlock()
			if len(broker.calls) != 0 {
				t.Fatal("unsafe turn reached broker")
			}
		})
	}
}

func TestHostToolsFactoryFailsClosedBeforePrompt(t *testing.T) {
	for _, mode := range []string{"profile-version", "profile-protocol", "profile-registration", "profile-native", "profile-missing", "profile-schema"} {
		t.Run(mode, func(t *testing.T) {
			mockOMP(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			rt, err := New().Factory(ctx, profileAgent(t), nil)
			if rt != nil {
				rt.Close()
				t.Fatal("unsafe factory returned a runtime")
			}
			var unavailable *model.Error
			if !errors.As(err, &unavailable) || unavailable.Status != 503 {
				t.Fatalf("expected fail503: %v", err)
			}
		})
	}
}

func TestRealHostToolsNoModelPreflight(t *testing.T) {
	if os.Getenv("MESHRA_REAL_OMP_PREFLIGHT") != "1" {
		t.Skip("opt in with MESHRA_REAL_OMP_PREFLIGHT=1; no prompt or model turn is sent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := profileAgent(t)
	a.Ephemeral = true
	rt, err := New().Factory(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if !nativeID.MatchString(rt.SessionID()) {
		t.Fatal("invalid native session")
	}
	r := rt.(*runtime)
	if err = r.setHostTools(ctx, nil); err != nil {
		t.Fatal("clear roster:", err)
	}
	if err = r.setHostTools(ctx, a.ToolDefinitions); err != nil {
		t.Fatal("restore exact roster:", err)
	}
}

func TestLegacyDoesNotRegisterStartupHostDefinitions(t *testing.T) {
	mockOMP(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := testAgent(t, "legacy-catalog")
	a.ToolDefinitions = hostDefinitions()
	rt, err := New().Factory(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	got, err := rt.Run(ctx, model.Turn{Task: model.Task{ID: "legacy", Text: "hello"}}, nil)
	if err != nil || got != "hello" {
		t.Fatalf("legacy behavior changed: %q, %v", got, err)
	}
}
