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
	return model.Adapter{Factory: factory, Capabilities: map[string]bool{"prompt": true, "stream": true, "resume": true, "durableHistory": true, "toolFreeDraft": false, "cancel": true, "read_only": false, "approval": false, "mcp": false, "profileAutomation": false}}
}

// The installed 0.163 app-server supports dynamic-tool protocol frames, but
// native apply_patch exclusion and rooted-only native reads remain unverified.
// Do not advertise profileAutomation until both restrictions can be enforced.
// The frame implementation below is covered independently by protocol fixtures;
// those fixtures are not evidence of native filesystem confinement.
var nativeToolDisable = []string{"shell_tool", "unified_exec", "unified_exec_tty", "code_mode", "code_mode_host", "code_mode_interrupt", "view_image", "multi_agent", "browser_use", "browser_use_external", "browser_use_full_cdp_access", "computer_use", "in_app_browser", "image_generation", "sleep_tool", "apps", "plugins", "remote_plugin", "codex_apps_mcp_2026_07_28", "skill_search", "skill_mcp_dependency_install", "guardian_approval", "hooks", "tool_suggest", "realtime_conversation", "browser_annotation_api", "write_stdin_approval", "tool_call_mcp_elicitation"}

// dynamicToolSpec is the installed app-server shape for one registered tool.
type dynamicToolSpec struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// toolCallParams is the installed app-server shape for a server tool request.
type toolCallParams struct {
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId"`
	CallID    string          `json:"callId"`
	Tool      string          `json:"tool"`
	Namespace *string         `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"`
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
	profile bool
	catalog map[string]bool
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
	// The installed 0.163 app-server serves an experimental client tool catalog:
	// thread/start registers dynamic tools and the server then asks this client
	// to run one with item/tool/call. A profile-managed turn uses that catalog
	// and disables every native tool surface, so the Meshra broker is the only
	// route to files, commands and the network. A draft has no broker and no
	// tool roster, so it stays unavailable on this harness.
	if a.GenerationDraft {
		return nil, &model.Error{Status: 503, Message: "Codex profile generation is unavailable: this adapter cannot enforce an empty native tool catalog on the installed app-server protocol. Use a harness advertising toolFreeDraft; ordinary Codex sessions are unchanged"}
	}
	dir, unlock, err := process.RuntimeStateDir("codex", a.ID, a.Ephemeral)
	if err != nil {
		return nil, err
	}
	env, err := isolatedEnv(dir)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("Codex isolated native auth: %w", err)
	}
	// A profile-managed thread serves the Meshra broker catalog only. Every
	// native tool surface is disabled and the effective state is verified below.
	profile := a.ToolDefinitions != nil
	specs, catalog, specErr := dynamicToolCatalog(a.ToolDefinitions)
	if specErr != nil {
		unlock()
		return nil, specErr
	}
	args := []string{"app-server", "--listen", "stdio://"}
	if profile {
		for _, name := range nativeToolDisable {
			args = append(args, "--disable", name)
		}
		args = append(args, "-c", "tools.web_search=false")
	}
	p, err := process.Start(ctx, "codex", args, dir, env)
	if err != nil {
		unlock()
		return nil, err
	}
	r := &runtime{p: p, cwd: a.Cwd, unlock: unlock, profile: profile, catalog: catalog}
	defer func() {
		if err != nil {
			_ = r.Close()
		}
	}()
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	initialize := map[string]any{"clientInfo": map[string]any{"name": "meshra", "title": "Meshra", "version": "0.1.0"}}
	if profile {
		// The dynamic tool catalog is an experimental server API on this version.
		initialize["capabilities"] = map[string]any{"experimentalApi": true}
	}
	initialized, err := r.command(startup, "initialize", initialize, emit)
	if err != nil {
		return nil, fmt.Errorf("Codex initialize: %w", err)
	}
	if profile {
		if err = requireDynamicToolSupport(initialized); err != nil {
			return nil, err
		}
	}
	if err = p.Send(startup, map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	if profile {
		// Native tools are gone, so the native working directory carries no
		// workspace content. The broker is the only route to profile files.
		workspace := filepath.Join(dir, "workspace")
		if err = os.MkdirAll(workspace, 0700); err != nil {
			return nil, fmt.Errorf("Codex profile workspace isolation: %w", err)
		}
		r.cwd = workspace
	}
	// Resolve project layers without starting a thread. The read-only OS sandbox
	// does not contain external MCP servers or command hooks.
	configCwd := a.Cwd
	if profile {
		// The native project root is the isolated directory, so an operator
		// project configuration is neither loaded nor trusted here.
		configCwd = r.cwd
	}
	effective, err := r.command(startup, "config/read", map[string]any{"cwd": configCwd, "includeLayers": false}, emit)
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
		return nil, fmt.Errorf("Codex isolated adapter refuses effective %s configuration; use a clean project configuration or a separately managed interactive harness", key)
	}
	if profile {
		if err = verifyNativeToolsDisabled(config.Config); err != nil {
			return nil, err
		}
	}
	// Full access: no OS sandbox and no approval prompts.
	params := map[string]any{"cwd": r.cwd, "sandbox": "danger-full-access", "approvalPolicy": "never"}
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
	if profile && len(specs) > 0 && method == "thread/start" {
		// The catalog is registered once, when the thread is created. A resumed
		// thread restores the same catalog from its own rollout session metadata.
		params["dynamicTools"] = specs
	}
	data, err := r.command(startup, method, params, emit)
	if err != nil {
		if profile && method == "thread/start" && dynamicToolUnsupported(err) {
			return nil, &model.Error{Status: 503, Message: "Codex profile automation is unavailable: the installed app-server rejected the dynamic tool catalog (" + err.Error() + "); install Codex 0.163.0 or newer"}
		}
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

// All server requests are answered. Full access accepts command and file
// approvals; unknown requests fail closed rather than guess a response shape.
func (r *runtime) deny(ctx context.Context, f frame, emit model.Emit) error {
	response := map[string]any{"id": f.ID}
	switch f.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		response["result"] = map[string]any{"decision": "accept"}
		return r.p.Send(ctx, response)
	}
	if emit != nil {
		emit("approval_required", "Codex denied server request: "+f.Method+"; this adapter does not grant approvals")
	}
	switch f.Method {
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
	// A broker catalog is only served by a thread that registered it. A turn
	// that expects tools without a registered catalog is refused, never run
	// with a silently empty tool surface.
	if r.profile && len(r.catalog) > 0 && turn.Tools == nil {
		return "", &model.Error{Status: 503, Message: "Codex profile automation requires the Meshra broker executor for this turn; no tool catalog was bound to the turn"}
	}
	if !r.profile && turn.Tools != nil {
		return "", &model.Error{Status: 503, Message: "Codex broker tools require a profile-managed thread; this native session was started without a registered tool catalog"}
	}
	if r.profile && turn.Tools != nil && emit != nil {
		// The native catalog is registered when the thread is created. A profile
		// edited afterwards can admit a different catalog; the difference stays
		// visible and every call outside the registered catalog is refused.
		if missing := unregisteredTools(turn.Tools.Definitions(), r.catalog); len(missing) > 0 {
			emit("progress", "Codex native tool catalog and the admitted broker catalog differ; tools outside the registered catalog are refused: "+strings.Join(missing, ", "))
		}
	}
	active := ""
	var calls sync.WaitGroup
	defer func() {
		if err != nil {
			if ctx.Err() != nil && active != "" {
				abort, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_ = r.p.Send(abort, map[string]any{"id": r.next(), "method": "turn/interrupt", "params": map[string]any{"threadId": r.session, "turnId": active}})
				cancel()
			}
		}
		waitToolCalls(&calls)
		if err != nil {
			_ = r.Close()
		}
	}()
	if emit != nil && turn.MCPURL != "" {
		emit("progress", "Codex isolated adapter: per-turn MCP is unavailable; no relay credential was sent to the harness")
	}
	requestID := r.next()
	params := map[string]any{"threadId": r.session, "cwd": r.cwd, "approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "dangerFullAccess"}, "input": []any{map[string]any{"type": "text", "text": "Meshra task " + turn.Task.ID + " from " + turn.Task.From + ":\n\n" + turn.Task.Text}}}
	if err = r.p.Send(ctx, map[string]any{"id": requestID, "method": "turn/start", "params": params}); err != nil {
		return "", err
	}
	accepted := false
	var terminal *nativeTurn
	var toolRequests []frame
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
			if r.profile && f.Method == "item/tool/call" {
				if active == "" {
					// The tool request can arrive before the turn response that
					// names the active turn. Buffer it; never run it unverified.
					if len(toolRequests) >= 32 {
						r.refuseToolCall(ctx, f, "too many unverified tool calls are pending")
					} else {
						toolRequests = append(toolRequests, f)
					}
				} else {
					r.toolCall(ctx, f, turn, emit, active, &calls)
				}
				continue
			}
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
			for _, request := range toolRequests {
				r.toolCall(ctx, request, turn, emit, active, &calls)
			}
			toolRequests = nil
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

// dynamicToolCatalog converts the Meshra broker catalog into the installed
// app-server registration shape. Only the tool name, description and schema
// cross the boundary; identity, policy and workspace stay with the broker.
func dynamicToolCatalog(definitions []model.ToolDefinition) ([]dynamicToolSpec, map[string]bool, error) {
	specs := make([]dynamicToolSpec, 0, len(definitions))
	catalog := map[string]bool{}
	for _, definition := range definitions {
		if !validToolName(definition.Name) {
			return nil, nil, fmt.Errorf("Codex broker catalog contains an unusable tool name %q", definition.Name)
		}
		schema := definition.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		if !json.Valid(schema) {
			return nil, nil, fmt.Errorf("Codex broker catalog tool %q has an invalid input schema", definition.Name)
		}
		specs = append(specs, dynamicToolSpec{Type: "function", Name: definition.Name, Description: definition.Description, InputSchema: schema})
		catalog[definition.Name] = true
	}
	return specs, catalog, nil
}

func validToolName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// requireDynamicToolSupport refuses an app-server that cannot be proven to
// carry the dynamic tool catalog. The installed version reports its own
// version in the initialize user agent.
func requireDynamicToolSupport(result json.RawMessage) error {
	var response struct {
		UserAgent string `json:"userAgent"`
	}
	if json.Unmarshal(result, &response) != nil || response.UserAgent == "" {
		return &model.Error{Status: 503, Message: "Codex profile automation is unavailable: the app-server did not report its version, so the dynamic tool catalog cannot be verified"}
	}
	version, ok := codexVersion(response.UserAgent)
	if !ok {
		return &model.Error{Status: 503, Message: "Codex profile automation is unavailable: the app-server user agent " + response.UserAgent + " does not report a verifiable version for the dynamic tool catalog"}
	}
	if !codexAtLeast(version, "0.163.0") {
		return &model.Error{Status: 503, Message: "Codex profile automation requires Codex 0.163.0 or newer for the dynamic tool catalog; installed version is " + version}
	}
	return nil
}

func codexVersion(userAgent string) (string, bool) {
	fields := strings.Fields(userAgent)
	if len(fields) == 0 {
		return "", false
	}
	parts := strings.Split(fields[0], "/")
	if len(parts) < 2 {
		return "", false
	}
	version := parts[len(parts)-1]
	if _, ok := codexVersionParts(version); !ok {
		return "", false
	}
	return version, true
}

func codexVersionParts(version string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(version, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, part := range parts {
		if part == "" {
			return out, false
		}
		value := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				return out, false
			}
			value = value*10 + int(r-'0')
		}
		out[i] = value
	}
	return out, true
}

func codexAtLeast(version, minimum string) bool {
	have, ok := codexVersionParts(version)
	if !ok {
		return false
	}
	want, ok := codexVersionParts(minimum)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

// dynamicToolUnsupported recognizes an app-server that refused the catalog.
func dynamicToolUnsupported(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "dynamictool") || strings.Contains(text, "experimentalapi") || strings.Contains(text, "unknown field")
}

// verifyNativeToolsDisabled proves that the installed app-server turned every
// native tool surface off. A missing or still-enabled feature fails closed.
func verifyNativeToolsDisabled(config map[string]json.RawMessage) error {
	raw, ok := config["features"]
	if !ok {
		return &model.Error{Status: 503, Message: "Codex profile automation is unavailable: the installed app-server did not report its effective feature state, so native tools cannot be proven disabled"}
	}
	var features map[string]any
	if json.Unmarshal(raw, &features) != nil {
		return &model.Error{Status: 503, Message: "Codex profile automation is unavailable: the installed app-server reported an unreadable effective feature state"}
	}
	for _, name := range nativeToolDisable {
		value, ok := features[name]
		if !ok {
			return &model.Error{Status: 503, Message: "Codex profile automation is unavailable: the installed app-server does not report feature " + name + ", so native tools cannot be proven disabled"}
		}
		if enabled, _ := value.(bool); enabled {
			return &model.Error{Status: 503, Message: "Codex profile automation is unavailable: native feature " + name + " is still enabled on the installed app-server"}
		}
	}
	return nil
}

// toolCall answers one server tool request. Only the active thread and turn may
// reach the broker, and only with a tool from the registered catalog. Every
// other request is refused instead of executed.
func (r *runtime) toolCall(ctx context.Context, f frame, turn model.Turn, emit model.Emit, active string, calls *sync.WaitGroup) {
	params := toolCallParams{}
	reason := ""
	switch {
	case json.Unmarshal(f.Params, &params) != nil:
		reason = "the tool request could not be read"
	case params.ThreadID != r.session:
		reason = "the tool request belongs to another native thread"
	case active == "" || params.TurnID != active:
		reason = "the tool request belongs to a stale or unknown turn"
	case params.CallID == "":
		reason = "the tool request carries no native call identity"
	case params.Namespace != nil && *params.Namespace != "":
		reason = "the tool request names an unregistered namespace"
	case !r.catalog[params.Tool]:
		reason = "the tool request names a tool outside the broker catalog"
	case turn.Tools == nil:
		reason = "no broker executor is bound to this turn"
	}
	if reason != "" {
		r.refuseToolCall(ctx, f, reason)
		if emit != nil {
			emit("tool_denied", "Codex denied tool call: "+reason)
		}
		return
	}
	calls.Add(1)
	go func() {
		defer calls.Done()
		result, err := turn.Tools.Call(ctx, model.ToolCall{ID: params.CallID, Name: params.Tool, Arguments: toolArguments(params.Arguments)})
		send, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = r.p.Send(send, map[string]any{"id": f.ID, "result": toolCallResult(result, err)})
	}()
}

// refuseToolCall answers a tool request without running anything.
func (r *runtime) refuseToolCall(ctx context.Context, f frame, reason string) {
	_ = r.p.Send(ctx, map[string]any{"id": f.ID, "result": map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": "Meshra refused this tool call: " + reason}}, "success": false}})
}

// toolArguments normalizes the installed argument shape. The server sends the
// parsed arguments, but a JSON encoded string is accepted as well.
func toolArguments(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(`{}`)
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return json.RawMessage(`{}`)
	}
	if text, ok := value.(string); ok {
		if json.Valid([]byte(text)) {
			return json.RawMessage(text)
		}
		return raw
	}
	return raw
}

// toolCallResult maps a broker result onto the installed output shape.
func toolCallResult(result model.ToolResult, err error) map[string]any {
	if err != nil {
		return map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": "Meshra broker tool failed: " + boundedText(err.Error(), 2000)}}, "success": false}
	}
	items := []any{}
	size := 0
	for _, content := range result.Content {
		switch content.Type {
		case "text":
			text := boundedText(content.Text, 64*1024)
			size += len(text)
			items = append(items, map[string]any{"type": "inputText", "text": text})
		case "image":
			if !strings.HasPrefix(content.MimeType, "image/") {
				continue
			}
			size += len(content.Data)
			items = append(items, map[string]any{"type": "inputImage", "imageUrl": "data:" + content.MimeType + ";base64," + content.Data})
		case "audio":
			if !strings.HasPrefix(content.MimeType, "audio/") {
				continue
			}
			size += len(content.Data)
			items = append(items, map[string]any{"type": "inputAudio", "audioUrl": "data:" + content.MimeType + ";base64," + content.Data})
		}
	}
	if size > 512*1024 {
		return map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": "Meshra refused this tool result: the encoded result exceeds the transport bound"}}, "success": false}
	}
	if result.IsError {
		return map[string]any{"contentItems": items, "success": false}
	}
	return map[string]any{"contentItems": items, "success": true}
}

func boundedText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + " [truncated]"
}

// waitToolCalls waits for in-flight broker calls so a response is never lost
// when the native process is torn down.
func waitToolCalls(calls *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		calls.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// unregisteredTools lists admitted broker tools that this native session never
// registered, so the difference is reported instead of hidden.
func unregisteredTools(definitions []model.ToolDefinition, registered map[string]bool) []string {
	missing := []string{}
	for _, definition := range definitions {
		if !registered[definition.Name] {
			missing = append(missing, definition.Name)
		}
	}
	return missing
}
