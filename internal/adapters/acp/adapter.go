// Package acp implements a generic Agent Client Protocol (ACP) v1 client over
// the repository's bounded JSONL process transport.
//
// The client speaks JSON-RPC 2.0 over newline-delimited JSON frames, exactly
// like the ACP stdio transport. It never starts an unbounded subprocess, never
// grants native execution, never approves a permission request on its own and
// never exposes a legacy or administrative Meshra route to the agent.
//
// The agent reaches Meshra tools only through the admitted Meshra MCP server
// that the operator reviewed for this configuration. Provider egress is
// limited to the operator-selected origins in Config.AllowedOrigins, which the
// launcher enforces for the child process.
package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
	"meshra/internal/netpolicy"
)

const (
	// ProtocolVersion is the only ACP major version this client implements.
	// ACP v2 is a draft and is never treated as v1.
	ProtocolVersion = 1
	// MaxFrameBytes bounds one inbound or outbound JSON-RPC frame.
	MaxFrameBytes = 1 << 20

	maxPendingRequests       = 16
	maxConcurrentClientCalls = 8
	maxResultBytes           = 256 * 1024
	maxSessionIDBytes        = 128
	maxWriteBytes            = 64 * 1024
	maxApprovalWait          = 5 * time.Minute
	maxToolCallIDBytes       = 200
	clientName               = "meshra"
	clientVersion            = "1"
)

// EnvVariable is one explicitly selected environment value for the agent.
type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HTTPHeader is one header of an HTTP MCP server declaration.
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServer is one reviewed MCP server the agent may connect to. Only the
// admitted Meshra MCP server belongs here: the legacy Meshra MCP server, its
// administration routes and its collaboration tools are never exposed to an
// ACP agent.
type MCPServer struct {
	// Type is "", "http" or "sse". An empty type is the stdio transport.
	Type string `json:"type,omitempty"`
	// Name identifies the server to the agent.
	Name string `json:"name"`
	// Command is the absolute path of a stdio MCP server executable.
	Command string `json:"command,omitempty"`
	// Args is an argv array, never shell text.
	Args []string `json:"args,omitempty"`
	// Env holds explicitly selected environment values for a stdio server.
	Env []EnvVariable `json:"env,omitempty"`
	// URL is the endpoint of an HTTP or SSE MCP server.
	URL string `json:"url,omitempty"`
	// Headers are the headers sent to an HTTP or SSE MCP server.
	Headers []HTTPHeader `json:"headers,omitempty"`
}

// Config is the reviewed ACP agent configuration. The relay stores it as JSON
// and resolves it for the binding that owns the turn.
type Config struct {
	// ID is the stored configuration identifier.
	ID string `json:"id,omitempty"`
	// Revision is the stored configuration revision.
	Revision int `json:"revision,omitempty"`
	// Name is the operator-visible configuration name.
	Name string `json:"name,omitempty"`
	// Preset names the installed CLI preset, such as gemini, cursor or cline.
	Preset string `json:"preset,omitempty"`
	// Executable is the absolute path of the ACP CLI.
	Executable string `json:"executable"`
	// Args is an argv array, never shell text.
	Args []string `json:"args,omitempty"`
	// Env holds explicitly selected environment values for the agent.
	Env map[string]string `json:"env,omitempty"`
	// CWD is the absolute working directory of the ACP session.
	CWD string `json:"cwd,omitempty"`
	// AuthMethodID selects one advertised authenticate method.
	AuthMethodID string `json:"authMethodId,omitempty"`
	// AllowedOrigins are the exact provider origins the agent may reach.
	AllowedOrigins []string `json:"allowedOrigins,omitempty"`
	// MCPServers are the reviewed MCP servers the agent may connect to.
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
	// ReadOnlyAuthFiles are the native authentication files the launcher may
	// mount read-only inside the confined agent.
	ReadOnlyAuthFiles []string `json:"readOnlyAuthFiles,omitempty"`
	// RuntimePaths are the reviewed runtime search paths of the agent.
	RuntimePaths []string `json:"runtimePaths,omitempty"`
	// SecretEnvRefs names the operator-reviewed secrets the launcher resolves
	// into the agent environment. Values are never carried in this struct.
	SecretEnvRefs []string `json:"secretEnvRefs,omitempty"`
	// Enabled reports whether the operator enabled this configuration.
	Enabled bool `json:"enabled"`
}

// PermissionOption is one option the agent offers for a permission request.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// PermissionRequest describes one exact agent permission request.
type PermissionRequest struct {
	SessionID  string
	ToolCallID string
	Title      string
	Kind       string
	Name       string
	RawInput   json.RawMessage
	Options    []PermissionOption
}

// PermissionDecision is the operator's answer to one permission request.
type PermissionDecision struct {
	// Approved reports that the operator approved this single call.
	Approved bool
	// OptionID is the exact option the operator selected. An empty value
	// selects the only offered one-call allow option.
	OptionID string
}

// Approver asks the local operator to approve exactly one client-owned
// operation. A nil approver denies every permission request.
type Approver interface {
	ApprovePermission(ctx context.Context, request PermissionRequest) (PermissionDecision, error)
}

// Launcher starts the ACP agent and returns its bounded JSONL transport. The
// launcher owns process confinement, the reviewed environment, the private
// state directory and the provider egress restriction: it resolves
// Config.SecretEnvRefs into the agent environment at launch time, mounts only
// the reviewed Config.ReadOnlyAuthFiles and gives the agent no route except
// the operator-selected origins in Config.AllowedOrigins.
type Launcher interface {
	Launch(ctx context.Context, config Config) (Transport, error)
}

// Options wires the adapter to the operator-controlled boundaries.
type Options struct {
	// Resolve returns the reviewed configuration for a configuration ID.
	Resolve func(ctx context.Context, id string) (Config, error)
	// Approve asks the operator to approve exactly one client-owned operation.
	Approve Approver
	// Launcher starts the agent process. A nil launcher refuses ACP
	// execution: an unconfined agent process is never started implicitly.
	Launcher Launcher
}

// DecodeStored decodes a stored ACP configuration document into a Config.
// The JSON field names match the stored configuration, so a stored document
// decodes without a field-by-field mapping. Unknown stored members are
// ignored.
func DecodeStored(body []byte) (Config, error) {
	var config Config
	if err := json.Unmarshal(body, &config); err != nil {
		return Config{}, err
	}
	return config, nil
}

// New registers the "acp" harness. The relay resolves the reviewed ACP
// configuration by the agent provider field and supplies the launcher that
// confines the agent process.
func New(options Options) model.Adapter {
	return model.Adapter{
		Factory: func(ctx context.Context, agent model.Agent, emit model.Emit) (model.Runtime, error) {
			return newRuntime(ctx, options, agent, emit)
		},
		Capabilities: map[string]bool{
			"prompt": true, "stream": true, "cancel": true, "tools": true, "mcp": true,
			"profileAutomation": true, "resume": false, "durableHistory": false, "toolFreeDraft": false,
		},
	}
}

type runtime struct {
	conn      *connection
	cfg       Config
	agent     model.Agent
	approver  Approver
	emit      model.Emit
	session   string
	caps      agentCapabilities
	mu        sync.Mutex
	turnCtx   context.Context
	turnEmit  model.Emit
	executor  model.ToolExecutor
	terminals map[string]*clientTerminal
	assistant bytes.Buffer
	overflow  bool
	calls     int64
	closed    bool
	once      sync.Once
}

func newRuntime(ctx context.Context, options Options, agent model.Agent, emit model.Emit) (model.Runtime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Launcher == nil {
		return nil, errors.New("ACP execution requires a configured launcher; an unconfined agent process is never started")
	}
	if options.Resolve == nil {
		return nil, errors.New("ACP configuration resolution is unavailable")
	}
	if agent.Config.Provider == "" {
		return nil, errors.New("the ACP harness requires an ACP configuration identifier in the provider field")
	}
	config, err := options.Resolve(ctx, agent.Config.Provider)
	if err != nil {
		return nil, fmt.Errorf("resolve the ACP configuration: %w", err)
	}
	if err = validateConfig(config); err != nil {
		return nil, err
	}
	if agent.ProfileManaged && len(config.AllowedOrigins) == 0 {
		// A profile-managed agent may only reach an operator-selected
		// provider origin, so the reviewed origin list may not be empty.
		return nil, errors.New("a profile-managed ACP agent requires at least one operator-selected provider origin")
	}
	transport, err := options.Launcher.Launch(ctx, config)
	if err != nil {
		return nil, err
	}
	r := &runtime{cfg: config, agent: agent, approver: options.Approve, emit: emit}
	r.conn = newConnection(transport, r)
	if err = r.start(ctx); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func (r *runtime) SessionID() string { r.mu.Lock(); defer r.mu.Unlock(); return r.session }

func (r *runtime) Close() error {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.conn.shutdown()
		r.closeTerminals()
	})
	return nil
}

func (r *runtime) start(ctx context.Context) error {
	setupCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	result, err := r.initialize(setupCtx)
	if err != nil {
		return err
	}
	if err = r.authenticate(setupCtx, result.AuthMethods); err != nil {
		return err
	}
	return r.openSession(setupCtx)
}

func (r *runtime) initialize(ctx context.Context) (initializeResult, error) {
	var result initializeResult
	params := initializeParams{
		ProtocolVersion:    ProtocolVersion,
		ClientCapabilities: clientCapabilities{FS: &fsCapabilities{ReadTextFile: true, WriteTextFile: true}, Terminal: hasTool(r.agent.ToolDefinitions, "meshra_workspace_command")},
		ClientInfo:         clientInfo{Name: clientName, Version: clientVersion},
	}
	raw, err := r.conn.call(ctx, "initialize", params)
	if err != nil {
		return result, fmt.Errorf("ACP initialize failed: %w", err)
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return result, errors.New("ACP initialize returned an unreadable result")
	}
	if result.ProtocolVersion != ProtocolVersion {
		return result, fmt.Errorf("unsupported ACP protocol version %d; this client implements ACP v1 only", result.ProtocolVersion)
	}
	r.caps = result.AgentCapabilities
	return result, nil
}

// authenticate performs the advertised authentication method only. Passwords
// and other secrets are never requested through this path.
func (r *runtime) authenticate(ctx context.Context, methods []authMethod) error {
	if len(methods) == 0 {
		if r.cfg.AuthMethodID != "" {
			return errors.New("the configuration names an authentication method that the agent does not advertise")
		}
		return nil
	}
	if r.cfg.AuthMethodID == "" {
		// Advertising a method does not mean an existing native sign-in is
		// absent. session/new is authoritative; authentication is user initiated.
		return nil
	}
	advertised := false
	for _, method := range methods {
		if method.ID == r.cfg.AuthMethodID {
			advertised = true
			break
		}
	}
	if !advertised {
		return fmt.Errorf("the ACP agent does not advertise the authentication method %q", r.cfg.AuthMethodID)
	}
	if _, err := r.conn.call(ctx, "authenticate", authenticateParams{MethodID: r.cfg.AuthMethodID}); err != nil {
		return fmt.Errorf("ACP authentication failed: %w", err)
	}
	return nil
}

func (r *runtime) openSession(ctx context.Context) error {
	cwd := r.cfg.CWD
	if cwd == "" {
		cwd = r.agent.Cwd
	}
	if !filepath.IsAbs(cwd) {
		return errors.New("the ACP session working directory must be an absolute path")
	}
	servers, err := r.mcpServers()
	if err != nil {
		return err
	}
	params := sessionParams{CWD: cwd, MCPServers: servers}
	var raw json.RawMessage
	if r.agent.SessionID != "" {
		if !r.caps.LoadSession {
			return errors.New("the ACP agent does not advertise session loading; a fresh ACP binding is required")
		}
		params.SessionID = r.agent.SessionID
		raw, err = r.conn.call(ctx, "session/load", params)
	} else {
		raw, err = r.conn.call(ctx, "session/new", params)
	}
	if err != nil {
		return fmt.Errorf("ACP session setup failed: %w", err)
	}
	var result sessionResult
	if err = json.Unmarshal(raw, &result); err != nil {
		return errors.New("ACP session setup returned an unreadable result")
	}
	if !validSessionID(result.SessionID) {
		return errors.New("the ACP agent returned an unusable session identifier")
	}
	if r.agent.SessionID != "" && result.SessionID != r.agent.SessionID {
		return errors.New("the ACP agent resumed a different session")
	}
	r.mu.Lock()
	r.session = result.SessionID
	r.mu.Unlock()
	if r.emit != nil {
		r.emit("session", result.SessionID)
	}
	return nil
}

// mcpServers returns the reviewed MCP declarations for the agent. The legacy
// Meshra MCP server and every administrative or collaboration route are never
// part of this list.
func (r *runtime) mcpServers() ([]mcpServer, error) {
	declared := make([]mcpServer, 0, len(r.cfg.MCPServers))
	for _, server := range r.cfg.MCPServers {
		switch server.Type {
		case "", "stdio":
		case "http":
			if !r.caps.MCPCapabilities.HTTP {
				return nil, fmt.Errorf("the ACP agent does not advertise the HTTP MCP transport required by %q", server.Name)
			}
		case "sse":
			if !r.caps.MCPCapabilities.SSE {
				return nil, fmt.Errorf("the ACP agent does not advertise the SSE MCP transport required by %q", server.Name)
			}
		default:
			return nil, fmt.Errorf("the MCP server %q uses an unsupported transport", server.Name)
		}
		transport := server.Type
		if transport == "stdio" {
			// The stdio transport is the default and carries no type field.
			transport = ""
		}
		declared = append(declared, mcpServer{Type: transport, Name: server.Name, Command: server.Command, Args: server.Args, Env: envList(server.Env), URL: server.URL, Headers: headerList(server.Headers)})
	}
	return declared, nil
}

func (r *runtime) Run(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", errors.New("the ACP runtime is closed")
	}
	r.turnCtx = turnCtx
	r.turnEmit = emit
	r.executor = turn.Tools
	r.assistant.Reset()
	r.overflow = false
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.turnCtx = nil
		r.turnEmit = nil
		r.executor = nil
		r.mu.Unlock()
		r.closeTerminals()
	}()
	params := promptParams{SessionID: r.SessionID(), Prompt: []contentBlock{{Type: "text", Text: turn.Task.Text}}}
	raw, err := r.conn.call(turnCtx, "session/prompt", params)
	if err != nil {
		if turnCtx.Err() != nil {
			r.cancelSession()
			return "", turnCtx.Err()
		}
		return "", fmt.Errorf("ACP prompt failed: %w", err)
	}
	var result promptResult
	if err = json.Unmarshal(raw, &result); err != nil {
		return "", errors.New("ACP prompt returned an unreadable result")
	}
	if result.StopReason == "cancelled" {
		return "", errors.New("the ACP agent cancelled the prompt turn")
	}
	r.mu.Lock()
	text := r.assistant.String()
	overflow := r.overflow
	r.mu.Unlock()
	if overflow {
		return "", fmt.Errorf("the ACP assistant response exceeds %d bytes", maxResultBytes)
	}
	return text, nil
}

// cancelSession asks the agent to stop the running turn. Pending permission
// requests are answered with the cancelled outcome by their own handlers.
func (r *runtime) cancelSession() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.conn.notify(ctx, "session/cancel", map[string]any{"sessionId": r.SessionID()})
}

// notification consumes agent notifications. Only bounded assistant text is
// retained; tool arguments and raw payloads are never persisted.
func (r *runtime) notification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var update updateParams
	if err := json.Unmarshal(params, &update); err != nil {
		return
	}
	if update.SessionID != "" && update.SessionID != r.SessionID() {
		return
	}
	switch update.Update.SessionUpdate {
	case "agent_message_chunk":
		if update.Update.Content == nil || update.Update.Content.Type != "text" || update.Update.Content.Text == "" {
			return
		}
		text := update.Update.Content.Text
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		if r.assistant.Len()+len(text) > maxResultBytes {
			r.overflow = true
			r.mu.Unlock()
			return
		}
		r.assistant.WriteString(text)
		emit := r.turnEmit
		r.mu.Unlock()
		if emit != nil {
			emit("delta", text)
		}
	case "tool_call":
		r.mu.Lock()
		emit := r.turnEmit
		r.mu.Unlock()
		if emit != nil && update.Update.Title != "" {
			emit("progress", "ACP tool call: "+truncate(update.Update.Title, 200))
		}
	}
}

// request answers one agent request. Every request receives an explicit
// result or an explicit protocol error; the client never leaves a request
// unanswered and never waits on a method it does not implement.
func (r *runtime) request(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	turnCtx, active := r.turnContext()
	if !active {
		// A request outside a running turn is never executed and never
		// reaches the operator approval path.
		if method == "session/request_permission" {
			return cancelledOutcome(), nil
		}
		return nil, &rpcError{Code: -32603, Message: "no active turn"}
	}
	requestID := ctx.Value(clientRequestIDKey{})
	ctx = turnCtx
	if requestID != nil {
		ctx = context.WithValue(ctx, clientRequestIDKey{}, requestID)
	}
	switch method {
	case "session/request_permission":
		return r.requestPermission(ctx, params)
	case "fs/read_text_file":
		return r.readTextFile(ctx, params)
	case "fs/write_text_file":
		return r.writeTextFile(ctx, params)
	case "terminal/create":
		return r.terminalCreate(ctx, params)
	case "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		return r.terminalRequest(ctx, method, params)
	default:
		return nil, &rpcError{Code: -32601, Message: "the client method " + method + " is not supported"}
	}
}

// requestPermission applies the permission policy. Agent-owned operations are
// never granted, a persistent allow option is never selected, and a
// client-owned operation requires one explicit operator approval for that
// single call.
func (r *runtime) requestPermission(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var request permissionParams
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid permission request"}
	}
	if request.SessionID != "" && request.SessionID != r.SessionID() {
		return nil, &rpcError{Code: -32602, Message: "the permission request names an unknown session"}
	}
	if ctx.Err() != nil {
		return cancelledOutcome(), nil
	}
	if !clientOwnedOperation(request.ToolCall.Kind) {
		// Agent-owned edits and executions are never granted by this client.
		return deniedOutcome(request.Options), nil
	}
	option, ok := allowOnceOption(request.Options)
	if !ok {
		// A persistent or unknown option never creates a standing grant.
		return deniedOutcome(request.Options), nil
	}
	decision, err := r.approve(ctx, request)
	if err != nil || !decision.Approved {
		if ctx.Err() != nil {
			// A cancelled turn answers every pending permission request with
			// the cancelled outcome.
			return cancelledOutcome(), nil
		}
		return deniedOutcome(request.Options), nil
	}
	if decision.OptionID != "" && decision.OptionID != option.OptionID {
		return deniedOutcome(request.Options), nil
	}
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option.OptionID}}, nil
}

// approve asks the operator for one decision about one exact operation.
func (r *runtime) approve(ctx context.Context, request permissionParams) (PermissionDecision, error) {
	if r.approver == nil {
		return PermissionDecision{}, errors.New("no operator approval path is configured")
	}
	options := make([]PermissionOption, 0, len(request.Options))
	for _, option := range request.Options {
		options = append(options, PermissionOption{OptionID: option.OptionID, Name: option.Name, Kind: option.Kind})
	}
	waitCtx, cancel := context.WithTimeout(ctx, maxApprovalWait)
	defer cancel()
	return r.approver.ApprovePermission(waitCtx, PermissionRequest{
		SessionID: request.SessionID, ToolCallID: request.ToolCall.ToolCallID, Title: request.ToolCall.Title,
		Kind: request.ToolCall.Kind, Name: request.ToolCall.Name, RawInput: request.ToolCall.RawInput, Options: options,
	})
}

// clientOwnedOperation reports whether Meshra implements the requested
// operation itself. Only file reads and exact file replacements are
// client-owned; every other agent-owned operation stays denied.
func clientOwnedOperation(kind string) bool {
	switch kind {
	case "read", "edit":
		return true
	default:
		return false
	}
}

// allowOnceOption returns the single offered one-call allow option. A
// persistent allow option is never selected: no approval answer creates a
// permanent grant.
func allowOnceOption(options []permissionOption) (permissionOption, bool) {
	for _, option := range options {
		if option.Kind == "allow_once" && option.OptionID != "" {
			return option, true
		}
	}
	return permissionOption{}, false
}

func deniedOutcome(options []permissionOption) any {
	for _, option := range options {
		if option.Kind == "reject_once" && option.OptionID != "" {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option.OptionID}}
		}
	}
	for _, option := range options {
		if option.Kind == "reject_always" && option.OptionID != "" {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option.OptionID}}
		}
	}
	return cancelledOutcome()
}

func cancelledOutcome() any {
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
}

// readTextFile serves a client-owned read through the admitted broker tool.
// The agent never reaches a host path directly.
func (r *runtime) readTextFile(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var request readTextFileParams
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid file read request"}
	}
	if request.SessionID != "" && request.SessionID != r.SessionID() {
		return nil, &rpcError{Code: -32602, Message: "the file read names an unknown session"}
	}
	path, ok := clientWorkspacePath(request.Path)
	if !ok {
		return nil, &rpcError{Code: -32602, Message: "a canonical path beneath /workspace is required"}
	}
	executor := r.currentExecutor()
	if executor == nil {
		return nil, &rpcError{Code: -32603, Message: "client file access is unavailable for this turn"}
	}
	callID, failure := requestCallID(ctx)
	if failure != nil {
		return nil, failure
	}
	result, err := executor.Call(ctx, model.ToolCall{ID: callID, Name: "meshra_workspace_read", Arguments: json.RawMessage(`{"path":` + quoteJSON(path) + `}`)})
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: "the workspace read was denied: " + err.Error()}
	}
	text, ok := brokerText(result)
	if !ok {
		return nil, &rpcError{Code: -32000, Message: "the workspace read did not return readable text"}
	}
	return map[string]any{"content": sliceLines(text, request.Line, request.Limit)}, nil
}

// writeTextFile stages an exact replacement through the admitted broker tool.
// The broker applies its own one-call approval and the host change stays a
// separate human decision.
func (r *runtime) writeTextFile(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var request writeTextFileParams
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid file write request"}
	}
	if request.SessionID != "" && request.SessionID != r.SessionID() {
		return nil, &rpcError{Code: -32602, Message: "the file write names an unknown session"}
	}
	path, ok := clientWorkspacePath(request.Path)
	if !ok {
		return nil, &rpcError{Code: -32602, Message: "a canonical path beneath /workspace is required"}
	}
	if len(request.Content) > maxWriteBytes {
		return nil, &rpcError{Code: -32602, Message: "the file content exceeds the bounded write size"}
	}
	executor := r.currentExecutor()
	if executor == nil {
		return nil, &rpcError{Code: -32603, Message: "client file access is unavailable for this turn"}
	}
	arguments := `{"path":` + quoteJSON(path) + `,"content":` + quoteJSON(request.Content) + `}`
	callID, failure := requestCallID(ctx)
	if failure != nil {
		return nil, failure
	}
	result, err := executor.Call(ctx, model.ToolCall{ID: callID, Name: "meshra_workspace_write", Arguments: json.RawMessage(arguments)})
	if err != nil || result.IsError {
		return nil, &rpcError{Code: -32000, Message: "the workspace write was denied or failed"}
	}
	return map[string]any{}, nil
}

func (r *runtime) currentExecutor() model.ToolExecutor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.executor
}

// turnContext returns the context of the running turn. Agent requests are
// answered inside the turn, so a cancelled or finished turn stops them.
func (r *runtime) turnContext() (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turnCtx != nil {
		return r.turnCtx, true
	}
	return nil, false
}

func (r *runtime) callID(kind string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return truncate("acp:"+kind+":"+r.session+":"+fmt.Sprintf("%d", r.calls), maxToolCallIDBytes)
}

func clientWorkspacePath(path string) (string, bool) {
	const prefix = "/workspace/"
	if path == "" || len(path) > 4096 || !strings.HasPrefix(path, prefix) || filepath.Clean(path) != path {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}

// brokerText extracts the text of a rooted read from the broker result.
func brokerText(result model.ToolResult) (string, bool) {
	if result.IsError {
		return "", false
	}
	for _, content := range result.Content {
		if content.Type != "text" || content.Text == "" {
			continue
		}
		var payload struct {
			Text     *string `json:"text"`
			Encoding string  `json:"encoding"`
		}
		if err := json.Unmarshal([]byte(content.Text), &payload); err != nil {
			return "", false
		}
		if payload.Encoding != "" || payload.Text == nil {
			return "", false
		}
		return *payload.Text, true
	}
	return "", false
}

// sliceLines applies the ACP line window to the returned text.
func sliceLines(text string, line, limit *int) string {
	if line == nil && limit == nil {
		return truncate(text, maxResultBytes)
	}
	lines := strings.Split(text, "\n")
	start := 0
	if line != nil {
		start = *line - 1
	}
	if start < 0 {
		start = 0
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if limit != nil && *limit >= 0 && start+*limit < end {
		end = start + *limit
	}
	return truncate(strings.Join(lines[start:end], "\n"), maxResultBytes)
}

// validateConfig rejects an unusable or unreviewed configuration before any
// process is started.
func validateConfig(config Config) error {
	if !config.Enabled {
		return errors.New("the ACP configuration is disabled")
	}
	if config.Executable == "" || !filepath.IsAbs(config.Executable) {
		return errors.New("the ACP executable must be an absolute path")
	}
	info, err := os.Stat(config.Executable)
	if err != nil {
		return fmt.Errorf("the ACP executable is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("the ACP executable must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return errors.New("the ACP executable is not executable")
	}
	if len(config.Args) > 128 {
		return errors.New("the ACP configuration declares too many arguments")
	}
	for _, arg := range config.Args {
		if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
			return errors.New("the ACP configuration declares an invalid argument")
		}
	}
	for name, value := range config.Env {
		if err = validateEnvName(name); err != nil {
			return err
		}
		if len(value) > 8192 || strings.ContainsRune(value, 0) {
			return fmt.Errorf("the environment value %q is invalid", name)
		}
	}
	origins, err := allowedOrigins(config.AllowedOrigins)
	if err != nil {
		return err
	}
	for _, server := range config.MCPServers {
		if err = validateMCPServer(server, origins); err != nil {
			return err
		}
	}
	return nil
}

func validateEnvName(name string) error {
	if name == "" || len(name) > 128 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) || strings.ContainsRune(name, '=') {
		return fmt.Errorf("the environment variable %q is invalid", name)
	}
	if strings.HasPrefix(name, "MESHRA_") {
		return fmt.Errorf("the environment variable %q would expose a Meshra credential to the agent", name)
	}
	return nil
}

// allowedOrigins parses the operator-selected provider origins.
func allowedOrigins(raw []string) (map[string]bool, error) {
	origins := map[string]bool{}
	for _, entry := range raw {
		if len(entry) > 2048 {
			return nil, errors.New("an allowed origin is too long")
		}
		origin, err := netpolicy.ParseOrigin(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed origin %q: %w", entry, err)
		}
		origins[origin.String()] = true
	}
	return origins, nil
}

// validateMCPServer rejects an MCP declaration that could reach an
// unreviewed destination or hide a shell command.
func validateMCPServer(server MCPServer, origins map[string]bool) error {
	if server.Name == "" || len(server.Name) > 64 || !utf8.ValidString(server.Name) || strings.ContainsAny(server.Name, "\x00\r\n\t") {
		return errors.New("an MCP server declaration requires a bounded name")
	}
	switch server.Type {
	case "", "stdio":
		if server.Command == "" || !filepath.IsAbs(server.Command) {
			return fmt.Errorf("the MCP server %q requires an absolute command path", server.Name)
		}
		info, err := os.Stat(server.Command)
		if err != nil {
			return fmt.Errorf("the MCP server %q command is unavailable: %w", server.Name, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("the MCP server %q command must be an executable regular file", server.Name)
		}
		for _, arg := range server.Args {
			if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("the MCP server %q declares an invalid argument", server.Name)
			}
		}
		for _, variable := range server.Env {
			if err = validateEnvName(variable.Name); err != nil {
				return err
			}
			if len(variable.Value) > 8192 || strings.ContainsRune(variable.Value, 0) {
				return fmt.Errorf("the MCP server %q declares an invalid environment value", server.Name)
			}
		}
	case "http", "sse":
		origin, err := mcpOrigin(server.URL)
		if err != nil {
			return fmt.Errorf("the MCP server %q is invalid: %w", server.Name, err)
		}
		if !loopbackOrigin(origin) && !origins[origin] {
			return fmt.Errorf("the MCP server %q origin %q is not an operator-selected origin", server.Name, origin)
		}
		for _, header := range server.Headers {
			if header.Name == "" || len(header.Name) > 128 || len(header.Value) > 8192 || strings.ContainsAny(header.Name, "\x00\r\n") || strings.ContainsRune(header.Value, 0) {
				return fmt.Errorf("the MCP server %q declares an invalid header", server.Name)
			}
		}
	default:
		return fmt.Errorf("the MCP server %q uses an unsupported transport", server.Name)
	}
	return nil
}

// mcpOrigin returns the canonical origin of an MCP endpoint. The endpoint
// path is allowed; credentials, a query and a fragment are not.
func mcpOrigin(raw string) (string, error) {
	if raw == "" || len(raw) > 2048 {
		return "", errors.New("an MCP endpoint URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("the MCP endpoint URL is invalid")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("only http and https MCP endpoints are supported")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return "", errors.New("the MCP endpoint URL must not carry credentials, a query or a fragment")
	}
	if parsed.Host == "" {
		return "", errors.New("the MCP endpoint URL requires a host")
	}
	origin, err := netpolicy.ParseOrigin(parsed.Scheme + "://" + parsed.Host)
	if err != nil {
		return "", errors.New("the MCP endpoint origin is invalid")
	}
	return origin.String(), nil
}

func loopbackOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// validSessionID keeps an untrusted agent identifier out of persisted state.
func validSessionID(session string) bool {
	if session == "" || len(session) > maxSessionIDBytes {
		return false
	}
	for _, r := range session {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

func envList(values []EnvVariable) []envVariable {
	out := make([]envVariable, 0, len(values))
	for _, value := range values {
		out = append(out, envVariable{Name: value.Name, Value: value.Value})
	}
	return out
}

func headerList(values []HTTPHeader) []httpHeader {
	out := make([]httpHeader, 0, len(values))
	for _, value := range values {
		out = append(out, httpHeader{Name: value.Name, Value: value.Value})
	}
	return out
}

func quoteJSON(value string) string {
	body, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(body)
}

func truncate(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cut := value[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
