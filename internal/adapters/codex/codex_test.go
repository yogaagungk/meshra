package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		switch cmd.Method {
		case "initialize":
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
			send(map[string]any{"id": 100, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": session, "turnId": "turn-1", "itemId": "command-1", "command": "rm -rf example"}})
		case "turn/interrupt":
			if !active {
				os.Exit(25)
			}
			response(cmd.ID, map[string]any{})
			notify("turn/completed", map[string]any{"threadId": session, "turn": map[string]any{"id": "turn-1", "status": "interrupted"}})
		case "":
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
