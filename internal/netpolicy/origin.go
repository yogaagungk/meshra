// Package netpolicy enforces operator-reviewed, exact-origin outbound access.
// It is not a network namespace: workers must additionally have no host route.
package netpolicy

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var ErrDenied = errors.New("network destination is not authorized")
var ErrRevoked = errors.New("network authorization is no longer active")
var ErrLimit = errors.New("network response or stream limit exceeded")

// Origin is a canonical HTTP origin. Its fields are immutable after parsing.
type Origin struct {
	Scheme string
	Host string
	Port string
}

func (o Origin) String() string {
	host := o.Host
	if strings.Contains(host, ":") { host = "[" + host + "]" }
	if (o.Scheme == "http" && o.Port != "80") || (o.Scheme == "https" && o.Port != "443") { host = net.JoinHostPort(o.Host, o.Port) }
	return o.Scheme + "://" + host
}
func (o Origin) Address() string { return net.JoinHostPort(o.Host, o.Port) }

// ParseOrigin rejects URL components that could hide additional authority.
func ParseOrigin(raw string) (Origin, error) {
	u, err := url.Parse(raw)
	if err != nil { return Origin{}, fmt.Errorf("invalid origin: %w", err) }
	if u.Path != "" && u.Path != "/" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") { return Origin{}, errors.New("origin must not include a path, query or fragment") }
	return originURL(u)
}

func originURL(u *url.URL) (Origin, error) {
	if u == nil || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.Host == "" { return Origin{}, errors.New("absolute origin without credentials required") }
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" { return Origin{}, errors.New("only http and https origins are supported") }
	host := strings.ToLower(u.Hostname())
	if strings.ContainsAny(host, "%\\/*@\x00 \t\r\n") || host == "" { return Origin{}, errors.New("invalid origin hostname") }
	if ip, err := netip.ParseAddr(host); err == nil { host = ip.Unmap().String() } else {
		host = strings.TrimSuffix(host, ".")
		if len(host) > 253 { return Origin{}, errors.New("origin hostname is too long") }
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' { return Origin{}, errors.New("invalid origin hostname") }
			for _, c := range label { if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') { return Origin{}, errors.New("origin hostname must be an ASCII DNS name or IP address") } }
		}
	}
	port := u.Port()
	if port == "" { if strings.HasSuffix(u.Host, ":") { return Origin{}, errors.New("empty origin port") }; if scheme == "https" { port = "443" } else { port = "80" } }
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 { return Origin{}, errors.New("invalid origin port") }
	return Origin{Scheme: scheme, Host: host, Port: strconv.Itoa(n)}, nil
}

func restricted(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() { return true }
	for _, prefix := range restrictedPrefixes { if prefix.Contains(ip) { return true } }
	return false
}

var restrictedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}
