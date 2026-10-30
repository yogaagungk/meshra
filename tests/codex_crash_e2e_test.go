package tests

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"meshra/internal/adapters/codex"
	"meshra/internal/api"
	"meshra/internal/model"
	"meshra/internal/relay"
)

// A protocol-faithful local App Server subprocess, not a provider integration.
// Commit a controllable external effect, then await an actual process kill.
func TestCrashCodexTransportHelper(t *testing.T) {
	if os.Getenv("MESHRA_CRASH_CODEX_HELPER") != "1" {
		t.Skip("subprocess only")
	}
	send := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"id": id, "result": result})
		fmt.Println(string(b))
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(21)
		}
		switch request.Method {
		case "initialize":
			send(request.ID, map[string]any{"userAgent": "simulated-crash-app-server"})
		case "initialized":
		case "config/read":
			send(request.ID, map[string]any{"config": map[string]any{}, "origins": map[string]any{}})
		case "thread/start", "thread/resume":
			send(request.ID, map[string]any{"thread": map[string]any{"id": "thread-crash-owned"}})
		case "turn/start":
			var params struct {
				ThreadID string `json:"threadId"`
				Sandbox  struct {
					Type    string `json:"type"`
					Network bool   `json:"networkAccess"`
				} `json:"sandboxPolicy"`
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			if json.Unmarshal(request.Params, &params) != nil || params.ThreadID != "thread-crash-owned" || params.Sandbox.Type != "dangerFullAccess" || params.Sandbox.Network || len(params.Input) != 1 {
				os.Exit(22)
			}
			words := strings.Fields(params.Input[0].Text)
			if len(words) < 3 || words[0] != "Meshra" || words[1] != "task" {
				os.Exit(23)
			}
			taskID := words[2]
			file, err := os.OpenFile(filepath.Join(os.Getenv("MESHRA_CRASH_EFFECT_DIR"), "effects"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				os.Exit(24)
			}
			if _, err = fmt.Fprintln(file, taskID); err != nil {
				os.Exit(25)
			}
			if err = file.Sync(); err != nil {
				os.Exit(26)
			}
			file.Close()
			send(request.ID, map[string]any{"turn": map[string]any{"id": "turn-crash-owned", "status": "inProgress"}})
			marker, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "taskId": taskID})
			if os.WriteFile(filepath.Join(os.Getenv("MESHRA_CRASH_EFFECT_DIR"), "started"), marker, 0600) != nil {
				os.Exit(27)
			}
			// Do not complete. The acceptance test kills this real adapter child.
		}
	}
	os.Exit(0)
}

func TestKilledNativeAdapterIsAmbiguousWithoutBlindReplay(t *testing.T) {
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nexec " + strconv.Quote(executable) + " -test.run '^TestCrashCodexTransportHelper$' -- \"$@\"\n"
	if err = os.WriteFile(filepath.Join(root, "codex"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MESHRA_CRASH_CODEX_HELPER", "1")
	t.Setenv("MESHRA_CRASH_EFFECT_DIR", root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	dir := t.TempDir()
	adapters := map[string]model.Adapter{"codex": codex.New()}
	core, err := relay.Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	admin := model.Principal{Admin: true}
	origin, err := core.Create(admin, model.CreateAgent{Harness: "omp", Name: "origin", Cwd: t.TempDir(), Policy: model.Policy{Operations: []string{"status", "events"}}})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := core.Create(admin, model.CreateAgent{Harness: "codex", Name: "crash", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = core.Action(admin, dest.ID, "start"); err != nil {
		t.Fatal(err)
	}
	request := model.SendRequest{From: origin.ID, To: dest.ID, Text: "controlled external effect", Type: "request", IdempotencyKey: "crash-once"}
	task, err := core.Send(admin, request)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := core.Send(admin, request)
	if err != nil || duplicate.ID != task.ID {
		t.Fatalf("duplicate created a second effect-bearing task: %+v %v", duplicate, err)
	}
	queued, err := core.Send(admin, model.SendRequest{From: origin.ID, To: dest.ID, Text: "must remain queued"})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(10 * time.Second)
	var marker struct {
		PID    int    `json:"pid"`
		TaskID string `json:"taskId"`
	}
	for {
		data, e := os.ReadFile(filepath.Join(root, "started"))
		if e == nil {
			if err = json.Unmarshal(data, &marker); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(until) {
			t.Fatal("adapter did not commit the simulated external effect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if marker.PID < 1 || marker.TaskID != task.ID {
		t.Fatalf("wrong process/task marker: %+v", marker)
	}
	child, err := os.FindProcess(marker.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Kill(); err != nil {
		t.Fatal(err)
	}
	until = time.Now().Add(10 * time.Second)
	var failed model.Task
	for {
		failed, err = core.Task(admin, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if model.Terminal(failed.State) {
			break
		}
		if time.Now().After(until) {
			t.Fatal("killed adapter did not produce a terminal task")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failed.State != "failed" || !failed.Ambiguous || failed.ErrorCategory != "adapter_failure" || failed.Attempts != 1 {
		t.Fatalf("unsafe crash classification: %+v", failed)
	}
	token, err := core.Token(admin, origin.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.New(core))
	var owned model.Task
	httpJSON(t, server.Client(), server.URL, token, "GET", "/v1/tasks/"+task.ID, nil, &owned, 200)
	server.Close()
	if owned.CorrelationID != task.CorrelationID || owned.OriginAgentID != origin.ID {
		t.Fatal("crash reply lost original requester correlation")
	}
	if err = core.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := relay.Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	unchanged, err := resumed.Task(admin, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err = resumed.Send(admin, request)
	if err != nil || duplicate.ID != task.ID || duplicate.State != "failed" {
		t.Fatalf("duplicate after restart escaped reconciliation: %+v %v", duplicate, err)
	}
	pending, err := resumed.Task(admin, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	effects, err := os.ReadFile(filepath.Join(root, "effects"))
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != "failed" || !unchanged.Ambiguous || unchanged.Attempts != 1 || pending.State != "queued" || pending.Attempts != 0 || string(effects) != task.ID+"\n" {
		t.Fatalf("crash/restart repeated effects or lost inbox: failed=%+v pending=%+v effects=%q", unchanged, pending, effects)
	}
}
