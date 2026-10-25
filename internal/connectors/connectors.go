// Package connectors implements explicit outbound messaging transports.
//
// Every transport dials the provider outward: Meshra never listens on an
// inbound public port. Provider credentials are resolved by the relay secret
// vault and are never part of a DTO, an event, a log line, an error message or
// a stored row.
package connectors

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Kind identifies a messaging provider. Only these exact names are accepted.
type Kind string

const (
	KindSlack    Kind = "slack"
	KindDiscord  Kind = "discord"
	KindTelegram Kind = "telegram"
)

// ParseKind accepts only the exact supported provider names.
func ParseKind(raw string) (Kind, bool) {
	switch Kind(raw) {
	case KindSlack:
		return KindSlack, true
	case KindDiscord:
		return KindDiscord, true
	case KindTelegram:
		return KindTelegram, true
	}
	return "", false
}

const (
	// MaxTextBytes bounds a single inbound or outbound message body.
	MaxTextBytes = 64 * 1024
	// MaxFileBytes bounds one transferred file.
	MaxFileBytes = 25 << 20
	// MaxFiles bounds the files carried by one message.
	MaxFiles = 4
	// MaxAuthorBytes bounds a displayed author name.
	MaxAuthorBytes = 128
	// MaxIdentifierBytes bounds a provider identifier used in a row key.
	MaxIdentifierBytes = 256
)

// Event is a normalized inbound provider message. It carries no credential and
// no provider URL: only opaque identifiers the relay matches against an
// operator-reviewed route.
type Event struct {
	Kind        Kind
	EventID     string
	ThreadKey   string
	WorkspaceID string
	ChannelID   string
	AuthorID    string
	AuthorName  string
	Text        string
	Files       []File
	ReceivedAt  time.Time
	Bot         bool
}

// File is an inbound attachment reference. URL is provider specific and is
// only usable through the owning transport, which validates the origin.
type File struct {
	ID   string
	Name string
	MIME string
	Size int64
	URL  string
}

// Message is an outbound reply.
type Message struct {
	Text  string
	Files []OutgoingFile
}

// OutgoingFile is one attachment to upload with a reply. Open returns a fresh
// reader; the transport closes it.
type OutgoingFile struct {
	Name string
	MIME string
	Size int64
	Open func() (io.ReadCloser, error)
}

// PostResult reports the provider identifier of a posted message.
type PostResult struct {
	RemoteID string
}

// Sink receives one inbound event. It returns true only when the event is
// durably recorded and the provider may acknowledge it. A false result asks
// the transport to redeliver the event instead of acknowledging it.
type Sink func(Event) bool

// Transport receives provider events and posts replies. Implementations must
// be safe for one receive loop and one delivery loop at a time.
type Transport interface {
	// Kind reports the provider.
	Kind() Kind
	// Receive runs until the context is cancelled, delivering events.
	Receive(ctx context.Context, sink Sink) error
	// Post sends one reply into the recorded thread.
	Post(ctx context.Context, threadKey string, message Message) (PostResult, error)
	// Download opens one inbound file for reading.
	Download(ctx context.Context, file File) (io.ReadCloser, error)
	// Test verifies the stored credential without sending a message.
	Test(ctx context.Context) error
	// Close releases provider resources.
	Close() error
}

// Egress grants a transport access to exactly the destinations the operator
// approved for the connector. A denied lookup fails closed.
type Egress struct {
	// Client returns an HTTP client bound to one exact approved origin.
	Client func(ctx context.Context, origin string) (*http.Client, error)
	// Dial opens a TCP connection to one exact approved provider address.
	Dial func(ctx context.Context, address string) (net.Conn, error)
}

// Config is the admitted configuration of one connector.
type Config struct {
	ConnectorID string
	Kind        Kind
	// Token is the provider bot token. It never leaves the transport.
	Token string
	// AppToken is the Slack application-level token used for Socket Mode.
	AppToken string
	// APIBase overrides the provider API base; it must be an exact origin.
	APIBase string
	// Cursor reads the persisted provider progress, such as the Telegram
	// update offset. It returns an empty string when nothing is stored.
	Cursor func(ctx context.Context) (string, error)
	// SaveCursor persists provider progress after a durable receipt.
	SaveCursor func(ctx context.Context, cursor string) error
	Egress      Egress
}

// DeliveryError separates a definite rejection from an uncertain completion.
// An uncertain completion means the provider may already have accepted the
// message, so a retry can duplicate it.
type DeliveryError struct {
	Uncertain bool
	Message   string
}

func (e *DeliveryError) Error() string { return e.Message }

// Definite reports a failure that will not change by repeating the request.
func Definite(format string, args ...any) error {
	return &DeliveryError{Message: fmt.Sprintf(format, args...)}
}

// Uncertain reports a failure where the provider may already have accepted the
// message.
func Uncertain(format string, args ...any) error {
	return &DeliveryError{Uncertain: true, Message: fmt.Sprintf(format, args...)}
}

// IsUncertain reports whether err describes an uncertain completion.
func IsUncertain(err error) bool {
	var e *DeliveryError
	if errors.As(err, &e) {
		return e.Uncertain
	}
	return false
}

// WebhookConflictError reports that the provider already has a webhook
// configured. The relay never deletes an existing webhook: removing it is an
// explicit operator action outside Meshra.
type WebhookConflictError struct{ Message string }

func (e *WebhookConflictError) Error() string { return e.Message }

// WebhookConflict reports an existing provider webhook.
func WebhookConflict(format string, args ...any) error {
	return &WebhookConflictError{Message: fmt.Sprintf(format, args...)}
}

// IsWebhookConflict reports whether err describes an existing provider webhook.
func IsWebhookConflict(err error) bool {
	var e *WebhookConflictError
	return errors.As(err, &e)
}

// Origins returns the exact provider origins one connector needs. The list is
// a provider constant, never operator supplied text.
func Origins(kind Kind) []string {
	switch kind {
	case KindSlack:
		return []string{SlackOrigin, SlackFilesOrigin}
	case KindDiscord:
		return []string{DiscordOrigin, "https://media.discordapp.net", "https://cdn.discordapp.net"}
	case KindTelegram:
		return []string{TelegramOrigin}
	}
	return nil
}

// SocketHosts returns the websocket peers one connector may dial.
func SocketHosts(kind Kind) []string {
	switch kind {
	case KindSlack:
		return []string{"wss-primary.slack.com:443", "wss-standby.slack.com:443"}
	case KindDiscord:
		return []string{"gateway.discord.gg:443"}
	}
	return nil
}

// MessageLimit returns the reply size limit of one provider.
func MessageLimit(kind Kind) int {
	switch kind {
	case KindSlack:
		return SlackMessageLimit
	case KindDiscord:
		return DiscordMessageLimit
	case KindTelegram:
		return TelegramMessageLimit
	}
	return 2000
}

// Scrub removes known secret values from provider error text. Provider errors
// frequently embed the request URL, which for Telegram carries the bot token.
func Scrub(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		if len(secret) >= 8 {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return errors.New(message)
}

// SplitText splits a reply into platform-sized pieces on rune boundaries,
// preferring a paragraph break and then a line break.
func SplitText(text string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	if len(text) <= limit {
		return []string{text}
	}
	pieces := []string{}
	for len(text) > limit {
		window := text[:limit]
		cut := strings.LastIndex(window, "\n\n")
		if cut < limit/3 {
			cut = strings.LastIndex(window, "\n")
		}
		if cut < limit/3 {
			cut = limit
			for cut > 0 && !utf8Boundary(text[cut]) {
				cut--
			}
			if cut == 0 {
				// A limit smaller than one rune still must make progress.
				cut = 1
				for cut < len(text) && !utf8Boundary(text[cut]) {
					cut++
				}
			}
		}
		piece := strings.TrimRight(text[:cut], "\n")
		if piece == "" {
			piece = text[:cut]
		}
		pieces = append(pieces, piece)
		text = strings.TrimLeft(text[cut:], "\n")
	}
	if text != "" {
		pieces = append(pieces, text)
	}
	return pieces
}

// clampText bounds text by bytes without splitting a rune.
func clampText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8Boundary(text[cut]) {
		cut--
	}
	return text[:cut]
}

// utf8Boundary reports whether the byte at index i starts a rune.
func utf8Boundary(b byte) bool { return b&0xC0 != 0x80 }

// providerHTTP keeps rate-limit delays across API and file requests without
// replaying any POST. The egress client remains responsible for pinned DNS and
// live grants; this extra boundary prevents SDK redirects or foreign URLs from
// forwarding credentials even when a fixture supplies a general HTTP client.
type providerHTTP struct {
	egress Egress
	mu sync.Mutex
	notBefore time.Time
}

func (p *providerHTTP) client(ctx context.Context, origin string) (*http.Client, error) {
	client, err := p.egress.Client(ctx, origin)
	if err != nil { return nil, err }
	if client == nil || client.Transport == nil { return nil, errors.New("connector requires an explicit egress transport") }
	return &http.Client{Transport: &providerRoundTripper{provider: p, base: client.Transport, origin: origin}, Timeout: client.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("connector redirects are not permitted") }}, nil
}

type providerRoundTripper struct {
	provider *providerHTTP
	base http.RoundTripper
	origin string
}

func permittedURL(raw, origin string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.User == nil && parsed.Fragment == "" && parsed.Scheme+"://"+parsed.Host == origin
}

func (t *providerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if !permittedURL(request.URL.String(), t.origin) || request.Host != "" && request.Host != request.URL.Host {
		return nil, errors.New("connector request origin is not permitted")
	}
	for {
		t.provider.mu.Lock()
		delay := time.Until(t.provider.notBefore)
		t.provider.mu.Unlock()
		if delay <= 0 { break }
		if err := waitContext(request.Context(), delay); err != nil { return nil, err }
	}
	// Disable net/http's body replay path as well as SDK-level retries.
	request = request.Clone(request.Context())
	request.GetBody = nil
	response, err := t.base.RoundTrip(request)
	if err != nil { return nil, errors.New("connector provider request failed") }
	if response.StatusCode == http.StatusTooManyRequests {
		delay := time.Second
		if seconds, err := strconv.ParseFloat(response.Header.Get("Retry-After"), 64); err == nil && seconds > 0 {
			delay = time.Duration(seconds * float64(time.Second))
		} else if until, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil && time.Until(until) > 0 {
			delay = time.Until(until)
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if readErr == nil {
			var body struct {
				RetryAfter float64 `json:"retry_after"`
				Parameters struct { RetryAfter float64 `json:"retry_after"` } `json:"parameters"`
			}
			if json.Unmarshal(raw, &body) == nil {
				for _, seconds := range []float64{body.RetryAfter, body.Parameters.RetryAfter} {
					if candidate := time.Duration(seconds * float64(time.Second)); candidate > delay { delay = candidate }
				}
			}
		}
		response.Body = &prefixedBody{Reader: io.MultiReader(bytes.NewReader(raw), response.Body), Closer: response.Body}
		t.provider.mu.Lock()
		if until := time.Now().Add(delay); until.After(t.provider.notBefore) { t.provider.notBefore = until }
		t.provider.mu.Unlock()
	}
	return response, nil
}

type prefixedBody struct { io.Reader; io.Closer }

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done(): return ctx.Err()
	case <-timer.C: return nil
	}
}

// socketDialer refuses ws://, environment proxies and unapproved peers. TLS
// authentication stays bound to the provider hostname, never the pinned IP.
func socketDialer(egress Egress, hosts []string) *websocket.Dialer {
	dialer := &websocket.Dialer{HandshakeTimeout: 15*time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	dialer.NetDialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connector websocket requires TLS")
	}
	dialer.NetDialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		allowed := false
		for _, host := range hosts { if address == host { allowed = true; break } }
		if network != "tcp" || !allowed { return nil, errors.New("connector websocket destination is not permitted") }
		conn, err := egress.Dial(ctx, address)
		if err != nil { return nil, err }
		host, _, err := net.SplitHostPort(address)
		if err != nil { conn.Close(); return nil, errors.New("invalid connector websocket peer") }
		config := dialer.TLSClientConfig.Clone()
		config.ServerName = host
		tlsConn := tls.Client(conn, config)
		if err = tlsConn.HandshakeContext(ctx); err != nil { conn.Close(); return nil, errors.New("connector websocket TLS authentication failed") }
		return tlsConn, nil
	}
	return dialer
}
