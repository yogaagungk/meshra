package externalmcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The fixtures drive the production client against a real MCP server. Nothing
// here asserts on source text or on echoed model text.

type launcherFunc func(ctx context.Context, config Config, env map[string]string) (Process, error)

func (f launcherFunc) Launch(ctx context.Context, config Config, env map[string]string) (Process, error) {
	return f(ctx, config, env)
}

// pipeProcess is one end of an in-process stdio server.
type pipeProcess struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	closer func()
}

// Stdin returns the write end of the pipe.
func (p *pipeProcess) Stdin() io.WriteCloser { return p.stdin }

// Stdout returns the read end of the pipe.
func (p *pipeProcess) Stdout() io.ReadCloser { return p.stdout }

// Close tears the pipe pair down.
func (p *pipeProcess) Close() error {
	_ = p.stdin.Close()
	_ = p.stdout.Close()
	if p.closer != nil {
		p.closer()
	}
	return nil
}

// newPipeLauncher starts one MCP server in process and speaks the stdio
// framing over a pipe pair, so the client exercises its real stdio path.
func newPipeLauncher(t *testing.T, server *mcp.Server) Launcher {
	t.Helper()
	return launcherFunc(func(ctx context.Context, config Config, env map[string]string) (Process, error) {
		requestReader, requestWriter := io.Pipe()
		responseReader, responseWriter := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			session, err := server.Connect(ctx, &mcp.IOTransport{Reader: requestReader, Writer: responseWriter}, nil)
			if err == nil {
				session.Wait()
			}
		}()
		return &pipeProcess{stdin: requestWriter, stdout: responseReader, closer: func() {
			_ = requestReader.Close()
			_ = responseWriter.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}}, nil
	})
}

func TestConfigRefusesShellCommandStringsAndHiddenAuthority(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "server")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := Config{ID: "server-1", Name: "Fixture", Transport: TransportStdio, Executable: executable, WorkspaceID: "workspace-1"}
	if err := base.Validate(); err != nil {
		t.Fatalf("a direct executable was refused: %v", err)
	}
	shell := base
	shell.Executable = "/bin/sh"
	shell.Args = []string{"-c", "echo hello"}
	if err := shell.Validate(); err == nil {
		t.Fatal("a shell command string was accepted")
	}
	relative := base
	relative.Executable = "server"
	if err := relative.Validate(); err == nil {
		t.Fatal("a relative executable was accepted")
	}
	missing := base
	missing.Executable = filepath.Join(directory, "absent")
	if err := missing.Validate(); err == nil {
		t.Fatal("a missing executable was accepted")
	}
	credentials := Config{ID: "server-2", Name: "Fixture", Transport: TransportHTTP, URL: "https://user:secret@example.com/mcp", WorkspaceID: "workspace-1"}
	if err := credentials.Validate(); err == nil {
		t.Fatal("a URL with credentials was accepted")
	}
	fragment := Config{ID: "server-3", Name: "Fixture", Transport: TransportHTTP, URL: "https://example.com/mcp#token", WorkspaceID: "workspace-1"}
	if err := fragment.Validate(); err == nil {
		t.Fatal("a URL with a fragment was accepted")
	}
	both := Config{ID: "server-4", Name: "Fixture", Transport: TransportHTTP, URL: "https://example.com/mcp", Executable: "/bin/true", WorkspaceID: "workspace-1"}
	if err := both.Validate(); err == nil {
		t.Fatal("an http server with an executable was accepted")
	}
	origin, err := OriginOf("https://example.com/mcp")
	if err != nil || origin != "https://example.com" {
		t.Fatalf("unexpected origin %q (%v)", origin, err)
	}
	origin, err = OriginOf("http://127.0.0.1:8080/mcp")
	if err != nil || origin != "http://127.0.0.1:8080" {
		t.Fatalf("unexpected origin %q (%v)", origin, err)
	}
}

func TestStdioCancellationReachesServerWithoutReplayingCall(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "cancellation-fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "blocked", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := Open(ctx, Options{Config: Config{ID: "server-1", Name: "Cancellation fixture", Transport: TransportStdio, Executable: executable, WorkspaceID: "workspace-1"}, Launcher: newPipeLauncher(t, server)})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	callContext, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	done := make(chan error, 1)
	go func() { _, err := session.Call(callContext, "blocked", json.RawMessage(`{}`)); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("tool dispatch did not start")
	}
	cancelCall()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled call returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("cancelled client call did not return")
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("server did not observe cancellation")
	}
}

func TestAuthorizationURLUsesPKCEAndState(t *testing.T) {
	discovery := &OAuthDiscovery{AuthorizationEndpoint: "https://auth.example.com/authorize", ClientID: "client-1", RedirectURI: "http://127.0.0.1:37731/console/api/automation/mcp-oauth-callback"}
	state, err := NewState()
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	target, err := AuthorizationURL(discovery, state, verifier, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(target, "code_challenge_method=S256") || !strings.Contains(target, "state=") {
		t.Fatalf("the authorization URL is incomplete: %s", target)
	}
	if strings.Contains(target, verifier) {
		t.Fatal("the authorization URL carries the code verifier")
	}
}
