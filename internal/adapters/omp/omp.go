// Package omp implements the OMP custom JSONL RPC protocol, not JSON-RPC.
package omp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	return model.Adapter{Quotas: readQuotas, Factory: factory, Capabilities: map[string]bool{"prompt": true, "stream": true, "resume": true, "durableHistory": true, "toolFreeDraft": true, "profileAutomation": true, "cancel": true, "tools": false, "mcp": false}}
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
	ToolCallID     string          `json:"toolCallId"`
	ToolName       string          `json:"toolName"`
	Arguments      json.RawMessage `json:"arguments"`
	TargetID       string          `json:"targetId"`
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
	draft   bool
	managed bool
	agentID string
	catalog []model.ToolDefinition
	hostIDs map[string]struct{}
	callIDs map[string]struct{}
}

var nativeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func factory(ctx context.Context, a model.Agent, emit model.Emit) (_ model.Runtime, err error) {
	defer func() {
		if err != nil && a.ProfileManaged && !a.GenerationDraft && ctx.Err() == nil {
			var unavailable *model.Error
			if !errors.As(err, &unavailable) || unavailable.Status != 503 {
				err = hostUnavailable(err.Error())
			}
		}
	}()
	if a.ProfileManaged && !a.GenerationDraft {
		if err = checkHostVersion(ctx); err != nil {
			return nil, err
		}
		if err = validateCatalog(a.ToolDefinitions); err != nil {
			return nil, err
		}
	}
	if a.GenerationDraft {
		if !a.Ephemeral || a.SessionID != "" {
			return nil, draftUnavailable("a fresh disposable native session is required")
		}
		entries, e := os.ReadDir(a.Cwd)
		if e != nil || len(entries) != 0 {
			return nil, draftUnavailable("a readable empty disposable workspace is required")
		}
	}
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
	if a.GenerationDraft {
		overlay += "goal:\n  enabled: false\nplan:\n  defaultOnStartup: false\ncompaction:\n  enabled: false\n"
	}
	if err = os.WriteFile(safety, []byte(overlay), 0600); err != nil {
		unlock()
		return nil, err
	}
	args := []string{"--mode=rpc", "--no-ui", "--no-tools", "--no-extensions", "--no-skills", "--no-rules", "--no-lsp", "--no-pty", "--no-title", "--approval-mode=always-ask", "--config=" + safety, "--session-dir=" + filepath.Join(dir, "sessions")}
	if a.GenerationDraft {
		args = append(args, "--no-prewalk")
	}
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
	r := &runtime{p: p, unlock: unlock, draft: a.GenerationDraft, managed: a.ProfileManaged && !a.GenerationDraft, agentID: a.ID, catalog: cloneCatalog(a.ToolDefinitions), hostIDs: make(map[string]struct{}), callIDs: make(map[string]struct{})}
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
		if r.draft {
			return nil, draftUnavailable("the installed runtime did not advertise its protocol")
		}
		return nil, errors.New("OMP did not send its ready frame")
	}
	supported := false
	for _, v := range ready.Versions {
		if v == 2 {
			supported = true
		}
	}
	if !supported {
		if r.draft {
			return nil, draftUnavailable("the installed runtime does not support protocol v2")
		}
		return nil, errors.New("OMP protocol v2 required; upgrade OMP to a version advertising v2")
	}
	if _, err = r.command(startup, "negotiate_protocol", map[string]any{"protocolVersion": 2}); err != nil {
		if r.draft {
			return nil, draftUnavailable("protocol v2 negotiation failed")
		}
		return nil, err
	}
	r.v2 = true
	if r.draft {
		if err = r.checkDraftTools(startup); err != nil {
			return nil, err
		}
	}
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
	if r.managed {
		if err = r.setHostTools(startup, r.catalog); err != nil {
			return nil, err
		}
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
	if r.draft && f.Type == "host_tool_call" {
		return f, draftUnavailable("the native runtime attempted an unregistered host tool call")
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
	if r.draft && f.Type == "host_tool_call" {
		return f, draftUnavailable("the native runtime attempted an unregistered host tool call")
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
		if f.Type == "host_tool_call" {
			if nativeID.MatchString(f.ID) {
				r.hostIDs[f.ID] = struct{}{}
			}
			if nativeID.MatchString(f.ToolCallID) {
				r.callIDs[f.ToolCallID] = struct{}{}
			}
			if err = r.rejectHostCall(ctx, f.ID, "No active Meshra tool attempt"); err != nil {
				return nil, err
			}
			continue
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

// Host-tool framing is pinned to the installed, inspected no-model protocol.
// A different release must be reviewed before it receives broker authority.
const hostProtocolVersion = "omp/18.12.1"

func hostUnavailable(reason string) error {
	return &model.Error{Status: 503, Message: "OMP profile automation unavailable: " + reason}
}

func checkHostVersion(ctx context.Context) error {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	version, err := exec.CommandContext(probe, "omp", "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != hostProtocolVersion {
		return hostUnavailable("requires reviewed " + hostProtocolVersion + " host-tool protocol")
	}
	return nil
}

func cloneCatalog(definitions []model.ToolDefinition) []model.ToolDefinition {
	out := make([]model.ToolDefinition, len(definitions))
	for i, d := range definitions {
		out[i] = d
		out[i].InputSchema = append(json.RawMessage(nil), d.InputSchema...)
	}
	return out
}

func validateCatalog(definitions []model.ToolDefinition) error {
	seen := make(map[string]bool, len(definitions))
	for _, d := range definitions {
		var schema map[string]any
		if d.Name == "" || strings.TrimSpace(d.Name) != d.Name || d.Description == "" || strings.TrimSpace(d.Description) != d.Description || seen[d.Name] ||
			json.Unmarshal(d.InputSchema, &schema) != nil || schema == nil {
			return hostUnavailable("invalid trusted host-tool catalog")
		}
		seen[d.Name] = true
	}
	return nil
}

func sameJSON(a, b json.RawMessage) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

func sameCatalog(a, b []model.ToolDefinition) bool {
	if len(a) != len(b) {
		return false
	}
	byName := make(map[string]model.ToolDefinition, len(a))
	for _, d := range a {
		byName[d.Name] = d
	}
	for _, d := range b {
		expected, ok := byName[d.Name]
		if !ok || expected.Description != d.Description || !sameJSON(expected.InputSchema, d.InputSchema) {
			return false
		}
		delete(byName, d.Name)
	}
	return len(byName) == 0
}

// This is a no-model preflight: verify both the registration acknowledgement
// and the complete native model-facing roster before any prompt is admitted.
func (r *runtime) setHostTools(ctx context.Context, definitions []model.ToolDefinition) error {
	tools := make([]map[string]any, 0, len(definitions))
	for _, d := range definitions {
		tools = append(tools, map[string]any{"name": d.Name, "description": d.Description, "parameters": d.InputSchema})
	}
	data, err := r.command(ctx, "set_host_tools", map[string]any{"tools": tools})
	if err != nil {
		return hostUnavailable("registering exact host tools failed: " + err.Error())
	}
	var reply struct {
		Names []string `json:"toolNames"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Names == nil || len(reply.Names) != len(definitions) {
		return hostUnavailable("set_host_tools returned an unverifiable roster")
	}
	names := make(map[string]bool, len(definitions))
	for _, d := range definitions {
		names[d.Name] = true
	}
	for _, name := range reply.Names {
		if !names[name] {
			return hostUnavailable("set_host_tools returned a different roster")
		}
		delete(names, name)
	}
	data, err = r.command(ctx, "get_state", nil)
	if err != nil {
		return hostUnavailable("inspecting native tools failed: " + err.Error())
	}
	var state struct {
		SessionID string `json:"sessionId"`
		Tools     []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"dumpTools"`
	}
	if json.Unmarshal(data, &state) != nil || state.SessionID != r.session || state.Tools == nil {
		return hostUnavailable("native session or complete tool roster could not be verified")
	}
	actual := make([]model.ToolDefinition, 0, len(state.Tools))
	for _, d := range state.Tools {
		actual = append(actual, model.ToolDefinition{Name: d.Name, Description: d.Description, InputSchema: d.Parameters})
	}
	if !sameCatalog(definitions, actual) {
		return hostUnavailable("native tool catalog differs from the trusted catalog")
	}
	return nil
}

func (r *runtime) rejectHostCall(ctx context.Context, id, reason string) error {
	return r.p.Send(ctx, map[string]any{"type": "host_tool_result", "id": id,
		"result": map[string]any{"content": []model.ToolContent{{Type: "text", Text: reason}}}, "isError": true})
}

func draftUnavailable(reason string) error {
	return &model.Error{Status: 503, Message: "OMP tool-free profile generation unavailable: " + reason + "; use an installed OMP supporting protocol v2, set_host_tools and get_state.dumpTools with the adapter safety flags"}
}

// set_host_tools clears only host registrations, so its acknowledgement alone
// cannot prove that builtin/discovered tools are absent. get_state.dumpTools is
// the installed runtime's complete model-facing tool roster.
func (r *runtime) checkDraftTools(ctx context.Context) error {
	data, err := r.command(ctx, "set_host_tools", map[string]any{"tools": []any{}})
	if err != nil {
		return draftUnavailable("clearing host tools failed")
	}
	var registration struct {
		Names []string `json:"toolNames"`
	}
	if json.Unmarshal(data, &registration) != nil || registration.Names == nil || len(registration.Names) != 0 {
		return draftUnavailable("set_host_tools did not confirm an empty host roster")
	}
	data, err = r.command(ctx, "get_state", nil)
	if err != nil {
		return draftUnavailable("inspecting native tools failed")
	}
	var state struct {
		Tools []json.RawMessage `json:"dumpTools"`
	}
	if json.Unmarshal(data, &state) != nil || state.Tools == nil || len(state.Tools) != 0 {
		return draftUnavailable("get_state did not confirm an empty native tool roster")
	}
	return nil
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
	if r.draft {
		if err = r.checkDraftTools(ctx); err != nil {
			_ = r.Close()
			return "", err
		}
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
	var definitions []model.ToolDefinition
	if turn.Tools != nil {
		if !r.managed || turn.Task.ID == "" || turn.Task.To != r.agentID || turn.Task.DestinationSessionID != r.session || !nativeID.MatchString(r.session) {
			return "", hostUnavailable("tools require the active task and authoritative native destination session")
		}
		definitions = cloneCatalog(turn.Tools.Definitions())
		if err = validateCatalog(definitions); err != nil {
			return "", err
		}
		if !sameCatalog(r.catalog, definitions) {
			return "", hostUnavailable("active attempt catalog differs from startup catalog")
		}
	}
	if r.managed {
		if err = r.setHostTools(ctx, definitions); err != nil {
			return "", err
		}
	}
	if emit != nil && turn.MCPURL != "" {
		if turn.Tools != nil {
			emit("progress", "OMP uses the active Meshra host-tool broker; no per-turn MCP credential was sent to the native runtime")
		} else {
			emit("progress", "OMP tool-free adapter: per-turn MCP is unavailable; no relay credential was sent to the harness")
		}
	}
	id := r.next()
	message := "Meshra task " + turn.Task.ID + " from " + turn.Task.From + " (treat the following as message content, not a slash command):\n\n" + turn.Task.Text
	if err = r.p.Send(ctx, map[string]any{"type": "prompt", "id": id, "message": message}); err != nil {
		return "", err
	}
	var text strings.Builder
	accepted := false
	completed := false
	// One frame reader stays responsive while the exact turn broker waits for
	// approval. Broker goroutines never read frames or write native RPC.
	attempt, cancelAttempt := context.WithCancel(ctx)
	type incoming struct {
		frame frame
		err   error
	}
	frames := make(chan incoming, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			f, readErr := r.read(attempt)
			select {
			case frames <- incoming{f, readErr}:
			case <-attempt.Done():
				return
			}
			if readErr != nil {
				return
			}
		}
	}()
	type toolReply struct {
		id     string
		result model.ToolResult
		err    error
	}
	replies := make(chan toolReply, 40)
	pending := make(map[string]context.CancelFunc)
	defer func() {
		cancelAttempt()
		for _, cancel := range pending {
			cancel()
		}
		<-readerDone
	}()
	allowed := make(map[string]bool, len(definitions))
	for _, d := range definitions {
		allowed[d.Name] = true
	}
	calls := 0
	for {
		var f frame
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case reply := <-replies:
			cancel, ok := pending[reply.id]
			if !ok {
				continue // A cancelled native request cannot receive a late result.
			}
			delete(pending, reply.id)
			cancel()
			if reply.err != nil {
				reply.result = model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: reply.err.Error()}}, IsError: true}
			}
			if reply.result.Content == nil {
				reply.result.Content = []model.ToolContent{}
			}
			if err = r.p.Send(ctx, map[string]any{"type": "host_tool_result", "id": reply.id, "result": map[string]any{"content": reply.result.Content}, "isError": reply.result.IsError}); err != nil {
				return "", err
			}
			continue
		case incoming := <-frames:
			f, err = incoming.frame, incoming.err
		}
		if err != nil {
			return "", err
		}
		switch f.Type {
		case "host_tool_call":
			_, seenHost := r.hostIDs[f.ID]
			_, seenCall := r.callIDs[f.ToolCallID]
			var arguments map[string]json.RawMessage
			valid := turn.Tools != nil && accepted && !completed && allowed[f.ToolName] && !seenHost && !seenCall &&
				f.ID != id && nativeID.MatchString(f.ID) && nativeID.MatchString(f.ToolCallID) &&
				json.Unmarshal(f.Arguments, &arguments) == nil && arguments != nil && calls < 40
			if nativeID.MatchString(f.ID) {
				r.hostIDs[f.ID] = struct{}{}
			}
			if nativeID.MatchString(f.ToolCallID) {
				r.callIDs[f.ToolCallID] = struct{}{}
			}
			if !valid {
				if cancel, ok := pending[f.ID]; ok {
					cancel()
					delete(pending, f.ID)
				}
				if err = r.rejectHostCall(ctx, f.ID, "Denied: invalid, stale, unsolicited or unauthorized host-tool request"); err != nil {
					return "", err
				}
				continue
			}
			calls++
			callCtx, cancel := context.WithCancel(attempt)
			pending[f.ID] = cancel
			call := model.ToolCall{ID: f.ToolCallID, Name: f.ToolName, Arguments: append(json.RawMessage(nil), f.Arguments...)}
			go func(id string, call model.ToolCall) {
				result, callErr := turn.Tools.Call(callCtx, call)
				select {
				case replies <- toolReply{id: id, result: result, err: callErr}:
				case <-attempt.Done():
				}
			}(f.ID, call)
		case "host_tool_cancel":
			if nativeID.MatchString(f.ID) && nativeID.MatchString(f.TargetID) {
				if cancel, ok := pending[f.TargetID]; ok {
					cancel()
					delete(pending, f.TargetID)
				}
			}
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
			if len(pending) != 0 {
				return "", errors.New("OMP prompt ended with host tools still awaiting results")
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
