package relay

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
	"meshra/internal/sandbox"
)

const executeCodeLimit = 60 << 10

type toolExecuteCodeArgs struct {
	Code           string `json:"code"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

type preparedExecuteCode struct {
	input       toolExecuteCodeArgs
	config      sandbox.Config
	interpreter string
	workspace   *sandbox.Snapshot
	baseline    string
}

func validateExecuteCode(q *toolExecuteCodeArgs) error {
	if q.Code == "" || len(q.Code) > executeCodeLimit || !utf8.ValidString(q.Code) || strings.ContainsRune(q.Code, 0) {
		return model.Err(400, "code must be valid UTF-8, nonempty, and at most 60 KiB without NUL")
	}
	if q.TimeoutSeconds == 0 {
		q.TimeoutSeconds = 60
	}
	if q.TimeoutSeconds < 1 || q.TimeoutSeconds > 300 {
		return model.Err(400, "code timeout must be 1..300 seconds")
	}
	return nil
}

func (b *toolBroker) prepareExecuteCode(ctx context.Context, state *brokerCall, q *toolExecuteCodeArgs) error {
	if b.snapshot.Binding.Harness != "openai" || !b.snapshot.Profile.Capabilities.WorkspaceRead || !b.snapshot.Profile.Capabilities.Command {
		return model.Err(403, "code execution requires Direct API workspace read and command capabilities")
	}
	config, revision, err := requireRuntime(ctx, b.relay)
	if err != nil {
		return err
	}
	interpreter := config.Executables["python3"]
	if interpreter == "" {
		return model.Err(503, "configure a reviewed python3 executable in the runtime manifest")
	}
	workspace, err := b.ensureWorkspace(ctx)
	if err != nil {
		return err
	}
	baseline, err := workspace.Changes(ctx)
	if err != nil {
		return sandboxToolError(err)
	}
	state.action.preview.ManifestHash = baseline.Hash
	baseline.Close()
	state.action.runtimeRevision = revision
	state.action.preview.Label = "Execute exact Python script in a read-only confined snapshot; nested tools require their own approvals"
	body, _ := json.Marshal(map[string]any{"code": q.Code, "interpreter": interpreter, "timeoutSeconds": q.TimeoutSeconds, "runtimeRevision": revision})
	state.action.preview.Arguments = json.RawMessage(redact(string(body)))
	if !json.Valid(state.action.preview.Arguments) {
		state.action.preview.Arguments, _ = json.Marshal(map[string]string{"redacted": redact(string(body))})
	}
	state.action.args = &preparedExecuteCode{input: *q, config: config, interpreter: interpreter, workspace: workspace, baseline: state.action.preview.ManifestHash}
	return nil
}

// A code action reserves the broker mutation slot until the worker starts.
// This makes the hash check and startup atomic against broker workspace writers.
func (b *toolBroker) claimExecuteCode(ctx context.Context, state *brokerCall, q *preparedExecuteCode) error {
	b.relay.mu.Lock()
	valid := b.workspace == q.workspace && b.mutation == state.invocation.ID
	b.relay.mu.Unlock()
	if !valid {
		return model.Err(409, "approved code workspace reservation changed")
	}
	baseline, err := q.workspace.Changes(ctx)
	if err != nil {
		return sandboxToolError(err)
	}
	matches := baseline.Hash == q.baseline
	baseline.Close()
	if !matches {
		return model.Err(409, "private workspace changed after code approval")
	}
	return b.claim(state)
}

// Bound even incomplete HTTP connections before the server allocates a goroutine.
type codeListener struct {
	net.Listener
	slots chan struct{}
}

type codeConn struct {
	net.Conn
	slots chan struct{}
	once  sync.Once
}

func (c *codeConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.slots })
	return err
}

func (l *codeListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &codeConn{Conn: conn, slots: l.slots}, nil
		default:
			_ = conn.Close()
		}
	}
}

type codeBridge struct {
	broker   *toolBroker
	parent   *brokerCall
	ctx      context.Context
	ready    <-chan struct{}
	cancel   context.CancelFunc
	server   *http.Server
	listener net.Listener
	dir      string
	mu       sync.Mutex
	closed   bool
	requests int
	limit    int
	busy     bool
	inflight sync.WaitGroup
	done     chan struct{}
}

func newCodeBridge(ctx context.Context, b *toolBroker, parent *brokerCall, ready <-chan struct{}) (*codeBridge, error) {
	dir, err := os.MkdirTemp("", "meshra-code-")
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "broker.sock"))
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err = os.Chmod(listener.Addr().String(), 0600); err != nil {
		listener.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	limit := 40
	if b.snapshot.Profile.MaxToolCalls > 0 {
		limit = min(b.snapshot.Profile.MaxToolCalls, 1000)
	}
	bridge := &codeBridge{broker: b, parent: parent, ctx: child, ready: ready, cancel: cancel, listener: listener, dir: dir, limit: limit, done: make(chan struct{})}
	bridge.server = &http.Server{Handler: bridge, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return child }}
	go func() {
		defer close(bridge.done)
		_ = bridge.server.Serve(&codeListener{Listener: listener, slots: make(chan struct{}, 8)})
	}()
	return bridge, nil
}

func (s *codeBridge) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	_ = s.server.Close()
	<-s.done
	s.inflight.Wait()
	_ = os.RemoveAll(s.dir)
}

func (s *codeBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed || s.busy || s.requests >= s.limit {
		s.mu.Unlock()
		http.Error(w, "code bridge unavailable or request limit reached", http.StatusTooManyRequests)
		return
	}
	s.requests++
	s.busy = true
	s.inflight.Add(1)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.busy = false; s.mu.Unlock(); s.inflight.Done() }()
	if r.Method != http.MethodPost || r.URL.Path != "/call" || r.URL.RawQuery != "" {
		http.Error(w, "only POST /call is available", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxToolArguments+4097))
	if err != nil || len(body) > maxToolArguments+4096 {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var request struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err = decodeAutomation(body, &request); err != nil || request.Name == "" || len(request.Name) > 200 {
		http.Error(w, "invalid tool request", http.StatusBadRequest)
		return
	}
	if request.Name == "meshra_execute_code" {
		http.Error(w, "recursive code execution is not permitted", http.StatusForbidden)
		return
	}
	if s.ready != nil {
		select {
		case <-s.ready:
		case <-r.Context().Done():
			return
		}
	}
	if err = s.broker.claim(s.parent); err != nil {
		s.respond(w, nil, err)
		return
	}
	result, err := s.broker.Call(r.Context(), model.ToolCall{ID: "code-" + randomID(), Name: request.Name, Arguments: request.Arguments})
	s.respond(w, &result, err)
}

func (s *codeBridge) respond(w http.ResponseWriter, result *model.ToolResult, err error) {
	var value any
	if err != nil {
		value = map[string]any{"error": redact(err.Error())}
	} else {
		value = map[string]any{"result": result}
	}
	body, marshalErr := json.Marshal(value)
	if marshalErr != nil || len(body) > 2<<20 {
		http.Error(w, "tool response exceeds bridge limit", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (b *toolBroker) executeCode(ctx context.Context, state *brokerCall, q *preparedExecuteCode) (model.ToolResult, error) {
	var zero model.ToolResult
	ready := make(chan struct{})
	bridge, err := newCodeBridge(ctx, b, state, ready)
	if err != nil {
		return zero, model.Err(503, "unable to create private code broker socket")
	}
	defer bridge.Close()
	result, err := sandbox.Run(ctx, q.config, sandbox.Request{
		Workspace: q.workspace, Argv: []string{q.interpreter, "-I", "-c", q.input.Code}, Kind: sandbox.Code,
		Timeout: time.Duration(q.input.TimeoutSeconds) * time.Second, ReadOnly: true,
		SocketMounts: []sandbox.SocketMount{{Source: bridge.listener.Addr().String(), Target: "/run/meshra/broker.sock", Purpose: "broker"}},
		BeforeStart:  func() error { return b.claimExecuteCode(ctx, state, q) },
		AfterStart: func() {
			b.relay.mu.Lock()
			if b.mutation == state.invocation.ID {
				b.mutation = ""
			}
			b.relay.mu.Unlock()
			close(ready)
		},
	})
	if err != nil {
		return zero, sandboxToolError(err)
	}
	value, err := toolJSON(map[string]any{"exitCode": result.ExitCode, "stdout": redact(result.Stdout), "stderr": redact(result.Stderr), "truncated": result.Truncated})
	value.IsError = result.ExitCode != 0
	return value, err
}
