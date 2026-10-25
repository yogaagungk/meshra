package netpolicy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newFixtureProxy(t *testing.T, policy *Policy, scope Scope, options ProxyOptions) (*Proxy, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if options.Active == nil {
		options.Active = func(ctx context.Context) error { return ctx.Err() }
	}
	dir, err := os.MkdirTemp(t.TempDir(), "egress")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	proxy, err := policy.ServeUnix(ctx, filepath.Join(dir, "egress.sock"), scope, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	return proxy, cancel
}
func unixProxyRequest(t *testing.T, socket, raw string) (*http.Response, net.Conn) {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = io.WriteString(conn, raw); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return response, conn
}

func TestUnixForwardProxyScopeHeadersAndCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("X-Hop") != "" {
			t.Error("proxy/hop credential headers escaped")
		}
		if request.Header.Get("Authorization") != "Bearer target-only" {
			t.Error("target credential was not retained")
		}
		_, _ = io.WriteString(w, "actual response")
	}))
	defer server.Close()
	policy, _, scope := newFixturePolicy(t, server.URL, true, nil)
	proxy, cancel := newFixtureProxy(t, policy, scope, ProxyOptions{})
	info, err := os.Stat(proxy.Path())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: info=%v err=%v", info, err)
	}
	authority := strings.TrimPrefix(server.URL, "http://")
	response, conn := unixProxyRequest(t, proxy.Path(), fmt.Sprintf("GET %s/resource HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer target-only\r\nProxy-Authorization: secret\r\nConnection: close, X-Hop\r\nX-Hop: strip\r\n\r\n", server.URL, authority))
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	conn.Close()
	if err != nil || response.StatusCode != 200 || string(body) != "actual response" || calls.Load() != 1 {
		t.Fatalf("forward: status=%d body=%q calls=%d err=%v", response.StatusCode, body, calls.Load(), err)
	}
	response, conn = unixProxyRequest(t, proxy.Path(), "GET http://unapproved.test/ HTTP/1.1\r\nHost: unapproved.test\r\nX-Meshra-Profile: trusted-profile\r\nConnection: close\r\n\r\n")
	response.Body.Close()
	conn.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("model header selected scope: %d", response.StatusCode)
	}
	cancel()
	_ = proxy.Close()
	if conn, err := net.Dial("unix", proxy.Path()); err == nil {
		conn.Close()
		t.Fatal("expired scope retained accepting socket")
	}
}

func connectTLSFixture(t *testing.T, socket string, server *httptest.Server) (net.Conn, *bufio.Reader) {
	t.Helper()
	authority := strings.TrimPrefix(server.URL, "https://")
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		conn.Close()
		t.Fatalf("CONNECT status=%v err=%v", response, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	tlsConn := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err = tlsConn.Handshake(); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return tlsConn, bufio.NewReader(tlsConn)
}

func TestUnixCONNECTRealTLSAndRevoke(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(w, "real TLS tunnel")
		w.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()
	policy, _, scope := newFixturePolicy(t, server.URL, true, nil)
	proxy, _ := newFixtureProxy(t, policy, scope, ProxyOptions{})
	conn, reader := connectTLSFixture(t, proxy.Path(), server)
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET /stream HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := make([]byte, len("real TLS tunnel"))
	if _, err = io.ReadFull(response.Body, body); err != nil || string(body) != "real TLS tunnel" {
		t.Fatalf("tunnel body=%q err=%v", body, err)
	}
	policy.Revoke("grant", 1)
	if _, err = io.ReadAll(response.Body); err == nil {
		t.Fatal("revoke left CONNECT stream open or falsely complete")
	}
	authority := strings.TrimPrefix(server.URL, "https://")
	denied, raw := unixProxyRequest(t, proxy.Path(), fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority))
	denied.Body.Close()
	raw.Close()
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("revoke allowed new tunnel: %d", denied.StatusCode)
	}
}

func TestCONNECTAlwaysRejectsMainListener(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { t.Error("self listener reached") }))
	defer server.Close()
	policy, _, scope := newFixturePolicy(t, server.URL, true, nil)
	if err := policy.SetListener(server.URL + "/mcp"); err != nil {
		t.Fatal(err)
	}
	proxy, _ := newFixtureProxy(t, policy, scope, ProxyOptions{})
	authority := strings.TrimPrefix(server.URL, "https://")
	response, conn := unixProxyRequest(t, proxy.Path(), fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority))
	response.Body.Close()
	conn.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("self listener allowed: %d", response.StatusCode)
	}
}

func TestProxyActiveCheckLimitsAndSocketSafety(t *testing.T) {
	policy, _, scope := newFixturePolicy(t, "https://example.com", false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "egress.sock")
	if _, err := policy.ServeUnix(context.Background(), path, scope, ProxyOptions{Active: func(context.Context) error { return nil }}); err == nil {
		t.Fatal("uncancelable scope accepted")
	}
	if _, err := policy.ServeUnix(ctx, path, scope, ProxyOptions{}); err == nil {
		t.Fatal("missing active-attempt validator accepted")
	}
	if err := os.WriteFile(path, []byte("do not remove"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ServeUnix(ctx, path, scope, ProxyOptions{Active: func(context.Context) error { return nil }}); err == nil {
		t.Fatal("existing socket path overwritten")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "do not remove" {
		t.Fatal("existing file modified")
	}
	var active atomic.Bool
	active.Store(true)
	proxy, _ := newFixtureProxy(t, policy, scope, ProxyOptions{Active: func(context.Context) error {
		if !active.Load() {
			return ErrRevoked
		}
		return nil
	}})
	active.Store(false)
	response, conn := unixProxyRequest(t, proxy.Path(), "GET https://example.com HTTP/1.1\r\nHost: example.com\r\n\r\n")
	response.Body.Close()
	conn.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("inactive proxy dispatched: %d", response.StatusCode)
	}
}
func TestProxyBoundsAcceptedStreamsAndCancelsTunnel(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { t.Error("unprompted HTTP request") }))
	defer server.Close()
	policy, _, scope := newFixturePolicy(t, server.URL, true, nil)
	proxy, cancel := newFixtureProxy(t, policy, scope, ProxyOptions{MaxStreams: 1})
	authority := strings.TrimPrefix(server.URL, "https://")
	first, err := net.DialTimeout("unix", proxy.Path(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	_ = first.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = fmt.Fprintf(first, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
	response, err := http.ReadResponse(bufio.NewReader(first), &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("first tunnel=%v err=%v", response, err)
	}
	second, err := net.DialTimeout("unix", proxy.Path(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(second, "GET http://unapproved.test/ HTTP/1.1\r\nHost: unapproved.test\r\n\r\n")
	var data [1]byte
	if _, err = second.Read(data[:]); err == nil {
		t.Fatal("stream limit accepted another connection")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("over-capacity socket was retained instead of closed")
	}
	cancel()
	if _, err = first.Read(data[:]); err == nil {
		t.Fatal("scope cancellation left tunnel open")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("scope cancellation did not promptly close tunnel")
	}
}

func TestWorkerOriginsNarrowPersistentGrants(t *testing.T) {
	var forbiddenCalls atomic.Int32
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "allowed") }))
	defer allowed.Close()
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forbiddenCalls.Add(1)
		_, _ = io.WriteString(w, "outside worker")
	}))
	defer forbidden.Close()
	tlsForbidden := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forbiddenCalls.Add(1) }))
	defer tlsForbidden.Close()
	policy, store, scope := newFixturePolicy(t, allowed.URL, true, nil)
	for _, origin := range []string{forbidden.URL, tlsForbidden.URL} {
		store.save(Grant{ID: origin, Revision: 1, Scope: scope, Origin: origin, AllowPrivate: true, Enabled: true})
	}
	origins := []string{allowed.URL}
	proxy, _ := newFixtureProxy(t, policy, scope, ProxyOptions{AllowedOrigins: origins})
	origins[0] = forbidden.URL // A caller cannot mutate an already admitted worker.
	for _, entry := range []struct {
		url    string
		status int
	}{{allowed.URL, http.StatusOK}, {forbidden.URL, http.StatusForbidden}} {
		host := strings.TrimPrefix(entry.url, "http://")
		response, conn := unixProxyRequest(t, proxy.Path(), fmt.Sprintf("GET %s/ HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", entry.url, host))
		response.Body.Close()
		conn.Close()
		if response.StatusCode != entry.status {
			t.Fatalf("%s: status=%d, want=%d", entry.url, response.StatusCode, entry.status)
		}
	}
	authority := strings.TrimPrefix(tlsForbidden.URL, "https://")
	response, conn := unixProxyRequest(t, proxy.Path(), fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", authority, authority))
	response.Body.Close()
	conn.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-worker CONNECT status=%d", response.StatusCode)
	}
	if forbiddenCalls.Load() != 0 {
		t.Fatal("worker reached a scope grant outside its immutable origin selection")
	}
}
