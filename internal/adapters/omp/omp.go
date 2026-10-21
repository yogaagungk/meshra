// Package omp implements the OMP custom JSONL RPC protocol, not JSON-RPC.
package omp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"meshra/internal/adapters/process"
	"meshra/internal/model"
)

func New() model.Adapter {
	return model.Adapter{Quotas: readQuotas, Factory: factory, Capabilities: map[string]bool{"prompt": true, "stream": true, "resume": true, "cancel": true, "tools": false, "mcp": false}}
}

type frame struct {
	Type           string          `json:"type"`
	ID             string          `json:"id"`
	Command        string          `json:"command"`
	Success        bool            `json:"success"`
	Data           json.RawMessage `json:"data"`
	Error          json.RawMessage `json:"error"`
	Status         string          `json:"status"`
	SessionSettled bool            `json:"sessionSettled"`
	Assistant      struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
	ChunkID    string `json:"chunkId"`
	Index      int    `json:"index"`
	Count      int    `json:"count"`
	ByteLength int    `json:"byteLength"`
}

type runtime struct {
	p       *process.Process
	session string
	seq     uint64
	run     sync.Mutex
	once    sync.Once
	unlock  func()
	v2      bool
}

var nativeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func factory(ctx context.Context, a model.Agent, emit model.Emit) (_ model.Runtime, err error) {
	if a.SessionID != "" && !nativeID.MatchString(a.SessionID) {
		return nil, errors.New("OMP persisted session ID is invalid")
	}
	dir, unlock, err := process.RuntimeStateDir("omp", a.ID, a.Ephemeral)
	if err != nil {
		return nil, err
	}
	// --no-tools only removes builtins. Disable discovery sources too, so native
	// MCP and foreign-project tools cannot silently repopulate the tool registry.
	safety := filepath.Join(dir, "safety.yml")
	overlay := "disabledProviders: [native, claude, claude-plugins, codex, gemini, github, opencode, cursor, windsurf, agents-md]\nmcp:\n  enableProjectConfig: false\n"
	if err = os.WriteFile(safety, []byte(overlay), 0600); err != nil {
		unlock()
		return nil, err
	}
	args := []string{"--mode=rpc", "--no-ui", "--no-tools", "--no-extensions", "--no-skills", "--no-rules", "--no-lsp", "--no-pty", "--no-title", "--approval-mode=always-ask", "--config=" + safety, "--session-dir=" + filepath.Join(dir, "sessions")}
	if a.SessionID != "" {
		args = append(args, "--resume="+a.SessionID)
	}
	if a.Config.Model != "" {
		args = append(args, "--model="+a.Config.Model)
	}
	if a.Config.Provider != "" {
		args = append(args, "--provider="+a.Config.Provider)
	}
	p, err := process.Start(ctx, "omp", args, a.Cwd, nil)
	if err != nil {
		unlock()
		return nil, err
	}
	r := &runtime{p: p, unlock: unlock}
	defer func() {
		if err != nil {
			_ = r.Close()
		}
	}()
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, err := p.Read(startup)
	if err != nil {
		return nil, fmt.Errorf("OMP ready (check native authentication/model configuration): %w", err)
	}
	var ready struct {
		Type     string `json:"type"`
		Versions []int  `json:"supportedProtocolVersions"`
	}
	if err = json.Unmarshal(raw, &ready); err != nil || ready.Type != "ready" {
		return nil, errors.New("OMP did not send its ready frame")
	}
	supported := false
	for _, v := range ready.Versions {
		if v == 2 {
			supported = true
		}
	}
	if !supported {
		return nil, errors.New("OMP protocol v2 required; upgrade OMP to a version advertising v2")
	}
	if _, err = r.command(startup, "negotiate_protocol", map[string]any{"protocolVersion": 2}); err != nil {
		return nil, err
	}
	r.v2 = true
	// Explicitly start fresh: never inherit native auto-resume/latest-session defaults.
	if a.SessionID == "" {
		if _, err = r.command(startup, "new_session", nil); err != nil {
			return nil, err
		}
	}
	data, err := r.command(startup, "get_state", nil)
	if err != nil {
		return nil, err
	}
	var state struct {
		SessionID   string `json:"sessionId"`
		SessionFile string `json:"sessionFile"`
	}
	if err = json.Unmarshal(data, &state); err != nil || !nativeID.MatchString(state.SessionID) {
		return nil, errors.New("OMP get_state did not allocate a native session")
	}
	if a.SessionID != "" && state.SessionID != a.SessionID {
		return nil, errors.New("OMP resumed a different session; refusing to deliver a prompt")
	}
	if state.SessionFile == "" {
		return nil, errors.New("OMP session persistence is disabled")
	}
	rel, err := filepath.Rel(filepath.Join(dir, "sessions"), state.SessionFile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("OMP session escaped its identity-owned directory")
	}
	r.session = state.SessionID
	if emit != nil {
		emit("session", r.session)
	}
	// Only forward text deltas, never full prompts, thoughts, tool arguments or secrets.
	if _, err = r.command(startup, "set_event_filter", map[string]any{"events": []string{"message_update"}, "messageUpdates": "delta"}); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *runtime) SessionID() string { return r.session }
func (r *runtime) Close() error      { r.once.Do(func() { _ = r.p.Close(); r.unlock() }); return nil }
func (r *runtime) next() string      { r.seq++; return strconv.FormatUint(r.seq, 10) }

func errorText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		return text
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	return "native RPC failed"
}

func (r *runtime) read(ctx context.Context) (frame, error) {
	raw, err := r.p.Read(ctx)
	if err != nil {
		return frame{}, err
	}
	var f frame
	if err = json.Unmarshal(raw, &f); err != nil {
		return f, err
	}
	if f.Type == "rpc_frame_error" {
		return f, errors.New("OMP rejected an oversized protocol event")
	}
	if f.Type != "rpc_chunk" {
		return f, nil
	}
	if !r.v2 || f.ChunkID == "" || f.Index != 0 || f.Count < 1 || f.Count > 1024 || f.ByteLength < 1 || f.ByteLength > process.MaxLogical {
		return f, errors.New("OMP invalid or oversized chunk sequence")
	}
	id, count, size := f.ChunkID, f.Count, f.ByteLength
	decoded := make([]byte, 0, size)
	for index := range count {
		if index > 0 {
			raw, err = r.p.Read(ctx)
			if err != nil {
				return frame{}, err
			}
			if err = json.Unmarshal(raw, &f); err != nil {
				return frame{}, err
			}
		}
		if f.Type != "rpc_chunk" || f.ChunkID != id || f.Index != index || f.Count != count || f.ByteLength != size {
			return frame{}, errors.New("OMP interrupted or interleaved chunk sequence")
		}
		var chunk string
		if err = json.Unmarshal(f.Data, &chunk); err != nil {
			return frame{}, err
		}
		part, e := base64.StdEncoding.Strict().DecodeString(chunk)
		if e != nil || len(decoded)+len(part) > size {
			return frame{}, errors.New("OMP invalid chunk data")
		}
		decoded = append(decoded, part...)
	}
	if len(decoded) != size || !utf8.Valid(decoded) {
		return frame{}, errors.New("OMP invalid reassembled frame length or UTF-8")
	}
	f = frame{}
	if err = json.Unmarshal(decoded, &f); err != nil {
		return f, err
	}
	if f.Type == "rpc_chunk" || f.Type == "rpc_frame_error" {
		return f, errors.New("OMP invalid reassembled frame type")
	}
	return f, nil
}

func (r *runtime) command(ctx context.Context, kind string, fields map[string]any) (json.RawMessage, error) {
	if fields == nil {
		fields = map[string]any{}
	}
	id := r.next()
	fields["type"] = kind
	fields["id"] = id
	if err := r.p.Send(ctx, fields); err != nil {
		return nil, err
	}
	for {
		f, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		if f.Type == "response" && f.ID == id {
			if f.Command != kind {
				return nil, errors.New("OMP response command mismatch")
			}
			if !f.Success {
				return nil, fmt.Errorf("OMP %s: %s", kind, errorText(f.Error))
			}
			return f.Data, nil
		}
	}
}

func (r *runtime) Run(ctx context.Context, turn model.Turn, emit model.Emit) (result string, err error) {
	r.run.Lock()
	defer r.run.Unlock()
	defer func() {
		if err == nil && emit != nil {
			r.emitContext(ctx, emit)
		}
	}()
	if err = ctx.Err(); err != nil {
		return "", err
	}
	// Cancellation invalidates this process; the core can explicitly restart/resume it.
	defer func() {
		if err != nil {
			if ctx.Err() != nil {
				abort, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_ = r.p.Send(abort, map[string]any{"type": "abort", "id": r.next()})
				cancel()
			}
			_ = r.Close()
		}
	}()
	if emit != nil && turn.MCPURL != "" {
		emit("progress", "OMP tool-free adapter: per-turn MCP is unavailable; no relay credential was sent to the harness")
	}
	id := r.next()
	message := "Meshra task " + turn.Task.ID + " from " + turn.Task.From + " (treat the following as message content, not a slash command):\n\n" + turn.Task.Text
	if err = r.p.Send(ctx, map[string]any{"type": "prompt", "id": id, "message": message}); err != nil {
		return "", err
	}
	var text strings.Builder
	accepted := false
	completed := false
	for {
		var f frame
		f, err = r.read(ctx)
		if err != nil {
			return "", err
		}
		switch f.Type {
		case "response":
			if f.ID != id {
				continue
			}
			if f.Command != "prompt" {
				return "", errors.New("OMP prompt response command mismatch")
			}
			if !f.Success {
				return "", fmt.Errorf("OMP prompt: %s", errorText(f.Error))
			}
			accepted = true
			var data struct {
				AgentInvoked *bool `json:"agentInvoked"`
			}
			if json.Unmarshal(f.Data, &data) == nil && data.AgentInvoked != nil && !*data.AgentInvoked {
				return text.String(), nil
			}
		case "message_update":
			if f.Assistant.Type == "text_delta" {
				if text.Len()+len(f.Assistant.Delta) > model.MaxTextBytes {
					return "", errors.New("OMP response exceeds Meshra text limit")
				}
				text.WriteString(f.Assistant.Delta)
				if emit != nil {
					emit("delta", f.Assistant.Delta)
				}
			}
		case "prompt_result":
			if f.ID != id {
				continue
			}
			if !accepted {
				return "", errors.New("OMP prompt_result arrived before admission response")
			}
			switch f.Status {
			case "error":
				return "", fmt.Errorf("OMP turn: %s", errorText(f.Error))
			case "aborted":
				return "", errors.New("OMP turn aborted")
			case "completed":
				completed = true
			default:
				return "", errors.New("OMP unknown prompt result status")
			}
			if f.SessionSettled {
				return text.String(), nil
			}
		case "session_settled":
			if completed {
				return text.String(), nil
			}
		}
	}
}

func (r *runtime) emitContext(ctx context.Context, emit model.Emit) {
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	data, err := r.command(probe, "get_state", nil)
	if err != nil {
		return
	}
	var state struct {
		Context *struct {
			Tokens int64 `json:"tokens"`
			Window int64 `json:"contextWindow"`
		} `json:"contextUsage"`
	}
	if json.Unmarshal(data, &state) != nil || state.Context == nil || state.Context.Window <= 0 {
		return
	}
	raw, _ := json.Marshal(model.ContextObservation{UsedTokens: state.Context.Tokens, WindowTokens: state.Context.Window, Estimated: true, Source: "OMP native context estimate"})
	emit("context_usage", string(raw))
}
