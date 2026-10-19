package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
