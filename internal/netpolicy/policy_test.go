package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)
func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) { return f(ctx, network, host) }

type grantFixture struct { mu sync.Mutex; grants map[string]Grant; calls int }
func (f *grantFixture) lookup(ctx context.Context, scope Scope, origin string) (Grant, error) {
	f.mu.Lock(); defer f.mu.Unlock()
	f.calls++
	grant, exists := f.grants[origin]
	if !exists || scope != grant.Scope { return Grant{}, ErrDenied }
	return grant, nil
}
func (f *grantFixture) save(grant Grant) { f.mu.Lock(); f.grants[grant.Origin] = grant; f.mu.Unlock() }
func newFixturePolicy(t *testing.T, origin string, allowPrivate bool, resolver Resolver) (*Policy, *grantFixture, Scope) {
	t.Helper()
	scope := Scope{ProfileID: "trusted-profile"}
	store := &grantFixture{grants: map[string]Grant{origin: {ID: "grant", Revision: 1, Scope: scope, Origin: origin, AllowPrivate: allowPrivate, Enabled: true}}}
	policy := New(store.lookup, Options{Resolver: resolver, ConnectTimeout: time.Second})
	if err := policy.SetListener("http://127.0.0.1:1/mcp"); err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = policy.Close() })
	return policy, store, scope
}

func TestParseOriginStrictAndCanonical(t *testing.T) {
	valid := map[string]string{
		"HTTPS://EXAMPLE.COM:443/": "https://example.com",
		"http://example.com.": "http://example.com",
		"http://[::ffff:127.0.0.1]:8080": "http://127.0.0.1:8080",
		"https://[2001:4860:4860::8888]": "https://[2001:4860:4860::8888]",
	}
	for raw, expected := range valid { origin, err := ParseOrigin(raw); if err != nil || origin.String() != expected { t.Errorf("%q: origin=%v err=%v", raw, origin, err) } }
	for _, raw := range []string{"", "example.com", "http://user:pass@example.com", "http://example.com/x", "http://example.com?", "http://example.com?q=1", "http://example.com#", "http://example.com#x", "http://*.example.com", "http://127.0.0.0/8", "file:///tmp/file", "http://example.com:0", "http://example.com:65536", "http://example.com:", "http://[fe80::1%25eth0]", "http://☃.example"} {
		if _, err := ParseOrigin(raw); err == nil { t.Errorf("accepted %q", raw) }
	}
}

func TestAddressSecurityAndSelfAliases(t *testing.T) {
	resolver := resolverFunc(func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		switch host {
		case "public.test": return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		case "mixed.test": return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("::ffff:127.0.0.1")}, nil
		case "relay.test", "alias.test": return []netip.Addr{netip.MustParseAddr("::ffff:127.0.0.1")}, nil
		default: return nil, errors.New("not found")
		}
	})
	policy := New(nil, Options{Resolver: resolver})
	defer policy.Close()
	if _, err := policy.ValidateGrant(context.Background(), "https://public.test", false); err == nil { t.Fatal("grants available without listener configuration") }
	if err := policy.SetListener("http://relay.test:37731/mcp"); err != nil { t.Fatal(err) }
	for _, raw := range []string{"http://127.0.0.1:80", "https://[::1]", "http://[::ffff:127.0.0.1]", "http://10.0.0.1", "http://172.16.0.1", "http://192.168.0.1", "http://169.254.169.254", "http://100.100.100.200", "https://[fe80::1]", "https://[fc00::1]", "https://[64:ff9b::7f00:1]", "https://mixed.test"} {
		if _, err := policy.ValidateGrant(context.Background(), raw, false); err == nil { t.Errorf("unconfirmed private origin accepted: %s", raw) }
	}
	for _, raw := range []string{"http://alias.test:37731", "https://127.0.0.1:37731", "https://[::ffff:127.0.0.1]:37731"} {
		if _, err := policy.ValidateGrant(context.Background(), raw, true); err == nil { t.Errorf("self alias accepted: %s", raw) }
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "https://public.test", "http://192.168.0.1"} {
		if _, err := policy.ValidateGrant(context.Background(), raw, true); err != nil { t.Errorf("exact confirmed origin denied: %s: %v", raw, err) }
	}
	if err := policy.SetListener("not-a-listener"); err == nil { t.Fatal("invalid listener accepted") }
	if _, err := policy.ValidateGrant(context.Background(), "https://public.test", false); err == nil { t.Fatal("invalid listener retained old authority") }
}

func TestHTTPClientDNSPinningAndFreshAuthorization(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { received.Add(1); _, _ = io.WriteString(w, "pinned") }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	origin := "http://fixture.test:" + port
	var resolutions atomic.Int32
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { resolutions.Add(1); return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil })
	policy, store, scope := newFixturePolicy(t, origin, true, resolver)
	client, err := policy.Client(scope, ClientOptions{Origin: origin, CredentialOrigin: origin})
	if err != nil { t.Fatal(err) }
	for range 2 {
		response, err := client.Get(origin + "/resource")
		if err != nil { t.Fatal(err) }
		body, err := io.ReadAll(response.Body); _ = response.Body.Close()
		if err != nil || string(body) != "pinned" { t.Fatalf("body=%q err=%v", body, err) }
	}
	if resolutions.Load() != 2 || received.Load() != 2 { t.Fatalf("DNS was not resolved once per new pinned connection: dns=%d requests=%d", resolutions.Load(), received.Load()) }
	store.mu.Lock(); lookups := store.calls; store.mu.Unlock()
	if lookups < 6 { t.Fatalf("grant was not reread at dispatch/connect: %d", lookups) }
	store.save(Grant{ID: "grant", Revision: 2, Scope: scope, Origin: origin, Enabled: false})
	if response, err := client.Get(origin); err == nil { response.Body.Close(); t.Fatal("disabled grant allowed new request") }
	if received.Load() != 2 { t.Fatal("disabled grant reached fixture") }
	wrong, err := policy.Client(Scope{ProfileID: scope.ProfileID, ConnectionID: "untrusted"}, ClientOptions{Origin: origin})
	if err != nil { t.Fatal(err) }
	if response, err := wrong.Get(origin); err == nil { response.Body.Close(); t.Fatal("empty connection selector acted as wildcard") }
}

func TestDNSRebindPrivateAnswerDenied(t *testing.T) {
	policy, _, _ := newFixturePolicy(t, "https://rebind.test", false, resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil }))
	origin, _ := ParseOrigin("https://rebind.test")
	if conn, err := policy.dial(context.Background(), Scope{ProfileID: "trusted-profile"}, origin, nil); err == nil { conn.Close(); t.Fatal("DNS rebind was allowed") }
}

func TestClientNoRedirectCredentialLeakProxyOrReplay(t *testing.T) {
	var targetCalls, proxyCalls, sourceCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { sourceCalls.Add(1); if request.Header.Get("Authorization") != "Bearer fixture-secret" { t.Error("credential did not reach bound origin") }; http.Redirect(w, request, target.URL+"/leak", http.StatusFound) }))
	defer source.Close()
	environmentProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { proxyCalls.Add(1) }))
	defer environmentProxy.Close()
	t.Setenv("HTTP_PROXY", environmentProxy.URL)
	t.Setenv("HTTPS_PROXY", environmentProxy.URL)
	t.Setenv("ALL_PROXY", environmentProxy.URL)
	policy, _, scope := newFixturePolicy(t, source.URL, true, nil)
	if _, err := policy.Client(scope, ClientOptions{Origin: source.URL, CredentialOrigin: target.URL}); err == nil { t.Fatal("different credential origin accepted") }
	client, err := policy.Client(scope, ClientOptions{Origin: source.URL, CredentialOrigin: source.URL})
	if err != nil { t.Fatal(err) }
	request, _ := http.NewRequest(http.MethodPost, source.URL, strings.NewReader("one effect"))
	request.Header.Set("Authorization", "Bearer fixture-secret")
	response, err := client.Do(request)
	if response != nil { response.Body.Close() }
	if err == nil || sourceCalls.Load() != 1 || targetCalls.Load() != 0 || proxyCalls.Load() != 0 { t.Fatalf("redirect/proxy/replay boundary: err=%v source=%d target=%d proxy=%d", err, sourceCalls.Load(), targetCalls.Load(), proxyCalls.Load()) }
	request, _ = http.NewRequest(http.MethodGet, target.URL, nil)
	if response, err := client.Do(request); err == nil { response.Body.Close(); t.Fatal("client changed origin") }
}

func TestClientResponseBoundAndActiveContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { w.(http.Flusher).Flush(); _, _ = io.WriteString(w, strings.Repeat("x", 100)) }))
	defer server.Close()
	policy, _, scope := newFixturePolicy(t, server.URL, true, nil)
	var active atomic.Bool
	active.Store(true)
	client, err := policy.Client(scope, ClientOptions{Origin: server.URL, MaxResponseBytes: 10, Active: func(context.Context) error { if !active.Load() { return ErrRevoked }; return nil }})
	if err != nil { t.Fatal(err) }
	response, err := client.Get(server.URL)
	if err != nil { t.Fatal(err) }
	body, err := io.ReadAll(response.Body); response.Body.Close()
	if len(body) > 10 || !errors.Is(err, ErrLimit) { t.Fatalf("unbounded body: len=%d err=%v", len(body), err) }
	active.Store(false)
	if response, err := client.Get(server.URL); err == nil { response.Body.Close(); t.Fatal("inactive attempt dispatched") }
}

func TestRevocationDuringDNSAndLiveResponse(t *testing.T) {
	var policy *Policy
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { policy.Revoke("grant", 1); return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil })
	policy, _, scope := newFixturePolicy(t, "http://wait.test:8080", true, resolver)
	origin, _ := ParseOrigin("http://wait.test:8080")
	if conn, err := policy.dial(context.Background(), scope, origin, nil); err == nil { conn.Close(); t.Fatal("revoked during resolution dispatched") }

	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { w.Header().Set("Content-Type", "text/plain"); _, _ = io.WriteString(w, "started"); w.(http.Flusher).Flush(); close(started); <-request.Context().Done() }))
	defer server.Close()
	live, _, scope := newFixturePolicy(t, server.URL, true, nil)
	client, _ := live.Client(scope, ClientOptions{Origin: server.URL})
	response, err := client.Get(server.URL)
	if err != nil { t.Fatal(err) }
	<-started
	live.Revoke("grant", 1)
	done := make(chan error, 1)
	go func() { _, e := io.ReadAll(response.Body); response.Body.Close(); done <- e }()
	select { case err = <-done: if err == nil { t.Fatal("revoked stream ended as successful complete response") }; case <-time.After(2*time.Second): t.Fatal("revoke did not close live stream") }
	if response, err := client.Get(server.URL); err == nil { response.Body.Close(); t.Fatal("stale store revision bypassed revoked stream generation") }
}

func TestClientUncertainDisconnectNeverReplayed(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { calls.Add(1); conn, _, err := w.(http.Hijacker).Hijack(); if err != nil { return }; conn.Close() }))
	defer server.Close()
	policy, _, scope := newFixturePolicy(t, server.URL, true, nil)
	client, _ := policy.Client(scope, ClientOptions{Origin: server.URL})
	request, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("effect"))
	request.Header.Set("Idempotency-Key", "not-replay-authority")
	if response, err := client.Do(request); err == nil { response.Body.Close(); t.Fatal("expected uncertain disconnect") }
	if calls.Load() != 1 { t.Fatalf("replayed effect %d times", calls.Load()) }
}

func TestListenerChangeInvalidatesPendingConnect(t *testing.T) {
	var policy *Policy
	policy, _, scope := newFixturePolicy(t, "http://change.test:8080", true, resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { _ = policy.SetListener("http://127.0.0.1:8080"); return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil }))
	origin, _ := ParseOrigin("http://change.test:8080")
	if conn, err := policy.dial(context.Background(), scope, origin, nil); err == nil { conn.Close(); t.Fatal("listener change allowed self-connect") }
}

func ExampleParseOrigin() {
	origin, _ := ParseOrigin("HTTPS://EXAMPLE.COM:443/")
	fmt.Println(origin.String())
	// Output: https://example.com
}
