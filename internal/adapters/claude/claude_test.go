package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"meshra/internal/model"
)

// The subprocess exercises the real JSONL/process boundary without an SDK,
// provider credentials, network, or paid turn.
func TestBridgeProcess(t *testing.T) {
	if os.Getenv("MESHRA_FAKE_CLAUDE") != "1" {
		return
	}
	var input struct{ Prompt, Cwd, SessionID, Token string }
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		os.Exit(2)
	}
	emit := func(kind, data string) {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"type": kind, "data": data})
	}
	id := input.SessionID
	if id == "" {
		id = "native-" + filepath.Base(input.Cwd)
	}
	if input.Prompt == "mismatch" {
		id = "someone-elses-session"
	}
	emit("session", id)
	switch input.Prompt {
	case "wait":
		time.Sleep(time.Minute)
	case "bad-json":
		fmt.Println("not json")
	case "missing-result":
	case "error":
		emit("error", "provider authentication rejected "+input.Token)
	default:
		emit("approval_required", "Denied tool Bash")
		emit("delta", "answer ")
		emit("result", id+": "+input.Prompt+" "+input.Token)
	}
	os.Exit(0)
}
func fakeAdapter(started *atomic.Int32) model.Adapter {
	return adapter(func(context.Context) (commandFactory, error) {
		return func(ctx context.Context) *exec.Cmd {
			started.Add(1)
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBridgeProcess$")
			cmd.Env = append(os.Environ(), "MESHRA_FAKE_CLAUDE=1")
			return cmd
		}, nil
	})
}
func TestSessionAllocationResumeIsolation(t *testing.T) {
	var started atomic.Int32
	a := fakeAdapter(&started)
	one, err := a.Factory(context.Background(), model.Agent{ID: "claude:one", Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := a.Factory(context.Background(), model.Agent{ID: "claude:two", Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if started.Load() != 0 || one.SessionID() != "" || two.SessionID() != "" {
		t.Fatal("factory ran a turn or invented a session")
	}
	var events []string
	emit := func(kind, data string) { events = append(events, kind+":"+data) }
	first, err := one.Run(context.Background(), model.Turn{Task: model.Task{Text: "first"}, Token: "secret"}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if one.SessionID() == "" || strings.Contains(first, "secret") || !strings.Contains(first, "[redacted]") {
		t.Fatalf("bad first response %q", first)
	}
	if len(events) < 3 || !strings.HasPrefix(events[0], "session:") || !strings.HasPrefix(events[1], "approval_required:") {
		t.Fatalf("event sequence: %v", events)
	}
	id := one.SessionID()
	second, err := one.Run(context.Background(), model.Turn{Task: model.Task{Text: "second"}}, nil)
	if err != nil || !strings.HasPrefix(second, id+":") || one.SessionID() != id {
		t.Fatalf("resume failed: %q %v", second, err)
	}
	if _, err = two.Run(context.Background(), model.Turn{Task: model.Task{Text: "other"}}, nil); err != nil {
		t.Fatal(err)
	}
	if one.SessionID() == two.SessionID() {
		t.Fatal("independent identities shared a native session")
	}
}
func TestBridgeFailures(t *testing.T) {
	for _, prompt := range []string{"bad-json", "missing-result", "error", "mismatch"} {
		t.Run(prompt, func(t *testing.T) {
			var started atomic.Int32
			r, err := fakeAdapter(&started).Factory(context.Background(), model.Agent{Cwd: t.TempDir(), SessionID: "owned-session"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			_, err = r.Run(context.Background(), model.Turn{Task: model.Task{Text: prompt}, Token: "credential"}, nil)
			if err == nil || strings.Contains(err.Error(), "credential") {
				t.Fatalf("expected sanitized failure, got %v", err)
			}
			if r.SessionID() != "owned-session" {
				t.Fatal("failure remapped owned session")
			}
		})
	}
}
func TestCancelReapsBridgeAndPreservesSession(t *testing.T) {
	var started atomic.Int32
	life, stop := context.WithCancel(context.Background())
	defer stop()
	r, err := fakeAdapter(&started).Factory(life, model.Agent{Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, model.Turn{Task: model.Task{Text: "wait"}}, func(kind, data string) {
			if kind == "session" {
				cancel()
			}
		})
		done <- err
	}()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge not reaped after cancellation")
	}
	if r.SessionID() == "" {
		t.Fatal("allocated session was not retained")
	}
	if _, err = r.Run(context.Background(), model.Turn{Task: model.Task{Text: "resumed"}}, nil); err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err = r.Run(context.Background(), model.Turn{Task: model.Task{Text: "after-stop"}}, nil); err == nil {
		t.Fatal("daemon cancellation ignored")
	}
}
func TestCloseCancelsTurn(t *testing.T) {
	var started atomic.Int32
	r, err := fakeAdapter(&started).Factory(context.Background(), model.Agent{Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	allocated := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), model.Turn{Task: model.Task{Text: "wait"}}, func(kind, data string) {
			if kind == "session" {
				allocated <- struct{}{}
			}
		})
		done <- err
	}()
	select {
	case <-allocated:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not allocate")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not reap bridge")
	}
	if _, err = r.Run(context.Background(), model.Turn{}, nil); err == nil {
		t.Fatal("closed runtime accepted a turn")
	}
}
func TestAuthenticatedSmoke(t *testing.T) {
	if os.Getenv("MESHRA_AUTH_SMOKE") != "1" {
		t.Skip("opt-in paid Claude SDK smoke; see docs/adapters-sdk.md")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a := New()
	r, err := a.Factory(ctx, model.Agent{ID: "claude:smoke", Cwd: t.TempDir(), Config: model.Config{Model: os.Getenv("MESHRA_CLAUDE_MODEL")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for range 2 {
		text, err := r.Run(ctx, model.Turn{Task: model.Task{Text: "Reply exactly MESHRA_SMOKE_OK. Do not use tools."}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "MESHRA_SMOKE_OK") || r.SessionID() == "" {
			t.Fatalf("unexpected smoke result %q", text)
		}
	}
}
