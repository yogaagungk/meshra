package main

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
	"testing"

	"meshra/internal/model"
)

func TestGlobalFlagsAnywhereAndLiteralValues(t *testing.T) {
	dir := t.TempDir()
	opts, args, help, err := globals([]string{"send", "--message", "--json", "--data-dir", dir, "--to", "claude:one", "--json", "--addr", "localhost:7331"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.JSON || help || opts.Addr != "127.0.0.1:7331" {
		t.Fatalf("%+v help=%v", opts, help)
	}
	_, _, body, _, err := command(args)
	if err != nil {
		t.Fatal(err)
	}
	if body.(model.SendRequest).Text != "--json" {
		t.Fatal("global parser ate literal message")
	}
	_, args, help, err = globals([]string{"send", "--message", "--help", "--to", "claude:one"})
	if err != nil || help {
		t.Fatal("literal --help was interpreted globally", err)
	}
	_, _, body, _, err = command(args)
	if err != nil || body.(model.SendRequest).Text != "--help" {
		t.Fatal(err)
	}
}
func TestLoopbackBindingValidation(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:7331", ":7331", "[::]:7331", "192.0.2.1:7331", "evil.local:7331", "127.0.0.1:0", "127.0.0.1:99999"} {
		if _, err := localAddress(addr); err == nil {
			t.Errorf("accepted unsafe address %s", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:7331", "localhost:7331", "[::1]:7331"} {
		if _, err := localAddress(addr); err != nil {
			t.Error(err)
		}
	}
}
func TestCommandCreatePoliciesAndTaskFlags(t *testing.T) {
	method, path, body, _, err := command([]string{"agent", "create", "--harness", "claude", "--name", "one", "--cwd", ".", "--allow-to", "codex:reviewer,omp:origin", "--allow-to=claude:two", "--allow-operation", "send,request", "--allow-operation", "agents_list", "--delegate", "--model", "test", "--provider", "test-provider"})
	if err != nil {
		t.Fatal(err)
	}
	req := body.(model.CreateAgent)
	if method != "POST" || path != "/v1/agents" || len(req.Policy.Destinations) != 3 || len(req.Policy.Operations) != 3 || !req.Policy.Delegate || !filepath.IsAbs(req.Cwd) || req.Config.Model != "test" {
		t.Fatalf("wrong create request %+v", req)
	}
	_, path, _, follow, err := command([]string{"task", "events", "task-1", "--follow", "--after", "42"})
	if err != nil || !follow || path != "/v1/tasks/task-1/events?after=42&follow=true" {
		t.Fatal(path, follow, err)
	}
	_, _, body, _, err = command([]string{"request", "--from", "omp:origin", "--to", "claude:one", "--message", "review", "--timeout", "2m", "--parent-task", "parent", "--idempotency-key", "key"})
	if err != nil {
		t.Fatal(err)
	}
	send := body.(model.SendRequest)
	if send.TimeoutSeconds != 120 || send.ParentTaskID != "parent" || send.IdempotencyKey != "key" {
		t.Fatal(send)
	}
}
func TestInvalidCommands(t *testing.T) {
	for _, args := range [][]string{{"agent"}, {"agent", "create", "--name", "one"}, {"agent", "list", "extra"}, {"agent", "status"}, {"agent", "bad", "id"}, {"send", "--to", "a"}, {"request", "--to", "a", "--message", "x", "--timeout", "-1"}, {"task", "events", "id", "--after", "-1"}, {"task", "status", "id", "--follow"}, {"bad"}} {
		if _, _, _, _, err := command(args); err == nil {
			t.Errorf("accepted invalid args %v", args)
		}
	}
}
func TestConfigPrecedenceAndValidation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte(`{"addr":"127.0.0.1:8000","dataDir":"`+dir+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	opts, _, _, err := globals([]string{"--config", file, "agent", "list", "--addr", "127.0.0.1:9000", "--json"})
	if err != nil || opts.Addr != "127.0.0.1:9000" || !opts.JSON || opts.DataDir != dir {
		t.Fatal(opts, err)
	}
	if err = os.WriteFile(file, []byte(`{"token":"do-not-accept-secrets"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = globals([]string{"--config", file}); err == nil {
		t.Fatal("unknown config field accepted")
	}
}
func TestCLIJSONAndHTTPError(t *testing.T) {
	t.Setenv("MESHRA_TOKEN", "human-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer human-token" {
			t.Error("missing credential")
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"agent not found"}`)
			return
		}
		io.WriteString(w, `{"schemaVersion":1,"taskId":"t","correlationId":"c","result":"complete result","originSessionId":"native"}`)
	}))
	defer srv.Close()
	var out, errout bytes.Buffer
	err := run(context.Background(), []string{"task", "status", "t", "--json", "--addr", strings.TrimPrefix(srv.URL, "http://")}, strings.NewReader(""), &out, &errout)
	if err != nil {
		t.Fatal(err)
	}
	var task map[string]any
	if err = json.Unmarshal(out.Bytes(), &task); err != nil || task["result"] != "complete result" || task["originSessionId"] != "native" {
		t.Fatal(out.String(), err)
	}
	if errout.Len() != 0 {
		t.Fatal(errout.String())
	}
	err = run(context.Background(), []string{"agent", "status", "missing", "--addr", strings.TrimPrefix(srv.URL, "http://")}, strings.NewReader(""), &out, &errout)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404: agent not found") {
		t.Fatal(err)
	}
}
func TestMCPProxyAgentOnlyAndProtocolStdout(t *testing.T) {
	admin := false
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/identity" {
			json.NewEncoder(w).Encode(map[string]any{"admin": admin, "agentId": "omp:origin"})
			return
		}
		calls++
		var req map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req["id"]) == 0 {
			w.WriteHeader(202)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
	}))
	defer srv.Close()
	opts := options{Addr: strings.TrimPrefix(srv.URL, "http://"), DataDir: t.TempDir()}
	t.Setenv("MESHRA_TOKEN", "agent-token")
	input := "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n"
	var out bytes.Buffer
	if err := proxy(context.Background(), opts, strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || strings.Count(out.String(), "\n") != 1 || !json.Valid(bytes.TrimSpace(out.Bytes())) {
		t.Fatal(calls, out.String())
	}
	admin = true
	out.Reset()
	if err := proxy(context.Background(), opts, strings.NewReader(input), &out); err == nil || out.Len() != 0 || calls != 2 {
		t.Fatal("admin proxy was not rejected")
	}
	t.Setenv("MESHRA_TOKEN", "")
	if err := os.WriteFile(filepath.Join(opts.DataDir, "admin.token"), []byte("sensitive-admin"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := proxy(context.Background(), opts, strings.NewReader(input), &out); err == nil || calls != 2 {
		t.Fatal("proxy read admin.token")
	}
}
