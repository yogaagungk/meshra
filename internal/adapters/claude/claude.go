// Package claude owns Claude Agent SDK sessions through the pinned Node bridge.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"meshra/internal/model"
)

type commandFactory func(context.Context) *exec.Cmd

type runtimeSession struct {
	life    context.Context
	agent   model.Agent
	command commandFactory
	mu      sync.Mutex
	session string
	closed  bool
	cancel  context.CancelFunc
	running sync.Mutex
}

// New uses MESHRA_CLAUDE_BRIDGE when set; otherwise it locates the installed
// bridge beside the executable, in the checkout, or relative to this source.
func New() model.Adapter {
	return adapter(func(context.Context) (commandFactory, error) {
		node, err := exec.LookPath("node")
		if err != nil {
			return nil, errors.New("Claude requires Node.js and npm ci in bridges/claude-agent-sdk")
		}
		candidates := []string{os.Getenv("MESHRA_CLAUDE_BRIDGE")}
		if candidates[0] == "" {
			exe, _ := os.Executable()
			_, source, _, _ := runtime.Caller(0)
			candidates = []string{filepath.Join(filepath.Dir(exe), "bridges/claude-agent-sdk/bridge.mjs"), "bridges/claude-agent-sdk/bridge.mjs", filepath.Join(filepath.Dir(source), "../../../bridges/claude-agent-sdk/bridge.mjs")}
		}
		for _, path := range candidates {
			path, err = filepath.Abs(path)
			if err != nil {
				continue
			}
			if _, err = os.Stat(path); err != nil {
				continue
			}
			if _, err = os.Stat(filepath.Join(filepath.Dir(path), "node_modules/@anthropic-ai/claude-agent-sdk/package.json")); err != nil {
				return nil, fmt.Errorf("Claude SDK missing: run npm ci --prefix %s", filepath.Dir(path))
			}
			return func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, node, path) }, nil
		}
		return nil, errors.New("Claude bridge missing: install bridges/claude-agent-sdk beside meshra or set MESHRA_CLAUDE_BRIDGE")
	})
}

func adapter(resolve func(context.Context) (commandFactory, error)) model.Adapter {
	return model.Adapter{Capabilities: map[string]bool{"prompt": true, "stream": true, "resume": true, "cancel": true, "mcp": true, "interactiveApproval": false, "authenticatedSmokeVerified": false}, Factory: func(ctx context.Context, agent model.Agent, emit model.Emit) (model.Runtime, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if agent.Config.Provider != "" && agent.Config.Provider != "anthropic" {
			return nil, errors.New("Claude provider must be anthropic; configure native SDK credentials in the daemon environment")
		}
		command, err := resolve(ctx)
		if err != nil {
			return nil, err
		}
		return &runtimeSession{life: ctx, agent: agent, command: command, session: agent.SessionID}, nil
	}}
}
func (r *runtimeSession) SessionID() string { r.mu.Lock(); defer r.mu.Unlock(); return r.session }
func (r *runtimeSession) Close() error {
	r.mu.Lock()
	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.running.Lock()
	r.running.Unlock()
	return nil
}
func (r *runtimeSession) Run(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
	r.running.Lock()
	defer r.running.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.life, cancel)
	defer stop()
	if err := r.life.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", errors.New("Claude runtime closed")
	}
	r.cancel = cancel
	session := r.session
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.cancel = nil; r.mu.Unlock() }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Prompt    string `json:"prompt"`
		Cwd       string `json:"cwd"`
		SessionID string `json:"sessionId,omitempty"`
		Model     string `json:"model,omitempty"`
		MCPURL    string `json:"mcpURL,omitempty"`
		Token     string `json:"token,omitempty"`
	}{turn.Task.Text, r.agent.Cwd, session, r.agent.Config.Model, turn.MCPURL, turn.Token})
	if err != nil {
		return "", err
	}
	cmd := r.command(ctx)
	cmd.Dir = r.agent.Cwd
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	cmd.Stderr = io.Discard // SDK stderr can include credentials and raw protocol.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err = cmd.Start(); err != nil {
		return "", fmt.Errorf("start Claude SDK bridge: %w", err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	var result string
	var runErr error
	resultSeen := false
	for scanner.Scan() {
		var frame struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			runErr = errors.New("invalid Claude bridge JSONL response")
			cancel()
			break
		}
		switch frame.Type {
		case "session":
			if frame.Data == "" {
				runErr = errors.New("Claude emitted an empty native session")
				cancel()
				break
			}
			r.mu.Lock()
			mismatch := r.session != "" && r.session != frame.Data
			if !mismatch {
				r.session = frame.Data
			}
			r.mu.Unlock()
			if mismatch {
				runErr = errors.New("Claude resumed an unexpected native session")
				cancel()
				break
			}
			if emit != nil {
				emit("session", frame.Data)
			}
		case "delta", "approval_required", "progress":
			if emit != nil {
				emit(frame.Type, redact(frame.Data, turn.Token))
			}
		case "result":
			result = redact(frame.Data, turn.Token)
			resultSeen = true
		case "error":
			runErr = errors.New(redact(frame.Data, turn.Token))
		default:
			runErr = errors.New("unknown Claude bridge response type")
			cancel()
		}
		if runErr != nil {
			cancel()
			break
		}
	}
	if err := scanner.Err(); err != nil && runErr == nil {
		runErr = fmt.Errorf("read Claude bridge: %w", err)
		cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil && runErr == nil {
		return "", ctx.Err()
	}
	if runErr != nil {
		return "", runErr
	}
	if waitErr != nil {
		return "", fmt.Errorf("Claude SDK bridge exited (%w); check native authentication and bridge dependencies", waitErr)
	}
	if !resultSeen {
		return "", errors.New("Claude SDK bridge exited without result")
	}
	if r.SessionID() == "" {
		return "", errors.New("Claude SDK returned no native session ID")
	}
	return result, nil
}
func redact(text, token string) string {
	if token != "" {
		return strings.ReplaceAll(text, token, "[redacted]")
	}
	return text
}
