package opencode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"meshra/internal/model"
)

type server struct {
	base     string
	password string
	client   *http.Client
	stop     func()
	cwd      string
}
type starter func(context.Context, model.Agent, model.Turn) (*server, error)

func executable() (string, error) {
	if path := os.Getenv("MESHRA_OPENCODE_BIN"); path != "" {
		return exec.LookPath(path)
	}
	if path, err := exec.LookPath("opencode"); err == nil {
		return path, nil
	}
	exe, _ := os.Executable()
	_, source, _, _ := runtime.Caller(0)
	for _, root := range []string{filepath.Dir(exe), ".", filepath.Join(filepath.Dir(source), "../../..")} {
		path, err := filepath.Abs(filepath.Join(root, "bridges/claude-agent-sdk/opencode/node_modules/.bin/opencode"))
		if err == nil {
			if _, err = os.Stat(path); err == nil {
				return path, nil
			}
		}
	}
	return "", errors.New("OpenCode missing: npm ci --prefix bridges/claude-agent-sdk/opencode (approve its postinstall if npm requires it), or set MESHRA_OPENCODE_BIN")
}

// A process is scoped to one turn so the inline MCP credential never survives
// the turn or crosses to another agent. Native sessions live in OpenCode's store.
func startServer(binary string) starter {
	return func(ctx context.Context, agent model.Agent, turn model.Turn) (*server, error) {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		password := hex.EncodeToString(secret)
		permission := map[string]any{"*": "ask", "read": "allow", "glob": "allow", "grep": "allow"}
		config := map[string]any{"autoupdate": false, "permission": permission, "share": "disabled"}
		if turn.MCPURL != "" && turn.Token != "" {
			config["mcp"] = map[string]any{"meshra": map[string]any{"type": "remote", "url": turn.MCPURL, "oauth": false, "enabled": true, "headers": map[string]string{"Authorization": "Bearer " + turn.Token}}}
		}
		raw, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		procCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		cmd := exec.CommandContext(procCtx, binary, "serve", "--hostname", "127.0.0.1", "--port", "0")
		cmd.Dir = agent.Cwd
		cmd.Env = environment(os.Environ(), map[string]string{"OPENCODE_SERVER_USERNAME": "opencode", "OPENCODE_SERVER_PASSWORD": password, "OPENCODE_CONFIG_CONTENT": string(raw)})
		cmd.Stderr = io.Discard
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cmd.WaitDelay = 3 * time.Second
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			cancel()
			return nil, err
		}
		if err = cmd.Start(); err != nil {
			cancel()
			return nil, fmt.Errorf("start OpenCode: %w", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		ready := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 4096), 1024*1024)
			for scanner.Scan() {
				const prefix = "opencode server listening on "
				line := scanner.Text()
				if strings.HasPrefix(line, prefix) {
					select {
					case ready <- strings.TrimSpace(strings.TrimPrefix(line, prefix)):
					default:
					}
				}
			}
		}()
		cleanup := func() { cancel(); _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done }
		timeout := time.NewTimer(30 * time.Second)
		defer timeout.Stop()
		var base string
		select {
		case base = <-ready:
		case err = <-done:
			cancel()
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			return nil, fmt.Errorf("OpenCode exited before readiness (%v); check pinned CLI installation and native provider configuration", err)
		case <-ctx.Done():
			cleanup()
			return nil, ctx.Err()
		case <-timeout.C:
			cleanup()
			return nil, errors.New("OpenCode did not report localhost readiness within 30s")
		}
		parsed, err := url.Parse(base)
		if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
			cleanup()
			return nil, errors.New("OpenCode reported a non-local server address")
		}
		s := &server{base: base, password: password, client: &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, stop: cleanup, cwd: agent.Cwd}
		var health struct {
			Healthy bool   `json:"healthy"`
			Version string `json:"version"`
		}
		healthCtx, healthCancel := context.WithTimeout(ctx, 10*time.Second)
		defer healthCancel()
		if err = s.call(healthCtx, "GET", "/global/health", nil, &health); err != nil {
			cleanup()
			return nil, fmt.Errorf("OpenCode health check: %w", err)
		}
		if !health.Healthy || health.Version != "1.19.3" {
			cleanup()
			return nil, fmt.Errorf("OpenCode requires healthy pinned server 1.19.3 (reported %q)", health.Version)
		}
		return s, nil
	}
}
func environment(in []string, values map[string]string) []string {
	out := make([]string, 0, len(in)+len(values))
	for _, entry := range in {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := values[key]; !ok {
			out = append(out, entry)
		}
	}
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}
