package relay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"meshra/internal/model"
)

func TestCrashWorker(t *testing.T) {
	dir := os.Getenv("MESHRA_CRASH_WORKER_DIR")
	if dir == "" {
		t.Skip("subprocess only")
	}
	r, err := Open(dir, fakeAdapters(func(ctx context.Context, v model.Turn, _ model.Emit) (string, error) {
		if err := os.WriteFile(filepath.Join(dir, "executing"), []byte(v.Task.ID), 0600); err != nil {
			return "", err
		}
		<-ctx.Done()
		return "", ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = r.Action(admin, "claude:dest", "start"); err != nil {
		t.Fatal(err)
	}
	<-r.ctx.Done()
}
func TestAbruptProcessDeathPreservesInboxWithoutReplay(t *testing.T) {
	r, dir := openTest(t, nil)
	origin := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "claude", "dest")
	running := sendTest(t, r, origin.ID, dest.ID, "uncertain work")
	queued := sendTest(t, r, origin.ID, dest.ID, "queued work")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashWorker$")
	cmd.Env = append(os.Environ(), "MESHRA_CRASH_WORKER_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(dir, "executing"))
		if err == nil {
			if string(b) != running.ID {
				t.Fatalf("FIFO lost before crash: %s", b)
			}
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !started {
		t.Fatal("worker did not execute task")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	resumed, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	uncertain, err := resumed.Task(admin, running.ID)
	if err != nil || uncertain.State != "failed" || uncertain.Attempts != 1 {
		t.Fatalf("unsafe uncertain recovery: %+v %v", uncertain, err)
	}
	pending, err := resumed.Task(admin, queued.ID)
	if err != nil || pending.State != "queued" || pending.Attempts != 0 {
		t.Fatalf("queued work changed: %+v %v", pending, err)
	}
	startTest(t, resumed, dest.ID)
	done := awaitTask(t, resumed, queued.ID)
	if done.State != "completed" || done.Attempts != 1 || done.Result != "reply:"+dest.ID+":queued work" {
		t.Fatalf("queued work not recovered: %+v", done)
	}
	unchanged, err := resumed.Task(admin, running.ID)
	if err != nil || unchanged.Attempts != 1 || unchanged.State != "failed" {
		t.Fatalf("uncertain work automatically replayed: %+v %v", unchanged, err)
	}
}
