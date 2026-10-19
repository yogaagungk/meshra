package omp

import (
	"bufio"
	"context"
	"encoding/base64"
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

func TestOMPTransportHelper(t *testing.T) {
	if os.Getenv("MESHRA_OMP_HELPER") != "1" {
		return
	}
	mode := os.Getenv("MESHRA_MOCK_MODE")
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
	session := "native-" + filepath.Base(filepath.Dir(dir))[:12]
	if resumed != "" {
		session = resumed
	}
	if mode == "wrong-session" {
		session = "other-session"
	}
	send := func(v any) { data, _ := json.Marshal(v); fmt.Println(string(data)) }
	send(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1, 2}, "maxFrameBytes": 1048576})
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var cmd map[string]any
		if json.Unmarshal(scan.Bytes(), &cmd) != nil {
			os.Exit(21)
		}
		id, kind := cmd["id"], cmd["type"]
		data := any(map[string]any{})
		if kind == "get_state" {
			data = map[string]any{"sessionId": session, "sessionFile": filepath.Join(dir, session+".jsonl")}
		}
		if kind == "prompt" {
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
