package relay

import (
	"context"
	"net/http"
	"net/url"

	"meshra/internal/externalmcp"
	"meshra/internal/model"
)

// Discovery may visit a separate, explicitly reviewed authorization origin.
// Every individual request still uses an exact netpolicy origin-bound client.
type mcpOAuthTransport struct {
	relay *Relay
	config externalmcp.Config
}

func (t *mcpOAuthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	origin, err := externalmcp.OriginOf(request.URL.Scheme + "://" + request.URL.Host)
	if err != nil { return nil, model.Err(403, "invalid OAuth discovery origin") }
	client, err := t.relay.mcpHTTPClient(request.Context(), t.config, origin)
	if err != nil { return nil, err }
	if client.Transport == nil { return nil, model.Err(503, "OAuth policy transport is unavailable") }
	return client.Transport.RoundTrip(request)
}

// MCPAuthCallbackAt checks the actual callback origin before using a code.
// Only the exact recorded loopback scheme, host, port and path are accepted.
func (r *Relay) MCPAuthCallbackAt(ctx context.Context, callbackURI, code, state string) error {
	parsed, err := url.Parse(callbackURI)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return model.Err(400, "invalid OAuth callback URI")
	}
	r.mu.Lock()
	var expected string
	err = r.db.QueryRowContext(ctx, "SELECT json_extract(body,'$.redirectUri') FROM mcp_auth_flows WHERE state_hash=?", tokenHash(state)).Scan(&expected)
	r.mu.Unlock()
	if err != nil || callbackURI != expected {
		return model.Err(403, "OAuth callback URI does not match the initiating flow")
	}
	return r.MCPAuthCallback(ctx, code, state)
}

// Caller holds relay ownership; the revoked-session flag already denies every
// callback persistence point even if database cleanup itself fails.
func (r *Relay) revokeMCPAuthFlowsLocked(owner string) error {
	if r.closed {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM automation_secrets WHERE id IN (SELECT json_extract(body,'$.verifierSecretId') FROM mcp_auth_flows WHERE session_owner=?)", owner); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM mcp_auth_flows WHERE session_owner=?", owner); err != nil {
		return err
	}
	return tx.Commit()
}
