package netpolicy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ClientOptions fixes the origin independently of request/model content. Set
// CredentialOrigin whenever attaching credentials, including custom headers.
type ClientOptions struct {
	Origin string
	CredentialOrigin string
	Timeout time.Duration
	MaxResponseBytes int64
	MaxRequestBytes int64
	Active func(context.Context) error
}

// Client never uses environment proxies, redirects, idle connection reuse or
// transport retries. A failed request must be surfaced, not replayed by callers.
func (p *Policy) Client(scope Scope, options ClientOptions) (*http.Client, error) {
	origin, err := ParseOrigin(options.Origin)
	if err != nil { return nil, err }
	if err = credentialBinding(origin, options.CredentialOrigin); err != nil { return nil, err }
	if options.Timeout <= 0 { options.Timeout = 30*time.Second }
	if options.Timeout > 5*time.Minute { return nil, errors.New("network timeout exceeds five minutes") }
	if options.MaxResponseBytes == 0 { options.MaxResponseBytes = 2 << 20 }
	if options.MaxRequestBytes == 0 { options.MaxRequestBytes = 25 << 20 }
	if options.MaxResponseBytes < 1 || options.MaxResponseBytes > 32 << 20 || options.MaxRequestBytes < 1 || options.MaxRequestBytes > 32 << 20 { return nil, errors.New("network byte bounds must be 1..32 MiB") }
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// The transport cannot choose a destination different from the URL
			// origin that the outer round tripper has already checked.
			if network != "tcp" || addr != origin.Address() { return nil, ErrDenied }
			return p.dial(ctx, scope, origin, options.Active)
		},
		DisableKeepAlives: true,
		DisableCompression: true,
		ForceAttemptHTTP2: false,
		TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10*time.Second,
		ResponseHeaderTimeout: options.Timeout,
		ExpectContinueTimeout: time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{Transport: &originTransport{policy: p, transport: transport, scope: scope, origin: origin, options: options}, Timeout: options.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrDenied }}, nil
}

type originTransport struct { policy *Policy; transport *http.Transport; scope Scope; origin Origin; options ClientOptions }
func (t *originTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	o, err := originURL(request.URL)
	if err != nil || o != t.origin || request.URL.Fragment != "" || request.Host != "" && !strings.EqualFold(request.Host, request.URL.Host) { return nil, ErrDenied }
	if _, err = t.policy.authorize(request.Context(), t.scope, o, t.options.Active); err != nil { return nil, err }
	if request.ContentLength > t.options.MaxRequestBytes { return nil, ErrLimit }
	cloned := request.Clone(request.Context())
	cloned.URL.Scheme = o.Scheme
	cloned.URL.Host = strings.TrimPrefix(o.String(), o.Scheme+"://")
	cloned.Host = cloned.URL.Host
	cloned.GetBody = nil // Do not make a body eligible for replay.
	cloned.Header.Del("Proxy-Authorization")
	cloned.Header.Del("Proxy-Connection")
	if cloned.Body != nil { cloned.Body = &boundedBody{ReadCloser: cloned.Body, remaining: t.options.MaxRequestBytes} }
	response, err := t.transport.RoundTrip(cloned)
	if err != nil { return nil, err }
	if response.ContentLength > t.options.MaxResponseBytes { _ = response.Body.Close(); return nil, ErrLimit }
	response.Body = &boundedBody{ReadCloser: response.Body, remaining: t.options.MaxResponseBytes}
	return response, nil
}
func (t *originTransport) CloseIdleConnections() { t.transport.CloseIdleConnections() }

type boundedBody struct { io.ReadCloser; remaining int64 }
func (b *boundedBody) Read(data []byte) (int, error) {
	if b.remaining < 0 { return 0, ErrLimit }
	if int64(len(data)) > b.remaining+1 { data = data[:b.remaining+1] }
	n, err := b.ReadCloser.Read(data)
	if int64(n) > b.remaining { allowed := int(b.remaining); b.remaining = -1; _ = b.ReadCloser.Close(); return allowed, ErrLimit }
	b.remaining -= int64(n)
	return n, err
}
