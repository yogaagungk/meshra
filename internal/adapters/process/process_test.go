package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessTransportHelper(t *testing.T) {
	mode := os.Getenv("MESHRA_PROCESS_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "invalid":
		fmt.Println("not JSON")
	case "oversized":
		fmt.Println(strings.Repeat("x", MaxFrame+1))
	case "exit":
		os.Exit(9)
	case "frame-eof":
		fmt.Println(`{"result":"final"}`)
	case "hang":
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(0)
}
func startHelper(t *testing.T, mode string) *Process {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "MESHRA_PROCESS_HELPER="+mode)
	p, err := Start(context.Background(), binary, []string{"-test.run", "^TestProcessTransportHelper$"}, t.TempDir(), env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}
func TestProcessReportsTransportFailure(t *testing.T) {
	for _, mode := range []string{"invalid", "oversized", "exit"} {
		t.Run(mode, func(t *testing.T) {
			p := startHelper(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := p.Read(ctx); err == nil {
				t.Fatal("expected protocol/process error")
			} else if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("failed to observe process error: %v", err)
			}
		})
	}
}
func TestFinalFrameBeforeEOF(t *testing.T) {
	p := startHelper(t, "frame-eof")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := p.Read(ctx)
	if err != nil || string(raw) != `{"result":"final"}` {
		t.Fatalf("frame %s, error %v", raw, err)
	}
	if _, err = p.Read(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}
func TestCloseReapsAndReadCancels(t *testing.T) {
	p := startHelper(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read cancellation: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if p.cmd.ProcessState == nil {
		t.Fatal("owned process was not reaped")
	}
	if err := syscall.Kill(p.cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("owned PID still exists: %v", err)
	}
}
func TestLifetimeCancellation(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, binary, []string{"-test.run", "^TestProcessTransportHelper$"}, t.TempDir(), append(os.Environ(), "MESHRA_PROCESS_HELPER=hang"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer p.Close()
	cancel()
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("lifetime cancellation did not reap process")
	}
}

func TestEphemeralRuntimeStateDoesNotCompeteWithDurableIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	durable, release, err := RuntimeStateDir("codex", "codex:assistant", false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, err = RuntimeStateDir("codex", "codex:assistant", false); err == nil {
		t.Fatal("durable identity lost exclusive ownership")
	}
	for i := 0; i < 10; i++ {
		dir, cleanup, err := RuntimeStateDir("codex", "codex:assistant", true)
		if err != nil {
			t.Fatal("chat collided with durable identity", err)
		}
		if dir == durable {
			t.Fatal("chat reused durable state")
		}
		if err = os.WriteFile(filepath.Join(dir, "session.json"), []byte("temporary state"), 0600); err != nil {
			t.Fatal(err)
		}
		cleanup()
		cleanup()
		if _, err = os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("temporary native state retained", err)
		}
	}
	if _, err = os.Stat(durable); err != nil {
		t.Fatal("chat cleanup removed durable state", err)
	}
}
