//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestBackgroundLogBoundAndReads(t *testing.T) {
	p := &backgroundProcess{done: make(chan struct{})}
	m := NewProcessManager(context.Background())
	m.processes["id"] = p
	data := bytes.Repeat([]byte("x"), maxBackgroundLogBytes+100)
	if n, err := p.Write(data); err != nil || n != len(data) {
		t.Fatalf("write: %d %v", n, err)
	}
	status, err := m.Poll("id")
	if err != nil || !status.Truncated || status.LogBytes != maxBackgroundLogBytes {
		t.Fatalf("status: %+v %v", status, err)
	}
	got, err := m.ReadLog("id", maxBackgroundLogBytes-3, 10)
	if err != nil || string(got) != "xxx" {
		t.Fatalf("log: %q %v", got, err)
	}
	got[0] = 'z'
	again, _ := m.ReadLog("id", maxBackgroundLogBytes-3, 10)
	if string(again) != "xxx" {
		t.Fatal("read exposed mutable log")
	}
	if _, err := m.ReadLog("id", -1, 1); err == nil {
		t.Fatal("negative offset accepted")
	}
	if _, err := m.ReadLog("id", 0, maxBackgroundReadBytes+1); err == nil {
		t.Fatal("oversized read accepted")
	}
	if _, err := m.Poll("other"); err == nil {
		t.Fatal("unknown process accepted")
	}
}

func TestBackgroundStartRequiresGuardAndBoundedDuration(t *testing.T) {
	m := NewProcessManager(context.Background())
	if _, err := m.Start(Config{}, Request{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("missing guard: %v", err)
	}
	guard := func() error { return nil }
	if _, err := m.Start(Config{}, Request{Kind: Browser, BeforeStart: guard}); !errors.Is(err, ErrDenied) {
		t.Fatalf("non-command: %v", err)
	}
	for i := range maxBackgroundProcesses {
		m.processes[string(rune('a'+i))] = &backgroundProcess{}
	}
	if _, err := m.Start(Config{}, Request{BeforeStart: guard, Timeout: 301 * time.Second}); err == nil {
		t.Fatal("long timeout accepted")
	}
	if _, err := m.Start(Config{}, Request{BeforeStart: guard}); !errors.Is(err, ErrLimit) {
		t.Fatalf("count limit: %v", err)
	}
}

func TestBackgroundCloseConcurrent(t *testing.T) {
	m := NewProcessManager(context.Background())
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := m.Poll("id"); !errors.Is(err, ErrDenied) {
		t.Fatalf("closed manager: %v", err)
	}
}

func TestRealBackgroundProcessLifecycle(t *testing.T) {
	c := realConfig(t)
	root := t.TempDir()
	snapshot := snapshotTest(t, root)
	manager := NewProcessManager(context.Background())
	defer manager.Close()
	denied := Request{Workspace: snapshot, Argv: []string{c.Executables["shell"], "-c", "touch /workspace/denied"}, BeforeStart: func() error { return ErrDenied }}
	if _, err := manager.Start(c, denied); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshot.Root, "denied")); !os.IsNotExist(err) {
		t.Fatal("denied command ran")
	}
	claimed := false
	id, err := manager.Start(c, Request{Workspace: snapshot, Argv: []string{c.Executables["shell"], "-c", "printf output; printf result > /workspace/result"}, BeforeStart: func() error { claimed = true; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("start guard not called")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := manager.Poll(id)
		if err != nil {
			t.Fatal(err)
		}
		if status.Finished {
			if status.ExitCode == nil || *status.ExitCode != 0 || status.Failure != "" {
				t.Fatalf("status: %+v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background command did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	log, err := manager.ReadLog(id, 0, 100)
	if err != nil || string(log) != "output" {
		t.Fatalf("output: %q %v", log, err)
	}
	if _, err := os.Stat(filepath.Join(root, "result")); !os.IsNotExist(err) {
		t.Fatal("wrote original workspace")
	}
	manifest, err := snapshot.Changes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()
	if len(manifest.Changes) == 0 {
		t.Fatal("missing private workspace change")
	}
}

func TestRealBackgroundKillReleasesSnapshot(t *testing.T) {
	c := realConfig(t)
	snapshot := snapshotTest(t, t.TempDir())
	manager := NewProcessManager(context.Background())
	defer manager.Close()
	id, err := manager.Start(c, Request{Workspace: snapshot, Argv: []string{c.Executables["shell"], "-c", "while :; do sleep 1; done"}, BeforeStart: func() error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); !errors.Is(err, ErrConflict) {
		t.Fatalf("live snapshot close: %v", err)
	}
	if err := manager.Kill(id); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Poll(id)
	if err != nil || !status.Finished {
		t.Fatalf("kill status: %+v %v", status, err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("snapshot still held after kill: %v", err)
	}
}

func TestRealBackgroundOwnerCancellationReapsProcess(t *testing.T) {
	c := realConfig(t)
	snapshot := snapshotTest(t, t.TempDir())
	manager := NewProcessManager(context.Background())
	defer manager.Close()
	owner, revoke := context.WithCancel(context.Background())
	defer revoke()
	id, done, err := manager.StartContext(owner, c, Request{Workspace: snapshot, Argv: []string{c.Executables["shell"], "-c", "while :; do sleep 1; done"}, BeforeStart: func() error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	revoke()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("revoked owner's process was not reaped")
	}
	status, err := manager.Poll(id)
	if err != nil || !status.Finished || status.Failure == "" {
		t.Fatalf("cancelled status: %+v %v", status, err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("revoked process retained snapshot: %v", err)
	}
}
