package api

import (
	"context"
	"net/http"
	"strings"

	"meshra/internal/model"
)

// MCPOAuthService completes one external MCP authorization flow. The provider
// redirects here with a code and a one-use state; the state identifies the
// console session that started the flow.
type MCPOAuthService interface {
	MCPAuthCallbackAt(ctx context.Context, callbackURI, code, state string) error
}

// mcpOAuthCallback is the single exempt console route. It runs before the
// cookie and CSRF gates because the provider returns without a session
// cookie, and it never requires one.
func (s *server) mcpOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w, "GET")
		return
	}
	if !loopbackHost(r.Host) {
		fail(w, model.Err(403, "OAuth callback requires a loopback host"))
		return
	}
	svc, ok := s.service.(MCPOAuthService)
	if !ok {
		fail(w, model.Err(404, "endpoint not found"))
		return
	}
	query := r.URL.Query()
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	if code == "" || state == "" || len(code) > 8192 || len(state) > 256 || len(query["code"]) != 1 || len(query["state"]) != 1 {
		fail(w, model.Err(400, "the authorization response is incomplete or ambiguous"))
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if err := svc.MCPAuthCallbackAt(r.Context(), scheme+"://"+r.Host+r.URL.Path, code, state); err != nil {
		fail(w, err)
		return
	}
	// The code and state are never retained in the redirect target.
	http.Redirect(w, r, "/console/", http.StatusSeeOther)
}
