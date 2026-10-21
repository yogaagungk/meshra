// Package codex implements the installed Codex App Server JSONL protocol.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"meshra/internal/adapters/process"
	"meshra/internal/model"
)

func New() model.Adapter {
	return model.Adapter{Factory: factory, Capabilities: map[string]bool{"prompt": true, "stream": true, "resume": true, "cancel": true, "read_only": true, "approval": false, "mcp": false}}
}

type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type nativeTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}
type runtime struct {
	p       *process.Process
	session string
	cwd     string
	seq     uint64
	run     sync.Mutex
	once    sync.Once
	unlock  func()
}

func isolatedEnv(dir string) ([]string, error) {
	// Do not inherit user MCP servers, hooks, plugins or unsafe sandbox defaults.
	// Authentication remains native; only auth.json is shared, never config.toml.
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(user, ".codex")
	}
	source := filepath.Join(home, "auth.json")
	dest := filepath.Join(dir, "auth.json")
	if _, err := os.Stat(source); err == nil {
		if _, err = os.Lstat(dest); os.IsNotExist(err) {
			absolute, e := filepath.Abs(source)
			if e != nil {
				return nil, e
			}
			if e = os.Symlink(absolute, dest); e != nil {
				return nil, e
			}
		} else if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CODEX_HOME=") {
			env = append(env, entry)
		}
	}
	return append(env, "CODEX_HOME="+dir), nil
}

func factory(ctx context.Context, a model.Agent, emit model.Emit) (_ model.Runtime, err error) {
	dir, unlock, err := process.RuntimeStateDir("codex", a.ID, a.Ephemeral)
	if err != nil {
		return nil, err
	}
	env, err := isolatedEnv(dir)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("Codex isolated native auth: %w", err)
	}
	p, err := process.Start(ctx, "codex", []string{"app-server", "--listen", "stdio://"}, dir, env)
	if err != nil {
		unlock()
		return nil, err
	}
	r := &runtime{p: p, cwd: a.Cwd, unlock: unlock}
	defer func() {
		if err != nil {
			_ = r.Close()
		}
	}()
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err = r.command(startup, "initialize", map[string]any{"clientInfo": map[string]any{"name": "meshra", "title": "Meshra", "version": "0.1.0"}}, emit); err != nil {
		return nil, fmt.Errorf("Codex initialize: %w", err)
	}
	if err = p.Send(startup, map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	// Resolve project layers without starting a thread. The read-only OS sandbox
	// does not contain external MCP servers or command hooks.
	effective, err := r.command(startup, "config/read", map[string]any{"cwd": a.Cwd, "includeLayers": false}, emit)
	if err != nil {
		return nil, fmt.Errorf("Codex inspect effective project configuration: %w", err)
	}
	var config struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err = json.Unmarshal(effective, &config); err != nil || config.Config == nil {
		return nil, errors.New("Codex config/read returned no effective configuration")
	}
	for _, key := range []string{"mcp_servers", "hooks", "plugins"} {
		raw := config.Config[key]
		if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" || string(raw) == "[]" {
			continue
		}
		return nil, fmt.Errorf("Codex isolated read-only adapter refuses effective %s configuration; use a clean project configuration or a separately managed interactive harness", key)
	}
	params := map[string]any{"cwd": a.Cwd, "sandbox": "read-only", "approvalPolicy": "on-request"}
	if a.Config.Model != "" {
		params["model"] = a.Config.Model
	}
	if a.Config.Provider != "" {
		params["modelProvider"] = a.Config.Provider
	}
	method := "thread/start"
	if a.SessionID != "" {
		method = "thread/resume"
		params["threadId"] = a.SessionID
		params["excludeTurns"] = true
	}
	data, err := r.command(startup, method, params, emit)
	if err != nil {
		return nil, fmt.Errorf("Codex %s (authenticate with codex login; custom provider definitions are not inherited): %w", method, err)
	}
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err = json.Unmarshal(data, &response); err != nil || response.Thread.ID == "" {
		return nil, errors.New("Codex did not allocate a native thread")
	}
	if a.SessionID != "" && response.Thread.ID != a.SessionID {
		return nil, errors.New("Codex resumed a different native thread; refusing delivery")
	}
	r.session = response.Thread.ID
	if emit != nil {
		emit("session", r.session)
	}
	return r, nil
}
func (r *runtime) SessionID() string { return r.session }
func (r *runtime) Close() error      { r.once.Do(func() { _ = r.p.Close(); r.unlock() }); return nil }
func (r *runtime) next() string      { r.seq++; return strconv.FormatUint(r.seq, 10) }
func matches(raw json.RawMessage, id string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value == id
}
func (r *runtime) read(ctx context.Context) (frame, error) {
	raw, err := r.p.Read(ctx)
	if err != nil {
		return frame{}, err
	}
	var f frame
	err = json.Unmarshal(raw, &f)
	return f, err
}

// All server requests are answered. Unknown requests fail closed, rather than hang
// or approve an operation with a guessed response shape.
func (r *runtime) deny(ctx context.Context, f frame, emit model.Emit) error {
	if emit != nil {
		emit("approval_required", "Codex denied server request: "+f.Method+"; this adapter does not grant approvals")
	}
	response := map[string]any{"id": f.ID}
	switch f.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		response["result"] = map[string]any{"decision": "decline"}
	case "item/permissions/requestApproval":
		response["result"] = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
	case "mcpServer/elicitation/request":
		response["result"] = map[string]any{"action": "decline"}
	default:
		response["error"] = map[string]any{"code": -32601, "message": "Meshra denies unsupported server requests"}
	}
	return r.p.Send(ctx, response)
}
func (r *runtime) command(ctx context.Context, method string, params any, emit model.Emit) (json.RawMessage, error) {
	id := r.next()
	if err := r.p.Send(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		f, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		if f.Method != "" && len(f.ID) > 0 {
			if err = r.deny(ctx, f, emit); err != nil {
				return nil, err
			}
			continue
		}
		if matches(f.ID, id) {
			if f.Error != nil {
				return nil, fmt.Errorf("%s: %s (code %d)", method, f.Error.Message, f.Error.Code)
			}
			if f.Result == nil {
				return nil, errors.New("Codex response missing result")
			}
			return f.Result, nil
		}
	}
}

func (r *runtime) Run(ctx context.Context, turn model.Turn, emit model.Emit) (result string, err error) {
	r.run.Lock()
	defer r.run.Unlock()
	if err = ctx.Err(); err != nil {
		return "", err
	}
	active := ""
	defer func() {
		if err != nil {
			if ctx.Err() != nil && active != "" {
				abort, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_ = r.p.Send(abort, map[string]any{"id": r.next(), "method": "turn/interrupt", "params": map[string]any{"threadId": r.session, "turnId": active}})
				cancel()
			}
			_ = r.Close()
		}
	}()
	if emit != nil && turn.MCPURL != "" {
		emit("progress", "Codex isolated read-only adapter: per-turn MCP is unavailable; no relay credential was sent to the harness")
	}
	requestID := r.next()
	params := map[string]any{"threadId": r.session, "cwd": r.cwd, "approvalPolicy": "on-request", "sandboxPolicy": map[string]any{"type": "readOnly", "networkAccess": false}, "input": []any{map[string]any{"type": "text", "text": "Meshra task " + turn.Task.ID + " from " + turn.Task.From + ":\n\n" + turn.Task.Text}}}
	if err = r.p.Send(ctx, map[string]any{"id": requestID, "method": "turn/start", "params": params}); err != nil {
		return "", err
	}
	accepted := false
	var terminal *nativeTurn
	// A resumed thread can emit old-turn events before turn/start responds.
	// Buffer early relevant events, then match only the authoritative response ID.
	var pending []frame
	pendingBytes := 0
	texts := map[string]string{}
	order := []string{}
	add := func(id, text string, replace bool) error {
		old, exists := texts[id]
		if !exists {
			order = append(order, id)
		}
		if replace {
			texts[id] = text
		} else {
			texts[id] = old + text
		}
		size := 0
		for _, v := range texts {
			size += len(v) + 1
		}
		if size > model.MaxTextBytes {
			return errors.New("Codex response exceeds Meshra text limit")
		}
		return nil
	}
	for {
		if accepted && terminal != nil && len(pending) == 0 {
			switch terminal.Status {
			case "completed":
				parts := make([]string, 0, len(order))
				for _, id := range order {
					parts = append(parts, texts[id])
				}
				return strings.Join(parts, "\n"), nil
			case "interrupted":
				return "", errors.New("Codex turn interrupted")
			case "failed":
				if terminal.Error != nil {
					return "", fmt.Errorf("Codex turn: %s", terminal.Error.Message)
				}
				return "", errors.New("Codex turn failed without error details")
			default:
				return "", fmt.Errorf("Codex invalid terminal status %q", terminal.Status)
			}
		}
		var f frame
		if accepted && len(pending) > 0 {
			f = pending[0]
			pending = pending[1:]
		} else {
			f, err = r.read(ctx)
			if err != nil {
				return "", err
			}
		}
		if f.Method != "" && len(f.ID) > 0 {
			if err = r.deny(ctx, f, emit); err != nil {
				return "", err
			}
			continue
		}
		if matches(f.ID, requestID) {
			if f.Error != nil {
				return "", fmt.Errorf("Codex turn/start: %s", f.Error.Message)
			}
			var response struct {
				Turn nativeTurn `json:"turn"`
			}
			if json.Unmarshal(f.Result, &response) != nil || response.Turn.ID == "" {
				return "", errors.New("Codex turn/start response missing turn ID")
			}
			if active != "" && active != response.Turn.ID {
				return "", errors.New("Codex turn ownership mismatch")
			}
			active = response.Turn.ID
			accepted = true
			if response.Turn.Status != "inProgress" && response.Turn.Status != "" {
				terminal = &response.Turn
			}
			continue
		}
		var event struct {
			ThreadID string     `json:"threadId"`
			TurnID   string     `json:"turnId"`
			ItemID   string     `json:"itemId"`
			Delta    string     `json:"delta"`
			Turn     nativeTurn `json:"turn"`
			Item     struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"item"`
			TokenUsage *struct {
				Last *struct {
					Input  int64  `json:"inputTokens"`
					Output int64  `json:"outputTokens"`
					Cached int64  `json:"cachedInputTokens"`
					Total  *int64 `json:"totalTokens"`
				} `json:"last"`
				Window *int64 `json:"modelContextWindow"`
			} `json:"tokenUsage"`
			WillRetry bool `json:"willRetry"`
			Error     struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		switch f.Method {
		case "thread/tokenUsage/updated", "item/agentMessage/delta", "item/completed", "turn/completed", "error":
		default:
			continue
		}
		if err = json.Unmarshal(f.Params, &event); err != nil {
			return "", err
		}
		if event.ThreadID != r.session {
			continue
		}
		eventTurn := event.TurnID
		if event.Turn.ID != "" {
			eventTurn = event.Turn.ID
		}
		if eventTurn == "" {
			continue
		}
		if !accepted {
			pendingBytes += len(f.Params)
			if len(pending) >= 64 || pendingBytes > process.MaxLogical {
				return "", errors.New("Codex early turn events exceed transport bounds")
			}
			pending = append(pending, f)
			continue
		}
		if eventTurn != active {
			continue
		}
		switch f.Method {
		case "thread/tokenUsage/updated":
			if emit != nil && event.TokenUsage != nil && event.TokenUsage.Last != nil {
				data, _ := json.Marshal(map[string]int64{"inputTokens": event.TokenUsage.Last.Input, "outputTokens": event.TokenUsage.Last.Output, "cachedInputTokens": event.TokenUsage.Last.Cached})
				emit("usage", string(data))
				if event.TokenUsage.Last.Total != nil && event.TokenUsage.Window != nil && *event.TokenUsage.Window > 0 {
					context, _ := json.Marshal(model.ContextObservation{UsedTokens: *event.TokenUsage.Last.Total, WindowTokens: *event.TokenUsage.Window, Source: "Codex native token usage"})
					emit("context_usage", string(context))
				}
			}
		case "item/agentMessage/delta":
			if event.ItemID == "" {
				return "", errors.New("Codex delta missing item ID")
			}
			if err = add(event.ItemID, event.Delta, false); err != nil {
				return "", err
			}
			if emit != nil {
				emit("delta", event.Delta)
			}
		case "item/completed":
			if event.Item.Type == "agentMessage" {
				if event.Item.ID == "" {
					return "", errors.New("Codex agent message missing item ID")
				}
				if err = add(event.Item.ID, event.Item.Text, true); err != nil {
					return "", err
				}
			}
		case "turn/completed":
			copy := event.Turn
			terminal = &copy
		case "error":
			if !event.WillRetry {
				return "", fmt.Errorf("Codex stream: %s", event.Error.Message)
			}
		}
	}
}
