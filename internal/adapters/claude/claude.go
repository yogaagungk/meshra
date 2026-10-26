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
	catalog []byte
	session string
	closed  bool
	cancel  context.CancelFunc
	running sync.Mutex
}

// New uses MESHRA_CLAUDE_BRIDGE when set; otherwise it locates the installed
// bridge beside the executable, in the checkout, or relative to this source.
func New() model.Adapter {
	resolve := func(context.Context) (commandFactory, error) {
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
	}
	a := adapter(resolve)
	a.Capabilities["toolFreeDraft"] = draftCompatible(resolve)
	a.Capabilities["profileAutomation"] = restrictedCompatible(resolve, true)
	return a
}

// Check only SDK configuration/schema support. No native session or provider
// prompt is opened, and unsupported installations keep their legacy adapter.
func draftCompatible(resolve func(context.Context) (commandFactory, error)) bool {
	return restrictedCompatible(resolve, false)
}

func restrictedCompatible(resolve func(context.Context) (commandFactory, error), profile bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command, err := resolve(ctx)
	if err != nil {
		return false
	}
	cwd, err := os.MkdirTemp("", "meshra-claude-draft-check-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(cwd)
	input := map[string]any{"prompt": "", "cwd": cwd, "generationDraft": true, "checkGenerationDraft": true}
	want := "toolFreeDraft"
	if profile {
		input = map[string]any{"prompt": "", "cwd": cwd, "toolDefinitions": []model.ToolDefinition{}, "checkProfileAutomation": true}
		want = "profileAutomation"
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return false
	}
	cmd := command(ctx)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	var frame struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	err = json.NewDecoder(io.LimitReader(stdout, 4096)).Decode(&frame)
	if err != nil || frame.Type != "result" || frame.Data != want {
		cancel()
		_ = cmd.Wait()
		return false
	}
	return cmd.Wait() == nil
}

func adapter(resolve func(context.Context) (commandFactory, error)) model.Adapter {
	return model.Adapter{Capabilities: map[string]bool{"prompt": true, "stream": true, "resume": true, "durableHistory": true, "toolFreeDraft": true, "profileAutomation": false, "cancel": true, "mcp": true, "interactiveApproval": false, "authenticatedSmokeVerified": false}, Factory: func(ctx context.Context, agent model.Agent, emit model.Emit) (model.Runtime, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if agent.Config.Provider != "" && agent.Config.Provider != "anthropic" {
			return nil, errors.New("Claude provider must be anthropic; configure native SDK credentials in the daemon environment")
		}
		if agent.GenerationDraft && agent.SessionID != "" {
			return nil, errors.New("Claude profile generation requires a fresh disposable session")
		}
		// Managed console chats run the legacy tool-denied path; only profiles need a catalog.
		if agent.ProfileManaged && agent.ToolDefinitions == nil {
			return nil, model.Err(503, "Claude profile automation requires an exact startup broker catalog")
		}
		var catalog []byte
		if agent.ToolDefinitions != nil {
			var err error
			catalog, err = json.Marshal(agent.ToolDefinitions)
			if err != nil {
				return nil, model.Err(503, "Claude profile automation received an invalid startup broker catalog")
			}
		}
		command, err := resolve(ctx)
		if err != nil {
			return nil, err
		}
		return &runtimeSession{life: ctx, agent: agent, command: command, session: agent.SessionID, catalog: catalog}, nil
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
	profile := r.catalog != nil
	var err error
	if profile && (turn.Task.To != r.agent.ID || (turn.Task.DestinationSessionID != "" && turn.Task.DestinationSessionID != session)) {
		return "", model.Err(503, "Claude broker task does not own this native destination session")
	}
	if !profile && turn.Tools != nil {
		return "", model.Err(503, "Claude broker tools require an exact profile-managed startup catalog")
	}
	var toolSocket, toolToken, toolBridge string
	if profile {
		if turn.Tools == nil || emit == nil {
			return "", model.Err(503, "Claude profile automation requires the active-attempt broker and a session persistence emitter")
		}
		current, err := json.Marshal(turn.Tools.Definitions())
		if err != nil || !bytes.Equal(current, r.catalog) {
			return "", model.Err(503, "Claude broker catalog changed; restart the native runtime with the admitted catalog")
		}
		endpoint, ok := turn.Tools.(model.ScopedToolEndpoint)
		if !ok {
			return "", model.Err(503, "Claude profile automation requires a private active-attempt MCP endpoint")
		}
		toolBridge, err = scopedBridge()
		if err != nil {
			return "", err
		}
		toolSocket, toolToken, err = endpoint.ScopedMCP(ctx)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(toolSocket) || !strings.HasSuffix(toolSocket, ".sock") || toolToken == "" || toolToken == turn.Token {
			return "", model.Err(503, "Claude profile automation requires a private scoped socket and a distinct attempt credential")
		}
	}
	mcpURL, token := turn.MCPURL, turn.Token
	if r.agent.GenerationDraft || profile {
		mcpURL, token = "", ""
	}
	payload, err := json.Marshal(struct {
		Prompt          string          `json:"prompt"`
		Cwd             string          `json:"cwd"`
		SessionID       string          `json:"sessionId,omitempty"`
		Model           string          `json:"model,omitempty"`
		MCPURL          string          `json:"mcpURL,omitempty"`
		Token           string          `json:"token,omitempty"`
		GenerationDraft bool            `json:"generationDraft,omitempty"`
		ToolDefinitions json.RawMessage `json:"toolDefinitions,omitempty"`
		ToolSocket      string          `json:"toolSocket,omitempty"`
		ToolToken       string          `json:"toolToken,omitempty"`
		ToolBridge      string          `json:"toolBridge,omitempty"`
	}{turn.Task.Text, r.agent.Cwd, session, r.agent.Config.Model, mcpURL, token, r.agent.GenerationDraft, r.catalog, toolSocket, toolToken, toolBridge})
	if err != nil {
		return "", err
	}
	cmd := r.command(ctx)
	cmd.Dir = r.agent.Cwd
	var control io.WriteCloser
	if profile {
		control, err = cmd.StdinPipe()
		if err != nil {
			return "", err
		}
		defer control.Close()
	} else {
		cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	}
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
	if control != nil {
		if _, err = control.Write(append(payload, '\n')); err != nil {
			cancel()
			_ = cmd.Wait()
			return "", errors.New("write Claude scoped bridge request")
		}
	}
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
		case "context_usage":
			if emit != nil {
				emit("context_usage", frame.Data)
			}
		case "usage":
			if emit != nil {
				emit("usage", frame.Data)
			}
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
				if ctx.Err() != nil {
					runErr = ctx.Err()
					break
				}
				if control != nil {
					if err = json.NewEncoder(control).Encode(map[string]string{"type": "session_ready", "data": frame.Data}); err != nil {
						runErr = errors.New("acknowledge Claude session persistence")
						cancel()
					}
				}
			}
		case "delta", "approval_required", "progress":
			if emit != nil {
				emit(frame.Type, redact(redact(frame.Data, turn.Token), toolToken))
			}
		case "result":
			result = redact(redact(frame.Data, turn.Token), toolToken)
			resultSeen = true
		case "error":
			if ctx.Err() != nil {
				runErr = ctx.Err()
			} else if profile {
				runErr = model.Err(503, "Claude profile automation unavailable: "+redact(redact(frame.Data, turn.Token), toolToken))
			} else {
				runErr = errors.New(redact(frame.Data, turn.Token))
			}
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
		if profile {
			return "", model.Err(503, "Claude profile automation bridge unavailable; check pinned SDK 0.3.303 and native authentication")
		}
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

func scopedBridge() (string, error) {
	exe, _ := os.Executable()
	_, source, _, _ := runtime.Caller(0)
	for _, candidate := range []string{filepath.Join(filepath.Dir(exe), "bridges/mcp-stdio/bridge.mjs"), "bridges/mcp-stdio/bridge.mjs", filepath.Join(filepath.Dir(source), "../../../bridges/mcp-stdio/bridge.mjs")} {
		path, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", model.Err(503, "Claude profile automation requires the reviewed bridges/mcp-stdio/bridge.mjs beside meshra")
}
