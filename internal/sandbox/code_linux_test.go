//go:build linux

package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealCodeWorkerReadOnlyAndBrokerSocket(t *testing.T) {
	c := realConfig(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	c.Executables["python3"] = python
	c, err = Configure(c)
	if err != nil {
		t.Fatal(err)
	}
	s := snapshotTest(t, t.TempDir())
	dir := t.TempDir()
	listener, err := net.Listen("unix", filepath.Join(dir, "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(listener.Addr().String(), 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4)
		if n, _ := conn.Read(buf); string(buf[:n]) == "ping" {
			_, _ = conn.Write([]byte("pong"))
		}
	}()
	code := `import socket
try:
    open('/workspace/forbidden', 'w').write('bad')
    raise SystemExit('workspace unexpectedly writable')
except OSError:
    pass
s = socket.socket(socket.AF_UNIX)
s.settimeout(5)
s.connect('/run/meshra/broker.sock')
s.sendall(b'ping')
assert s.recv(4) == b'pong'
print('confined')`
	result, err := Run(context.Background(), c, Request{Workspace: s, Argv: []string{c.Executables["python3"], "-I", "-c", code}, Kind: Code, ReadOnly: true, Timeout: 10 * time.Second, SocketMounts: []SocketMount{{Source: listener.Addr().String(), Target: "/run/meshra/broker.sock", Purpose: "broker"}}})
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "confined") {
		t.Fatalf("worker: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "forbidden")); !os.IsNotExist(err) {
		t.Fatalf("snapshot modified: %v", err)
	}
}
