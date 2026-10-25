// Package externalmcp implements a separately managed client for third-party
// MCP servers. It never replaces the Meshra /mcp server: the existing server
// keeps its own credential model and allowlists, and every external server is
// an operator-reviewed configuration with its own binding, revision and
// approval rules.
package externalmcp

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Transport selects how the daemon reaches an external MCP server.
type Transport string

const (
	// TransportStdio launches the server as a child process. Launching is code
	// execution and therefore always happens in the confined worker.
	TransportStdio Transport = "stdio"
	// TransportHTTP reaches a Streamable HTTP endpoint over an exact origin.
	TransportHTTP Transport = "http"
)

// Config is the operator-reviewed description of one external MCP server.
// Secret values are never part of it: only references to stored secrets are.
type Config struct {
	ID               string    `json:"id"`
	Revision         int       `json:"revision"`
	Name             string    `json:"name"`
	Transport        Transport `json:"transport"`
	URL              string    `json:"url,omitempty"`
	Executable       string    `json:"executable,omitempty"`
	Args             []string  `json:"args,omitempty"`
	SecretHeaderRefs []string  `json:"secretHeaderRefs,omitempty"`
	SecretEnvRefs    []string  `json:"secretEnvRefs,omitempty"`
	WorkspaceID      string    `json:"workspaceId"`
	AllowedOrigins   []string  `json:"allowedOrigins,omitempty"`
	// AllowPrivate records the operator's explicit confirmation that private,
	// loopback or LAN origins may be reached. It never widens the origin set.
	AllowPrivate bool `json:"allowPrivate,omitempty"`
	Enabled      bool `json:"enabled"`
}

// MaxServerName bounds the operator-visible server name.
const MaxServerName = 128

// MaxArgs bounds the argument vector of a stdio server.
const MaxArgs = 64

// MaxSecretRefs bounds the number of referenced secrets per server.
const MaxSecretRefs = 32

// MaxAllowedOrigins bounds the reviewed origin list of one server.
const MaxAllowedOrigins = 32

// ErrUnavailable reports a missing dependency rather than a bad request.
var ErrUnavailable = errors.New("external MCP dependency unavailable")

// Validate rejects a configuration that could widen authority, hide a shell
// command string or reach an origin the operator did not review.
func (c Config) Validate() error {
	if c.ID == "" || len(c.ID) > 200 || !SafeText(c.ID) {
		return errors.New("server id is required and must be plain text")
	}
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > MaxServerName || !utf8.ValidString(c.Name) || !SafeText(c.Name) {
		return errors.New("server name must be 1..128 bytes of plain text")
	}
	if c.Revision < 0 {
		return errors.New("revision must not be negative")
	}
	if strings.TrimSpace(c.WorkspaceID) == "" || len(c.WorkspaceID) > 200 || !SafeText(c.WorkspaceID) {
		return errors.New("workspaceId is required and must be plain text")
	}
	if len(c.SecretHeaderRefs) > MaxSecretRefs || len(c.SecretEnvRefs) > MaxSecretRefs {
		return fmt.Errorf("at most %d secret references per server", MaxSecretRefs)
	}
	for _, refs := range [][]string{c.SecretHeaderRefs, c.SecretEnvRefs} {
		for _, ref := range refs {
			if !SecretName(ref) {
				return errors.New("secret references must be plain identifiers")
			}
		}
	}
	if len(c.AllowedOrigins) > MaxAllowedOrigins {
		return fmt.Errorf("at most %d allowed origins per server", MaxAllowedOrigins)
	}
	switch c.Transport {
	case TransportStdio:
		if c.URL != "" {
			return errors.New("a stdio server must not carry a URL")
		}
		if err := c.validateExecutable(); err != nil {
			return err
		}
	case TransportHTTP:
		if c.Executable != "" || len(c.Args) > 0 {
			return errors.New("an http server must not carry an executable")
		}
		if _, err := c.EndpointOrigin(); err != nil {
			return err
		}
	default:
		return errors.New("transport must be stdio or http")
	}
	return nil
}

// validateExecutable requires an existing absolute regular executable. A
// relative name would resolve through PATH and a shell would interpret it.
func (c Config) validateExecutable() error {
	if c.Executable == "" || !filepath.IsAbs(c.Executable) || filepath.Clean(c.Executable) != c.Executable {
		return errors.New("executable must be an absolute path")
	}
	if len(c.Args) > MaxArgs {
		return fmt.Errorf("at most %d arguments are allowed", MaxArgs)
	}
	for _, arg := range c.Args {
		if len(arg) > 4096 || !utf8.ValidString(arg) || strings.ContainsAny(arg, "\x00\n\r") {
			return errors.New("arguments must be plain single-line strings")
		}
	}
	// A shell interpreter with a command argument is a shell command string.
	// The daemon never runs a shell, so such a configuration is refused.
	if IsShell(c.Executable) {
		for _, arg := range c.Args {
			if arg == "-c" || arg == "-lc" {
				return errors.New("shell command strings are not accepted; use a direct executable")
			}
		}
	}
	info, err := os.Stat(c.Executable)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: configured executable does not exist", ErrUnavailable)
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("executable must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return errors.New("executable is not executable")
	}
	return nil
}

// EndpointOrigin returns the exact scheme/host/port origin of an HTTP server.
func (c Config) EndpointOrigin() (string, error) {
	if c.Transport != TransportHTTP {
		return "", errors.New("origin is only defined for http servers")
	}
	return OriginOf(c.URL)
}

// OriginOf canonicalizes an endpoint URL to its exact origin.
func OriginOf(raw string) (string, error) {
	if raw == "" || len(raw) > 2048 {
		return "", errors.New("url must be 1..2048 bytes")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("url is not a valid absolute URL")
	}
	if parsed.User != nil {
		return "", errors.New("url must not contain credentials")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", errors.New("url must not contain a fragment")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errors.New("url must not contain a query")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("url scheme must be http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.ContainsAny(host, "%\\/*@ \t\r\n") {
		return "", errors.New("url host is invalid")
	}
	port := parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if len(port) > 5 {
		return "", errors.New("url port is invalid")
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return "", errors.New("url port is invalid")
		}
	}
	origin := scheme + "://" + host
	if (scheme == "http" && port != "80") || (scheme == "https" && port != "443") {
		origin += ":" + port
	}
	return origin, nil
}

// AllowedOrigin reports whether the exact origin was reviewed for this server.
func (c Config) AllowedOrigin(origin string) bool {
	for _, allowed := range c.AllowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

// SafeText reports whether a value is plain, single-line, printable text.
func SafeText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' {
			return false
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// SecretName reports whether a secret reference is a plain identifier.
func SecretName(value string) bool {
	if value == "" || len(value) > 200 || !SafeText(value) {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

// IsShell reports whether an executable is a shell interpreter.
func IsShell(path string) bool {
	switch filepath.Base(path) {
	case "sh", "bash", "dash", "zsh", "fish", "ksh", "csh", "tcsh":
		return true
	}
	return false
}
