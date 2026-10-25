package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sync"
	"time"
)

// Scope is supplied by the daemon, never taken from worker request headers.
// Grants match both selectors exactly; an empty selector is not a wildcard.
type Scope struct {
	ProfileID    string `json:"profileId,omitempty"`
	ConnectionID string `json:"connectionId,omitempty"`
}

type Grant struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	Scope
	Origin       string `json:"origin"`
	AllowPrivate bool   `json:"allowPrivate"`
	Enabled      bool   `json:"enabled"`
}

// Lookup must read current persisted authorization on each call. It is invoked
// without the policy mutex held, including immediately before network connect.
type Lookup func(context.Context, Scope, string) (Grant, error)
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type Options struct {
	Resolver       Resolver
	ConnectTimeout time.Duration
}

type activeStream struct {
	grant  Grant
	cancel context.CancelFunc
	conn   net.Conn
}
type Policy struct {
	mu               sync.Mutex
	lookup           Lookup
	resolver         Resolver
	connectTimeout   time.Duration
	listener         *Origin
	listenerRevision uint64
	blocked          map[string]int
	streams          map[*activeStream]struct{}
	closed           bool
}

func New(lookup Lookup, options Options) *Policy {
	if options.Resolver == nil {
		options.Resolver = net.DefaultResolver
	}
	if options.ConnectTimeout <= 0 || options.ConnectTimeout > 30*time.Second {
		options.ConnectTimeout = 10 * time.Second
	}
	return &Policy{lookup: lookup, resolver: options.Resolver, connectTimeout: options.ConnectTimeout, blocked: make(map[string]int), streams: make(map[*activeStream]struct{})}
}

// SetListener must be called before use. All streams are closed on a listener
// change; callers cannot retain access based on an obsolete self-listener check.
func (p *Policy) SetListener(raw string) error {
	u, err := url.Parse(raw)
	var origin Origin
	if err == nil {
		origin, err = originURL(u)
	}
	p.mu.Lock()
	p.listenerRevision++
	if err != nil {
		p.listener = nil
	} else {
		p.listener = &origin
	}
	p.closeStreamsLocked("", 0)
	p.mu.Unlock()
	return err
}

func (p *Policy) Revoke(id string, revision int) {
	p.mu.Lock()
	if revision > p.blocked[id] {
		p.blocked[id] = revision
	}
	p.closeStreamsLocked(id, revision)
	p.mu.Unlock()
}
func (p *Policy) closeStreamsLocked(id string, revision int) {
	for s := range p.streams {
		if id == "" || s.grant.ID == id && s.grant.Revision <= revision {
			s.cancel()
			if s.conn != nil {
				_ = s.conn.Close()
			}
		}
	}
}
func (p *Policy) Close() error {
	p.mu.Lock()
	p.closed = true
	p.closeStreamsLocked("", 0)
	p.mu.Unlock()
	return nil
}

func (p *Policy) authorize(ctx context.Context, scope Scope, origin Origin, active func(context.Context) error) (Grant, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	if active != nil {
		if err := active(ctx); err != nil {
			return Grant{}, err
		}
	}
	if p.lookup == nil || scope.ProfileID == "" && scope.ConnectionID == "" {
		return Grant{}, ErrDenied
	}
	g, err := p.lookup(ctx, scope, origin.String())
	if err != nil {
		return Grant{}, err
	}
	if !g.Enabled || g.ID == "" || g.Revision < 1 || g.Scope != scope || g.Origin != origin.String() {
		return Grant{}, ErrDenied
	}
	p.mu.Lock()
	denied := p.closed || p.listener == nil || p.blocked[g.ID] >= g.Revision
	p.mu.Unlock()
	if denied {
		return Grant{}, ErrRevoked
	}
	return g, nil
}

func (p *Policy) addresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	addresses, err := p.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 || len(addresses) > 64 {
		return nil, ErrDenied
	}
	for i := range addresses {
		if !addresses[i].IsValid() || addresses[i].Zone() != "" {
			return nil, ErrDenied
		}
		addresses[i] = addresses[i].Unmap()
	}
	return addresses, nil
}

func (p *Policy) checkAddresses(ctx context.Context, origin Origin, g Grant) ([]netip.Addr, error) {
	ips, err := p.addresses(ctx, origin.Host)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	var listener Origin
	if p.listener != nil {
		listener = *p.listener
	}
	p.mu.Unlock()
	if listener.Host == "" {
		return nil, ErrDenied
	}
	var self []netip.Addr
	if origin.Port == listener.Port {
		self, err = p.addresses(ctx, listener.Host)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve relay listener: %w", err)
		}
		// Wildcard listeners cover every interface. A loopback alias at the
		// listener port is forbidden even when its address family differs.
		for _, ip := range self {
			if ip.IsUnspecified() {
				interfaces, e := net.InterfaceAddrs()
				if e != nil {
					return nil, e
				}
				for _, addr := range interfaces {
					prefix, e := netip.ParsePrefix(addr.String())
					if e == nil {
						self = append(self, prefix.Addr().Unmap())
					}
				}
			}
		}
	}
	for _, ip := range ips {
		if ip.IsUnspecified() || ip.IsMulticast() || !g.AllowPrivate && restricted(ip) {
			return nil, ErrDenied
		}
		if origin.Port == listener.Port {
			if ip.IsLoopback() {
				return nil, ErrDenied
			}
			for _, forbidden := range self {
				if ip == forbidden {
					return nil, ErrDenied
				}
			}
		}
	}
	return ips, nil
}

// ValidateGrant checks current DNS and the self-listener boundary without opening
// a connection. The same checks are repeated at every eventual connection.
func (p *Policy) ValidateGrant(ctx context.Context, raw string, allowPrivate bool) (string, error) {
	o, err := ParseOrigin(raw)
	if err != nil {
		return "", err
	}
	if _, err = p.checkAddresses(ctx, o, Grant{AllowPrivate: allowPrivate}); err != nil {
		return "", err
	}
	return o.String(), nil
}

// Dial opens a tracked, DNS-pinned TCP stream to an exact reviewed HTTP(S)
// origin. The caller owns protocol framing and TLS with that origin's hostname.
// Grant revocation and caller cancellation close the stream.
func (p *Policy) Dial(ctx context.Context, scope Scope, rawOrigin string, active func(context.Context) error) (net.Conn, error) {
	origin, err := ParseOrigin(rawOrigin)
	if err != nil {
		return nil, err
	}
	if active == nil {
		return nil, errors.New("network stream requires a live owner")
	}
	return p.dial(ctx, scope, origin, active)
}

func (p *Policy) dial(ctx context.Context, scope Scope, origin Origin, active func(context.Context) error) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	connectCtx, connectCancel := context.WithTimeout(ctx, p.connectTimeout)
	defer connectCancel()
	g, err := p.authorize(connectCtx, scope, origin, active)
	if err != nil {
		cancel()
		return nil, err
	}
	p.mu.Lock()
	listenerRevision := p.listenerRevision
	p.mu.Unlock()
	ips, err := p.checkAddresses(connectCtx, origin, g)
	if err != nil {
		cancel()
		return nil, err
	}
	// DNS lookup may have waited. Reread grant and active attempt before dispatch.
	latest, err := p.authorize(connectCtx, scope, origin, active)
	if err != nil || latest.ID != g.ID || latest.Revision != g.Revision {
		cancel()
		if err != nil {
			return nil, err
		}
		return nil, ErrRevoked
	}
	stream := &activeStream{grant: g, cancel: cancel}
	p.mu.Lock()
	if p.closed || p.blocked[g.ID] >= g.Revision || p.listenerRevision != listenerRevision {
		p.mu.Unlock()
		cancel()
		return nil, ErrRevoked
	}
	if len(p.streams) >= 128 {
		p.mu.Unlock()
		cancel()
		return nil, ErrLimit
	}
	p.streams[stream] = struct{}{}
	p.mu.Unlock()
	conn, err := (&net.Dialer{}).DialContext(connectCtx, "tcp", net.JoinHostPort(ips[0].String(), origin.Port))
	if err != nil {
		p.release(stream)
		return nil, err
	}
	p.mu.Lock()
	if ctx.Err() != nil || p.closed || p.blocked[g.ID] >= g.Revision || p.listenerRevision != listenerRevision {
		p.mu.Unlock()
		_ = conn.Close()
		p.release(stream)
		return nil, ErrRevoked
	}
	stream.conn = conn
	p.mu.Unlock()
	// The dial deadline must not become a stream lifetime timer. Cancellation of
	// the caller still closes the stream, and revoke explicitly closes it too.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return &trackedConn{Conn: conn, policy: p, stream: stream, stop: stop}, nil
}

func (p *Policy) release(s *activeStream) {
	p.mu.Lock()
	delete(p.streams, s)
	p.mu.Unlock()
	s.cancel()
}

type trackedConn struct {
	net.Conn
	policy *Policy
	stream *activeStream
	stop   func() bool
	once   sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.stop(); c.policy.release(c.stream) })
	return err
}

func credentialBinding(origin Origin, binding string) error {
	if binding == "" {
		return nil
	}
	bound, err := ParseOrigin(binding)
	if err != nil || bound != origin {
		return errors.New("credentials must be bound to the exact request origin")
	}
	return nil
}
