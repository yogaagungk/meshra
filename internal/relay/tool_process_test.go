package relay

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"meshra/internal/sandbox"
)

func TestProcessArgumentsAndWorkspaceGate(t *testing.T) {
	good := []toolProcessArgs{
		{Operation: "start", Argv: []string{"echo", "hello"}},
		{Operation: "start", Shell: "echo hello"},
		{Operation: "poll", ID: "0123456789abcdef0123456789abcdef"},
		{Operation: "kill", ID: "0123456789abcdef0123456789abcdef"},
		{Operation: "read-log", ID: "0123456789abcdef0123456789abcdef", Offset: 2, Limit: 4},
	}
	for _, q := range good {
		if err := validateProcessArgs(&q); err != nil {
			t.Fatalf("valid %+v: %v", q, err)
		}
	}
	bad := []toolProcessArgs{
		{Operation: "start"}, {Operation: "start", Argv: []string{"echo"}, Shell: "true"},
		{Operation: "start", Argv: []string{"echo"}, TimeoutSeconds: 301},
		{Operation: "poll", ID: "bad"},
		{Operation: "kill", ID: "0123456789abcdef0123456789abcdef", Limit: 1},
		{Operation: "read-log", ID: "0123456789abcdef0123456789abcdef", Limit: 65537},
	}
	for _, q := range bad {
		if err := validateProcessArgs(&q); err == nil {
			t.Fatalf("accepted %+v", q)
		}
	}
	for _, name := range []string{"meshra_workspace_write", "meshra_workspace_command", "meshra_attachment_publish", "meshra_execute_code"} {
		if !processWorkspaceConflict(name) {
			t.Fatalf("missing workspace gate: %s", name)
		}
	}
	for _, name := range []string{"meshra_process", "meshra_workspace_read", "meshra_todo"} {
		if processWorkspaceConflict(name) {
			t.Fatalf("unnecessary workspace gate: %s", name)
		}
	}
}

func TestConsoleRevocationReapsOwnedBackgroundProcess(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap unavailable")
	}
	shell, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sandbox.Configure(sandbox.Config{Executables: map[string]string{"bwrap": bwrap, "shell": shell}})
	if err != nil {
		t.Fatal(err)
	}
	if status := sandbox.Check(context.Background(), config); status.State != "ready" {
		t.Skipf("confined runtime unavailable: %v", status.Prerequisites)
	}
	r, _ := openTest(t, nil)
	r.SetConsoleSessionValidator(func(id string) bool { return id == "owner" || id == "other" })
	owner, release, err := r.beginConsoleJob(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other, releaseOther, err := r.beginConsoleJob(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOther()
	snapshot, err := sandbox.SnapshotWorkspace(context.Background(), t.TempDir(), t.TempDir(), sandbox.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	manager := sandbox.NewProcessManager(context.Background())
	defer manager.Close()
	id, done, err := manager.StartContext(owner, config, sandbox.Request{Workspace: snapshot, Argv: []string{shell, "-c", "while :; do sleep 1; done"}, BeforeStart: func() error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	r.RevokeConsoleSession("owner")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("revoked worker survived")
	}
	if other.Err() != nil {
		t.Fatal("revoked unrelated console job")
	}
	status, err := manager.Poll(id)
	if err != nil || !status.Finished || status.Failure == "" {
		t.Fatalf("revoked process: %+v %v", status, err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("revoked worker retained workspace: %v", err)
	}
}
