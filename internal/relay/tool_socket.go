package relay

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"meshra/internal/model"
	"meshra/internal/toolmcp"
)

// MCPHandler is an optional transport extension on the trusted executor. It
// replaces the legacy server for profile credentials; it never wraps it.
func (b *toolBroker) MCPHandler() http.Handler {
	b.socketMu.Lock()
	defer b.socketMu.Unlock()
	if b.mcpHandler == nil {
		b.mcpHandler = toolmcp.New(b, func(ctx context.Context, _ *http.Request) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			b.relay.mu.Lock()
			defer b.relay.mu.Unlock()
			_, _, _, err := b.activeLocked()
			return err
		})
	}
	return b.mcpHandler
}

type activeToolSocket struct {
	path, token string
	server      *http.Server
	handler     *toolmcp.Handler
	done        chan struct{}
	once        sync.Once
}

func (s *activeToolSocket) Close() {
	s.once.Do(func() { s.handler.Close(); _ = s.server.Close(); <-s.done; _ = os.Remove(s.path) })
}

// ScopedMCP is a trusted adapter extension. This credential is not registered in
// the main bearer store and can never authorize legacy REST or MCP operations.
func (b *toolBroker) ScopedMCP(ctx context.Context) (string, string, error) {
	b.socketMu.Lock()
	defer b.socketMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	b.relay.mu.Lock()
	_, _, _, err := b.activeAttemptLocked(false)
	b.relay.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	if b.socket != nil {
		return b.socket.path, b.socket.token, nil
	}
	dir := filepath.Join(filepath.Dir(b.relay.lock.Name()), "tool-sockets")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", model.Err(503, "private tool socket directory is unsafe")
	}
	path := filepath.Join(dir, "attempt-"+randomID()+".sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		return "", "", err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		os.Remove(path)
		return "", "", err
	}
	endpoint := &activeToolSocket{path: path, token: randomID() + randomID(), done: make(chan struct{})}
	endpoint.handler = toolmcp.New(b, func(ctx context.Context, r *http.Request) error {
		auth := strings.Fields(r.Header.Get("Authorization"))
		if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") || subtle.ConstantTimeCompare([]byte(auth[1]), []byte(endpoint.token)) != 1 {
			return model.Err(401, "private attempt credential required")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Discovery can precede session/new's response; Call still requires the
		// allocated native session before admitting any tool invocation.
		b.relay.mu.Lock()
		defer b.relay.mu.Unlock()
		_, _, _, err := b.activeAttemptLocked(false)
		return err
	})
	endpoint.server = &http.Server{Handler: endpoint.handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384, BaseContext: func(net.Listener) context.Context { return b.ctx }}
	b.socket = endpoint
	go func() { defer close(endpoint.done); _ = endpoint.server.Serve(listener) }()
	return endpoint.path, endpoint.token, nil
}
func (r *Relay) cleanupToolSockets() error {
	dir := filepath.Join(filepath.Dir(r.lock.Name()), "tool-sockets")
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return model.Err(503, "private tool socket directory is unsafe")
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "attempt-") || !strings.HasSuffix(name, ".sock") || !validAttachmentID(strings.TrimSuffix(strings.TrimPrefix(name, "attempt-"), ".sock")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSocket == 0 {
			continue
		}
		if err = os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
