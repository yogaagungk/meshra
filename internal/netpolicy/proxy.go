package netpolicy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ProxyOptions struct {
	Active func(context.Context) error
	// AllowedOrigins narrows this worker beyond persistent scope grants. An
	// empty list leaves origin selection to that scope's reviewed grants.
	AllowedOrigins    []string
	MaxStreams        int
	MaxStreamBytes    int64
	MaxStreamDuration time.Duration
}

type Proxy struct {
	policy      *Policy
	scope       Scope
	options     ProxyOptions
	cancel      context.CancelFunc
	path        string
	listener    net.Listener
	server      *http.Server
	semaphore   chan struct{}
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	once        sync.Once
}

// ServeUnix creates a socket dedicated to this trusted scope and active attempt.
// Its containing directory must already be private; existing paths are never
// removed or followed. Only this socket, not its directory, belongs in a worker.
func (p *Policy) ServeUnix(ctx context.Context, path string, scope Scope, options ProxyOptions) (*Proxy, error) {
	if ctx.Done() == nil || options.Active == nil {
		return nil, errors.New("egress proxy requires an active cancelable scope")
	}
	if scope.ProfileID == "" && scope.ConnectionID == "" {
		return nil, ErrDenied
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("proxy socket path must be absolute")
	}
	dir := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || canonical != dir {
		return nil, errors.New("proxy socket directory must not contain symlinks")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("proxy socket directory must have private permissions")
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("proxy socket path already exists or cannot be inspected")
	}
	if options.MaxStreams == 0 {
		options.MaxStreams = 8
	}
	if options.MaxStreamBytes == 0 {
		options.MaxStreamBytes = 32 << 20
	}
	if options.MaxStreamDuration == 0 {
		options.MaxStreamDuration = 5 * time.Minute
	}
	if options.MaxStreams < 1 || options.MaxStreams > 32 || options.MaxStreamBytes < 1 || options.MaxStreamBytes > 32<<20 || options.MaxStreamDuration < time.Second || options.MaxStreamDuration > 5*time.Minute {
		return nil, errors.New("invalid proxy stream limits")
	}
	if len(options.AllowedOrigins) > 32 {
		return nil, errors.New("too many worker origins")
	}
	origins := make([]string, len(options.AllowedOrigins))
	for i, raw := range options.AllowedOrigins {
		origin, err := ParseOrigin(raw)
		if err != nil {
			return nil, err
		}
		origins[i] = origin.String()
	}
	options.AllowedOrigins = origins
	if err = options.Active(ctx); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	listener = &boundedListener{Listener: listener, slots: make(chan struct{}, options.MaxStreams)}
	ctx, cancel := context.WithCancel(ctx)
	proxy := &Proxy{policy: p, scope: scope, options: options, cancel: cancel, path: path, listener: listener, semaphore: make(chan struct{}, options.MaxStreams), connections: make(map[net.Conn]struct{})}
	proxy.server = &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: options.MaxStreamDuration, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return ctx }, ConnState: func(conn net.Conn, state http.ConnState) {
		proxy.mu.Lock()
		if state == http.StateClosed {
			delete(proxy.connections, conn)
		} else {
			proxy.connections[conn] = struct{}{}
		}
		proxy.mu.Unlock()
	}}
	context.AfterFunc(ctx, func() { _ = proxy.Close() })
	go func() { _ = proxy.server.Serve(listener); _ = proxy.Close() }()
	return proxy, nil
}
func (p *Proxy) Path() string { return p.path }
func (p *Proxy) Close() error {
	p.once.Do(func() {
		p.cancel()
		_ = p.server.Close()
		_ = p.listener.Close()
		p.mu.Lock()
		for conn := range p.connections {
			_ = conn.Close()
		}
		p.mu.Unlock()
	})
	return nil
}
func (p *Proxy) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	select {
	case p.semaphore <- struct{}{}:
		defer func() { <-p.semaphore }()
	default:
		http.Error(w, "egress stream capacity reached", http.StatusTooManyRequests)
		return
	}
	if err := p.options.Active(request.Context()); err != nil {
		http.Error(w, "egress scope expired", http.StatusForbidden)
		return
	}
	if request.Method == http.MethodConnect {
		p.connect(w, request)
		return
	}
	p.forward(w, request)
}
func (p *Proxy) allows(origin Origin) bool {
	if len(p.options.AllowedOrigins) == 0 {
		return true
	}
	for _, allowed := range p.options.AllowedOrigins {
		if allowed == origin.String() {
			return true
		}
	}
	return false
}

func (p *Proxy) forward(w http.ResponseWriter, request *http.Request) {
	origin, err := originURL(request.URL)
	if err != nil || origin.Scheme != "http" {
		http.Error(w, "absolute http proxy URL required", http.StatusBadRequest)
		return
	}
	if !p.allows(origin) {
		http.Error(w, "origin is outside this worker's configuration", http.StatusForbidden)
		return
	}
	client, err := p.policy.Client(p.scope, ClientOptions{Origin: origin.String(), Timeout: p.options.MaxStreamDuration, MaxResponseBytes: p.options.MaxStreamBytes, MaxRequestBytes: p.options.MaxStreamBytes, Active: p.options.Active})
	if err != nil {
		http.Error(w, "egress destination denied", http.StatusForbidden)
		return
	}
	outgoing := request.Clone(request.Context())
	outgoing.RequestURI = ""
	outgoing.Close = true
	stripHopHeaders(outgoing.Header)
	response, err := client.Do(outgoing)
	if err != nil {
		http.Error(w, "egress destination denied or unavailable", http.StatusForbidden)
		return
	}
	defer response.Body.Close()
	stripHopHeaders(response.Header)
	for name, values := range response.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.Header().Set("Connection", "close")
	w.WriteHeader(response.StatusCode)
	if _, err = io.Copy(w, response.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func stripHopHeaders(headers http.Header) {
	for _, value := range headers.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			headers.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		headers.Del(name)
	}
}

func (p *Proxy) connect(w http.ResponseWriter, request *http.Request) {
	if request.URL.User != nil || request.URL.Path != "" || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" || request.URL.Host != request.Host {
		http.Error(w, "invalid CONNECT authority", http.StatusBadRequest)
		return
	}
	host, port, err := net.SplitHostPort(request.Host)
	if err != nil || host == "" || port == "" {
		http.Error(w, "CONNECT requires host and port", http.StatusBadRequest)
		return
	}
	origin, err := ParseOrigin("https://" + net.JoinHostPort(host, port))
	if err != nil {
		http.Error(w, "invalid CONNECT origin", http.StatusBadRequest)
		return
	}
	if !p.allows(origin) {
		http.Error(w, "origin is outside this worker's configuration", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), p.options.MaxStreamDuration)
	defer cancel()
	upstream, err := p.policy.dial(ctx, p.scope, origin, p.options.Active)
	if err != nil {
		http.Error(w, "egress destination denied or unavailable", http.StatusForbidden)
		return
	}
	defer upstream.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT unavailable", http.StatusInternalServerError)
		return
	}
	downstream, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer downstream.Close()
	defer func() { p.mu.Lock(); delete(p.connections, downstream); p.mu.Unlock() }()
	deadline := time.Now().Add(p.options.MaxStreamDuration)
	_ = downstream.SetDeadline(deadline)
	_ = upstream.SetDeadline(deadline)
	if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = buffered.Flush(); err != nil {
		return
	}
	var used atomic.Int64
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = downstream.Close(); _ = upstream.Close() })
	defer stop()
	go func() {
		copyTunnel(upstream, buffered, &used, p.options.MaxStreamBytes)
		_ = downstream.Close()
		_ = upstream.Close()
		close(done)
	}()
	copyTunnel(downstream, upstream, &used, p.options.MaxStreamBytes)
	_ = downstream.Close()
	_ = upstream.Close()
	<-done
}

func copyTunnel(destination io.Writer, source io.Reader, used *atomic.Int64, maximum int64) {
	var buffer [32 << 10]byte
	for {
		n, err := source.Read(buffer[:])
		if n > 0 {
			if used.Add(int64(n)) > maximum {
				return
			}
			written, writeErr := destination.Write(buffer[:n])
			if writeErr != nil || written != n {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Bound accepted sockets as well as dispatched requests: slow headers must not
// allow a worker to consume unbounded daemon file descriptors.
type boundedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &boundedConn{Conn: conn, release: func() { <-l.slots }}, nil
		default:
			_ = conn.Close()
		}
	}
}

type boundedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *boundedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }
