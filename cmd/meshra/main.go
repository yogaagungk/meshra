package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"meshra/internal/adapters/claude"
	"meshra/internal/adapters/codex"
	"meshra/internal/adapters/omp"
	"meshra/internal/adapters/opencode"
	"meshra/internal/api"
	"meshra/internal/model"
	"meshra/internal/relay"
)

const helpText = `Meshra — local named-agent relay

Usage: meshra [global flags] <command> [flags]

Commands:
  serve                     Run the authenticated localhost daemon
  agent create --harness H --name N --cwd DIR
       [--model M] [--provider P] [--display-name N]
       [--allow-to ID,...] [--allow-operation OP,...] [--delegate]
  agent list
  agent status|start|stop|restart|remove|token ID
  send|request --from ID --to ID --message TEXT
       [--idempotency-key KEY] [--timeout SECONDS|DURATION] [--parent-task ID]
  task status|cancel|retry ID
  task events ID [--after N] [--follow]
  mcp stdio                 Agent-only MCP proxy; requires MESHRA_TOKEN

Global flags (accepted anywhere):
  --data-dir DIR            Default: UserConfigDir()/meshra
  --addr HOST:PORT          Default: 127.0.0.1:7331; loopback only
  --config FILE            JSON object with addr and/or dataDir
  --json                   Complete machine-readable JSON (events: JSONL)
  --help, -h               Show this help

Human commands use MESHRA_TOKEN, otherwise DATA_DIR/admin.token.
Tokens are never included in process arguments. Token output is sensitive.
Requests return persistent tasks immediately; use task events --follow for replies.
Exit codes: 0 success, 1 operation failed, 2 invalid command or configuration.
`

type options struct {
	DataDir string `json:"dataDir"`
	Addr    string `json:"addr"`
	JSON    bool   `json:"-"`
}
type usageError struct{ error }

func usage(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "meshra:", err)
		var u usageError
		if errors.As(err, &u) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func globals(args []string) (options, []string, bool, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return options{}, nil, false, err
	}
	opts := options{DataDir: filepath.Join(dir, "meshra"), Addr: "127.0.0.1:7331"}
	overrides := map[string]string{}
	var rest []string
	wantHelp := false
	for i := 0; i < len(args); i++ {
		name, value, has := strings.Cut(args[i], "=")
		if name == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		// Preserve command option values verbatim, even when a message happens to
		// equal a global flag such as --json or --help.
		commandValue := false
		switch name {
		case "--harness", "--name", "--cwd", "--model", "--provider", "--display-name", "--allow-to", "--allow-operation", "--from", "--to", "--message", "--idempotency-key", "--timeout", "--parent-task", "--max-hops", "--after":
			commandValue = true
		}
		if commandValue && !has {
			rest = append(rest, args[i])
			i++
			if i >= len(args) {
				return opts, nil, false, usage("%s requires a value", name)
			}
			rest = append(rest, args[i])
			continue
		}
		switch name {
		case "--help", "-h":
			wantHelp = true
		case "--json":
			if has {
				b, e := strconv.ParseBool(value)
				if e != nil {
					return opts, nil, false, usage("invalid --json")
				}
				opts.JSON = b
			} else {
				opts.JSON = true
			}
		case "--data-dir", "--addr", "--config":
			if !has {
				i++
				if i >= len(args) {
					return opts, nil, false, usage("%s requires a value", name)
				}
				value = args[i]
			}
			if value == "" {
				return opts, nil, false, usage("%s requires a value", name)
			}
			overrides[name] = value
		default:
			rest = append(rest, args[i])
		}
	}
	if file := overrides["--config"]; file != "" {
		f, e := os.Open(file)
		if e != nil {
			return opts, nil, false, usage("read config: %v", e)
		}
		defer f.Close()
		d := json.NewDecoder(io.LimitReader(f, 65537))
		d.DisallowUnknownFields()
		if e = d.Decode(&opts); e != nil {
			return opts, nil, false, usage("invalid config: %v", e)
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			return opts, nil, false, usage("config must contain one JSON object")
		}
	}
	if value := overrides["--data-dir"]; value != "" {
		opts.DataDir = value
	}
	if value := overrides["--addr"]; value != "" {
		opts.Addr = value
	}
	if opts.DataDir == "" {
		return opts, nil, false, usage("dataDir must not be empty")
	}
	opts.DataDir, err = filepath.Abs(opts.DataDir)
	if err != nil {
		return opts, nil, false, err
	}
	opts.Addr, err = localAddress(opts.Addr)
	if err != nil {
		return opts, nil, false, err
	}
	return opts, rest, wantHelp, nil
}
func localAddress(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", usage("--addr must be loopback HOST:PORT: %v", err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", usage("--addr must bind a literal loopback IP (or localhost), never a wildcard")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", usage("--addr port must be 1..65535")
	}
	return net.JoinHostPort(host, port), nil
}

type flags struct {
	values     map[string][]string
	positional []string
}

func parse(args []string, spec map[string]bool) (flags, error) {
	out := flags{values: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out.positional = append(out.positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			out.positional = append(out.positional, arg)
			continue
		}
		name, value, has := strings.Cut(arg, "=")
		boolean, ok := spec[name]
		if !ok {
			return out, usage("unknown flag %s", name)
		}
		if boolean {
			if !has {
				value = "true"
			}
			if _, err := strconv.ParseBool(value); err != nil {
				return out, usage("%s requires true or false", name)
			}
		} else if !has {
			i++
			if i >= len(args) {
				return out, usage("%s requires a value", name)
			}
			value = args[i]
		}
		out.values[name] = append(out.values[name], value)
	}
	return out, nil
}
func (f flags) get(name string) string {
	v := f.values[name]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}
func (f flags) list(name string) []string {
	out := []string{}
	for _, v := range f.values[name] {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
func (f flags) required(names ...string) error {
	for _, name := range names {
		if f.get(name) == "" {
			return usage("%s is required", name)
		}
	}
	return nil
}
func oneID(f flags) (string, error) {
	if len(f.positional) != 1 || f.positional[0] == "" {
		return "", usage("exactly one agent/task ID is required")
	}
	return url.PathEscape(f.positional[0]), nil
}

func run(ctx context.Context, args []string, in io.Reader, out, errout io.Writer) error {
	opts, args, help, err := globals(args)
	if err != nil {
		return err
	}
	if help || len(args) == 0 {
		_, err = io.WriteString(out, helpText)
		return err
	}
	if args[0] == "serve" {
		if len(args) != 1 {
			return usage("serve accepts only global flags")
		}
		return serve(ctx, opts, errout)
	}
	if args[0] == "mcp" {
		if len(args) != 2 || args[1] != "stdio" {
			return usage("usage: meshra mcp stdio")
		}
		return proxy(ctx, opts, in, out)
	}
	method, path, body, follow, err := command(args)
	if err != nil {
		return err
	}
	token, err := humanToken(opts)
	if err != nil {
		return err
	}
	client := newClient(opts, token)
	if follow {
		return client.events(ctx, path, opts.JSON, out)
	}
	raw, err := client.call(ctx, method, path, body)
	if err != nil {
		return err
	}
	return printResult(out, raw, opts.JSON)
}
func command(args []string) (method, path string, body any, follow bool, err error) {
	method = http.MethodGet
	switch args[0] {
	case "agent":
		if len(args) < 2 {
			err = usage("agent requires a subcommand")
			return
		}
		sub := args[1]
		if sub == "create" {
			var f flags
			f, err = parse(args[2:], map[string]bool{"--harness": false, "--name": false, "--cwd": false, "--model": false, "--provider": false, "--display-name": false, "--allow-to": false, "--allow-operation": false, "--delegate": true})
			if err != nil {
				return
			}
			if len(f.positional) != 0 {
				err = usage("agent create takes flags, not positional arguments")
				return
			}
			if err = f.required("--harness", "--name", "--cwd"); err != nil {
				return
			}
			var cwd string
			cwd, err = filepath.Abs(f.get("--cwd"))
			if err != nil {
				return
			}
			body = model.CreateAgent{Harness: f.get("--harness"), Name: f.get("--name"), DisplayName: f.get("--display-name"), Cwd: cwd, Config: model.Config{Model: f.get("--model"), Provider: f.get("--provider")}, Policy: model.Policy{Destinations: f.list("--allow-to"), Operations: f.list("--allow-operation"), Delegate: f.get("--delegate") == "true"}}
			method = http.MethodPost
			path = "/v1/agents"
			return
		}
		var f flags
		f, err = parse(args[2:], nil)
		if err != nil {
			return
		}
		if sub == "list" {
			if len(f.positional) != 0 {
				err = usage("agent list takes no ID")
				return
			}
			path = "/v1/agents"
			return
		}
		var id string
		id, err = oneID(f)
		if err != nil {
			return
		}
		path = "/v1/agents/" + id
		switch sub {
		case "status":
		case "remove":
			method = http.MethodDelete
		case "start", "stop", "restart", "token":
			method = http.MethodPost
			path += "/" + sub
		default:
			err = usage("unknown agent command %q", sub)
		}
		return
	case "send", "request":
		var f flags
		f, err = parse(args[1:], map[string]bool{"--from": false, "--to": false, "--message": false, "--idempotency-key": false, "--timeout": false, "--parent-task": false, "--max-hops": false})
		if err != nil {
			return
		}
		if len(f.positional) != 0 {
			err = usage("send/request takes flags only")
			return
		}
		if err = f.required("--to", "--message"); err != nil {
			return
		}
		req := model.SendRequest{From: f.get("--from"), To: f.get("--to"), Text: f.get("--message"), IdempotencyKey: f.get("--idempotency-key"), ParentTaskID: f.get("--parent-task")}
		if raw := f.get("--timeout"); raw != "" {
			req.TimeoutSeconds, err = seconds(raw)
			if err != nil {
				return
			}
		}
		if raw := f.get("--max-hops"); raw != "" {
			req.MaxHops, err = strconv.Atoi(raw)
			if err != nil || req.MaxHops < 1 {
				err = usage("--max-hops must be a positive integer")
				return
			}
		}
		method = http.MethodPost
		path = "/v1/" + args[0]
		body = req
		return
	case "task":
		if len(args) < 2 {
			err = usage("task requires a subcommand")
			return
		}
		sub := args[1]
		spec := map[string]bool{}
		if sub == "events" {
			spec = map[string]bool{"--follow": true, "--after": false}
		}
		var f flags
		f, err = parse(args[2:], spec)
		if err != nil {
			return
		}
		var id string
		id, err = oneID(f)
		if err != nil {
			return
		}
		path = "/v1/tasks/" + id
		switch sub {
		case "status":
		case "cancel", "retry":
			method = http.MethodPost
			path += "/" + sub
		case "events":
			after := int64(0)
			if raw := f.get("--after"); raw != "" {
				after, err = strconv.ParseInt(raw, 10, 64)
				if err != nil || after < 0 {
					err = usage("--after must be a nonnegative integer")
					return
				}
			}
			follow = f.get("--follow") == "true"
			path += "/events?after=" + strconv.FormatInt(after, 10)
			if follow {
				path += "&follow=true"
			}
		default:
			err = usage("unknown task command %q", sub)
		}
		return
	default:
		err = usage("unknown command %q; use --help", args[0])
		return
	}
}
func seconds(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err == nil {
		if n > 0 {
			return n, nil
		}
		return 0, usage("--timeout must be positive")
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > 24*time.Hour {
		return 0, usage("--timeout must be positive seconds or a duration up to 24h")
	}
	return int((d + time.Second - 1) / time.Second), nil
}
func humanToken(opts options) (string, error) {
	if token := strings.TrimSpace(os.Getenv("MESHRA_TOKEN")); token != "" {
		return token, nil
	}
	data, err := os.ReadFile(filepath.Join(opts.DataDir, "admin.token"))
	if err != nil {
		return "", fmt.Errorf("read admin.token (start meshra serve first, or set MESHRA_TOKEN): %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("admin.token is empty")
	}
	return token, nil
}

type apiClient struct {
	base, token string
	http        *http.Client
}

func newClient(opts options, token string) *apiClient {
	return &apiClient{base: "http://" + opts.Addr, token: token, http: &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *apiClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to Meshra at %s: %w", c.base, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&payload)
		if payload.Error == "" {
			payload.Error = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, payload.Error)
	}
	return resp, nil
}
func (c *apiClient) call(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 8*1024*1024 {
		return nil, fmt.Errorf("response exceeds 8 MiB")
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("daemon returned invalid JSON")
	}
	return data, nil
}
func printResult(out io.Writer, raw json.RawMessage, machine bool) error {
	if machine {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, compact.String())
		return err
	}
	var token struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &token) == nil && token.Token != "" {
		_, err := fmt.Fprintln(out, token.Token)
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, pretty.String())
	return err
}
func (c *apiClient) events(ctx context.Context, path string, machine bool, out io.Writer) error {
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 4096), 256*1024)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := json.RawMessage(strings.TrimPrefix(line, "data: "))
		if machine {
			if err = printResult(out, raw, true); err != nil {
				return err
			}
		} else {
			var event model.Event
			if err = json.Unmarshal(raw, &event); err != nil {
				return err
			}
			if _, err = fmt.Fprintf(out, "%d  %s  %s\n", event.ID, event.Type, event.Data); err != nil {
				return err
			}
		}
	}
	return scan.Err()
}

func serve(ctx context.Context, opts options, errout io.Writer) error {
	listener, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return err
	}
	// Validate the actual socket as well as the requested address.
	tcp, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		listener.Close()
		return fmt.Errorf("refusing a non-loopback listener")
	}
	core, err := relay.Open(opts.DataDir, map[string]model.Adapter{"omp": omp.New(), "codex": codex.New(), "claude": claude.New(), "opencode": opencode.New()})
	if err != nil {
		listener.Close()
		return err
	}
	logger := slog.New(slog.NewJSONHandler(errout, nil))
	defer func() {
		if err := core.Close(); err != nil {
			logger.Error("relay shutdown could not close persistent state")
		}
		logger.Info("relay stopped")
	}()
	core.SetMCPURL("http://" + listener.Addr().String() + "/mcp")
	srv := &http.Server{Handler: api.New(core), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	logger.Info("relay listening", "address", listener.Addr().String())
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = srv.Shutdown(shutdown)
		if err != nil {
			_ = srv.Close()
		}
		<-done
		return err
	}
}

func proxy(ctx context.Context, opts options, in io.Reader, out io.Writer) error {
	// Closing real stdin on cancellation releases a blocked Scanner during signal
	// shutdown; tests and other finite in-memory readers need no background work.
	if closer, ok := in.(io.Closer); ok {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = closer.Close()
			case <-done:
			}
		}()
	}
	token := strings.TrimSpace(os.Getenv("MESHRA_TOKEN"))
	if token == "" {
		return fmt.Errorf("MCP stdio requires an agent MESHRA_TOKEN; admin.token is never read")
	}
	client := newClient(opts, token)
	raw, err := client.call(ctx, http.MethodGet, "/v1/identity", nil)
	if err != nil {
		return err
	}
	var identity struct {
		Admin   bool   `json:"admin"`
		AgentID string `json:"agentId"`
	}
	if err = json.Unmarshal(raw, &identity); err != nil {
		return err
	}
	if identity.Admin || identity.AgentID == "" {
		return fmt.Errorf("MCP stdio accepts only agent credentials, not the administrator token")
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 128*1024)
	for scanner.Scan() {
		line := json.RawMessage(scanner.Bytes())
		if !json.Valid(line) {
			return fmt.Errorf("invalid MCP JSON input")
		}
		resp, e := client.request(ctx, http.MethodPost, "/mcp", line)
		if e != nil {
			return e
		}
		if resp.StatusCode == http.StatusAccepted {
			resp.Body.Close()
			continue
		}
		data, e := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
		resp.Body.Close()
		if e != nil {
			return e
		}
		if len(data) > 1024*1024 || !json.Valid(data) {
			return fmt.Errorf("invalid MCP response")
		}
		var compact bytes.Buffer
		if e = json.Compact(&compact, data); e != nil {
			return e
		}
		if _, e = fmt.Fprintln(out, compact.String()); e != nil {
			return e
		}
	}
	return scanner.Err()
}
