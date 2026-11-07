package claude

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"meshra/internal/model"
)

type fakeLauncher struct {
	called     atomic.Int32
	gotSocket  string
	payloadOut string
	script     string
}

func (f *fakeLauncher) Launch(ctx context.Context, toolSocket string) (*model.ConfinedProcess, error) {
	f.called.Add(1)
	f.gotSocket = toolSocket
	cmd := exec.Command("/bin/sh", "-c", f.script)
	return &model.ConfinedProcess{
		Cmd: cmd, Start: cmd.Start, Wait: cmd.Wait, Close: func() error { return nil },
		Cwd: "/workspace", ToolSocket: "/run/meshra/broker.sock",
	}, nil
}

func restrictedAgent(t *testing.T, confined model.ConfinedLauncher) model.Agent {
	return model.Agent{ID: "claude:profile", Cwd: t.TempDir(), ToolDefinitions: profileDefinitions(), Restricted: true, Confined: confined}
}

func hostCommandCounter(counter *atomic.Int32) model.Adapter {
	return adapter(func(context.Context) (commandFactory, error) {
		return func(ctx context.Context) *exec.Cmd {
			counter.Add(1)
			return exec.CommandContext(ctx, "/bin/false")
		}, nil
	})
}

// A restricted agent with no confined launcher must refuse; the host command
// factory is never used.
func TestRestrictedWithoutConfinementNeverStartsOnHost(t *testing.T) {
	dir, _ := os.MkdirTemp("", "meshra-conf-")
	defer os.RemoveAll(dir)
	var host atomic.Int32
	broker := &scopedBroker{definitions: profileDefinitions(), socket: filepath.Join(dir, "t.sock"), credential: "attempt-only"}
	agent := restrictedAgent(t, nil)
	rt, err := hostCommandCounter(&host).Factory(context.Background(), agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = rt.Run(ctx, model.Turn{Task: model.Task{To: agent.ID, Text: "x"}, Tools: broker}, func(string, string) {})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Status != 503 || !strings.Contains(typed.Message, "confined") {
		t.Fatalf("restricted run without confinement: %v", err)
	}
	if host.Load() != 0 {
		t.Fatal("restricted run started a host process")
	}
}

// With a launcher the adapter uses only the prepared command, sends the confined
// workspace and socket paths, and never the host working directory or socket.
func TestRestrictedUsesConfinedLauncherAndConfinedPaths(t *testing.T) {
	dir, _ := os.MkdirTemp("", "meshra-conf-")
	defer os.RemoveAll(dir)
	capture := filepath.Join(dir, "payload")
	var host atomic.Int32
	launcher := &fakeLauncher{script: `head -n1 > ` + capture + `; printf '{"type":"error","data":"stub"}\n'`}
	broker := &scopedBroker{definitions: profileDefinitions(), socket: filepath.Join(dir, "t.sock"), credential: "attempt-only"}
	agent := restrictedAgent(t, launcher)
	rt, err := hostCommandCounter(&host).Factory(context.Background(), agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = rt.Run(ctx, model.Turn{Task: model.Task{To: agent.ID, Text: "x"}, Tools: broker}, func(string, string) {})
	if launcher.called.Load() != 1 || host.Load() != 0 {
		t.Fatalf("launcher=%d host=%d", launcher.called.Load(), host.Load())
	}
	if launcher.gotSocket != broker.socket {
		t.Fatalf("launcher got %q", launcher.gotSocket)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"cwd":"/workspace"`) || !strings.Contains(text, `"toolSocket":"/run/meshra/broker.sock"`) {
		t.Fatalf("payload does not use confined paths: %s", text)
	}
	if strings.Contains(text, agent.Cwd) || strings.Contains(text, broker.socket) {
		t.Fatalf("payload leaked a host path: %s", text)
	}
}
