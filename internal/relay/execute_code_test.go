package relay

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

func TestExecuteCodeValidation(t *testing.T) {
	for _, q := range []toolExecuteCodeArgs{{}, {Code: strings.Repeat("x", executeCodeLimit+1)}, {Code: "a\x00b"}, {Code: "x", TimeoutSeconds: -1}, {Code: "x", TimeoutSeconds: 301}} {
		if validateExecuteCode(&q) == nil {
			t.Fatalf("accepted invalid input: %+v", q)
		}
	}
	q := toolExecuteCodeArgs{Code: "print(1)"}
	if err := validateExecuteCode(&q); err != nil || q.TimeoutSeconds != 60 {
		t.Fatalf("default timeout: %+v %v", q, err)
	}
	action, err := parseToolAction(model.ToolCall{Name: "meshra_execute_code", Arguments: json.RawMessage(`{"code":"print(1)"}`)})
	if err != nil || !action.approval || action.mutation {
		t.Fatalf("action must be separately approved but permit nested mutations: %+v %v", action, err)
	}
}

func TestExecuteCodeCatalogGate(t *testing.T) {
	for _, tc := range []struct {
		harness             string
		read, command, want bool
	}{
		{"openai", true, true, true}, {"openai", false, true, false}, {"openai", true, false, false},
		{"claude", true, true, false}, {"omp", true, true, false},
	} {
		var p model.Profile
		p.Binding.Harness = tc.harness
		p.Capabilities.WorkspaceRead = tc.read
		p.Capabilities.Command = tc.command
		found := false
		for _, definition := range profileToolCatalog(p) {
			if definition.Name == "meshra_execute_code" {
				found = true
			}
		}
		if found != tc.want {
			t.Fatalf("catalog gate %+v: %v", tc, found)
		}
	}
}

func TestCodeBridgeRejectsRecursionAndRemovesSocket(t *testing.T) {
	b := &toolBroker{}
	bridge, err := newCodeBridge(context.Background(), b, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	path, dir := bridge.listener.Addr().String(), bridge.dir
	defer bridge.Close()
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0700}, {path, 0600}} {
		info, err := os.Stat(item.path)
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("unsafe mode for %s: %v %v", item.path, info, err)
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Post("http://unix/call", "application/json", strings.NewReader(`{"name":"meshra_execute_code","arguments":{"code":"print(1)"}}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("recursive call status %d", response.StatusCode)
	}
	response, err = client.Post("http://unix/call", "application/json", strings.NewReader(strings.Repeat("x", maxToolArguments+4097)))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized call status %d", response.StatusCode)
	}
	bridge.Close()
	if _, err := os.Lstat(filepath.Join(dir, "broker.sock")); !os.IsNotExist(err) {
		t.Fatalf("socket remains: %v", err)
	}
}
