package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"meshra/internal/externalmcp"
	"meshra/internal/model"
	"meshra/internal/netpolicy"
	"meshra/internal/sandbox"
)

// The external MCP client is a separately managed surface. It never replaces
// the Meshra /mcp server: every external server is an operator-reviewed
// configuration with its own revision, binding, approval and secret rules.

const (
	// mcpSecretKindToken stores an OAuth token set of one external server.
	mcpSecretKindToken = "mcp.oauth"
	// mcpSecretKindVerifier stores a PKCE verifier of one pending flow.
	mcpSecretKindVerifier = "mcp.pkce"
	// mcpFlowLifetime bounds one pending authorization flow.
	mcpFlowLifetime = 10 * time.Minute
	// mcpSessionTimeout bounds one external MCP session.
	mcpSessionTimeout = 60 * time.Second
	// mcpMaxArguments bounds the arguments of one external tool call.
	mcpMaxArguments = 64 << 10
)

// mcpToolBinding is one reviewed tool of one binding.
type mcpToolBinding struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// mcpBindingView describes one profile binding of an external server.
type mcpBindingView struct {
	ProfileID string           `json:"profileId"`
	ServerID  string           `json:"serverId"`
	Revision  int              `json:"revision"`
	Enabled   bool             `json:"enabled"`
	Tools     []mcpToolBinding `json:"tools"`
}

// mcpServerView is the operator-facing description of one external server.
// It never contains a secret value, a token or an authorization code.
type mcpServerView struct {
	ID               string             `json:"id"`
	Revision         int                `json:"revision"`
	Name             string             `json:"name"`
	Transport        string             `json:"transport"`
	URL              string             `json:"url,omitempty"`
	Executable       string             `json:"executable,omitempty"`
	Args             []string           `json:"args,omitempty"`
	SecretHeaderRefs []string           `json:"secretHeaderRefs,omitempty"`
	SecretEnvRefs    []string           `json:"secretEnvRefs,omitempty"`
	SecretStatus     map[string]string  `json:"secretStatus,omitempty"`
	WorkspaceID      string             `json:"workspaceId"`
	AllowedOrigins   []string           `json:"allowedOrigins,omitempty"`
	AllowPrivate     bool               `json:"allowPrivate,omitempty"`
	Enabled          bool               `json:"enabled"`
	Auth             string             `json:"auth"`
	AuthExpiresAt    string             `json:"authExpiresAt,omitempty"`
	Tools            []externalmcp.Tool `json:"tools,omitempty"`
	Bindings         []mcpBindingView   `json:"bindings,omitempty"`
}

// mcpAuthFlow is one pending authorization flow.
type mcpAuthFlow struct {
	ID           string `json:"id"`
	ServerID     string `json:"serverId"`
	SessionOwner string `json:"sessionOwner"`
	StateHash    string `json:"stateHash"`
	VerifierID   string `json:"verifierSecretId"`
	RedirectURI  string `json:"redirectUri"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientId"`
	ExpiresAt    string `json:"expiresAt"`
	Consumed     bool   `json:"consumed"`
	ServerRevision int `json:"serverRevision"`
}

type mcpPendingAuthorization struct {
	Verifier string `json:"verifier"`
	Discovery externalmcp.OAuthDiscovery `json:"discovery"`
}
// mcpClientOperation implements the mcp.* automation operations. Only the
// operator may configure, test, enable, bind or authenticate an external
// server; agents and connectors never reach these operations.
func (r *Relay) mcpClientOperation(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) || len(raw) > 64<<10 {
		return nil, model.Err(400, "invalid MCP payload")
	}
	switch operation {
	case "mcp.list":
		return r.mcpList(ctx, raw)
	case "mcp.save":
		return r.mcpSave(ctx, raw)
	case "mcp.test":
		return r.mcpTest(ctx, raw)
	case "mcp.setEnabled":
		return r.mcpSetEnabled(ctx, raw)
	case "mcp.bind":
		return r.mcpBind(ctx, raw)
	case "mcp.remove":
		return r.mcpRemove(ctx, raw)
	case "mcp.authStart":
		return r.mcpAuthStart(ctx, p, raw)
	case "mcp.authStatus":
		return r.mcpAuthStatus(ctx, raw)
	case "mcp.signOut":
		return r.mcpSignOut(ctx, raw)
	}
	return nil, model.Err(404, "unknown MCP operation")
}

// mcpServer loads one stored external server configuration.
func (r *Relay) mcpServer(id string) (externalmcp.Config, error) {
	var body string
	err := r.db.QueryRow("SELECT body FROM external_mcp_servers WHERE id=?", id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return externalmcp.Config{}, model.Err(404, "MCP server not found")
	}
	if err != nil {
		return externalmcp.Config{}, err
	}
	var config externalmcp.Config
	if err = json.Unmarshal([]byte(body), &config); err != nil {
		return externalmcp.Config{}, err
	}
	return config, nil
}

// mcpToolRows returns the last discovered tool definitions of one server.
func (r *Relay) mcpToolRows(serverID string) ([]externalmcp.Tool, error) {
	rows, err := r.db.Query("SELECT name,digest,body FROM external_mcp_tools WHERE server_id=? ORDER BY name", serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tools := []externalmcp.Tool{}
	for rows.Next() {
		var name, digest, body string
		if err = rows.Scan(&name, &digest, &body); err != nil {
			return nil, err
		}
		var tool externalmcp.Tool
		if err = json.Unmarshal([]byte(body), &tool); err != nil {
			return nil, err
		}
		tool.Name, tool.Digest = name, digest
		tools = append(tools, tool)
	}
	return tools, rows.Err()
}

// mcpBinding loads one profile binding of one external server.
func (r *Relay) mcpBinding(profileID, serverID string) (mcpBindingView, error) {
	var binding mcpBindingView
	var tools string
	var enabled bool
	err := r.db.QueryRow("SELECT profile_id,server_id,revision,tool_names,enabled FROM profile_mcp_bindings WHERE profile_id=? AND server_id=?", profileID, serverID).Scan(&binding.ProfileID, &binding.ServerID, &binding.Revision, &tools, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return binding, model.Err(404, "MCP binding not found")
	}
	if err != nil {
		return binding, err
	}
	binding.Enabled = enabled
	if err = json.Unmarshal([]byte(tools), &binding.Tools); err != nil {
		return binding, err
	}
	return binding, nil
}

// mcpList returns every configured external server without any secret value.
func (r *Relay) mcpList(ctx context.Context, raw json.RawMessage) (any, error) {
	var query struct {
		After string `json:"after"`
		Limit int    `json:"limit"`
	}
	if err := decodeAutomation(raw, &query); err != nil {
		return nil, err
	}
	if query.After != "" && !profileIdentifier(query.After) {
		return nil, model.Err(400, "invalid MCP cursor")
	}
	limit := query.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, model.Err(400, "limit must be 1..100")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	rows, err := r.db.QueryContext(ctx, "SELECT id FROM external_mcp_servers WHERE (?='' OR id>?) ORDER BY id LIMIT ?", query.After, query.After, limit+1)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	more := len(ids) > limit
	if more {
		ids = ids[:limit]
	}
	items := make([]mcpServerView, 0, len(ids))
	for _, id := range ids {
		view, err := r.mcpServerView(ctx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, view)
	}
	page := model.AutomationPage[mcpServerView]{Items: items, HasMore: more}
	if more && len(ids) > 0 {
		page.NextAfter = ids[len(ids)-1]
	}
	return page, nil
}

// mcpServerView builds the operator-facing description of one server.
func (r *Relay) mcpServerView(ctx context.Context, id string) (mcpServerView, error) {
	config, err := r.mcpServer(id)
	if err != nil {
		return mcpServerView{}, err
	}
	view := mcpServerView{
		ID:               config.ID,
		Revision:         config.Revision,
		Name:             config.Name,
		Transport:        string(config.Transport),
		URL:              config.URL,
		Executable:       config.Executable,
		Args:             config.Args,
		SecretHeaderRefs: config.SecretHeaderRefs,
		SecretEnvRefs:    config.SecretEnvRefs,
		WorkspaceID:      config.WorkspaceID,
		AllowedOrigins:   config.AllowedOrigins,
		AllowPrivate:     config.AllowPrivate,
		Enabled:          config.Enabled,
		Auth:             "signed_out",
	}
	status := map[string]string{}
	for _, ref := range append(append([]string{}, config.SecretHeaderRefs...), config.SecretEnvRefs...) {
		state, err := r.secretStatus(ctx, ref)
		if err != nil {
			state = "missing"
		}
		status[ref] = state
	}
	if len(status) > 0 {
		view.SecretStatus = status
	}
	tools, err := r.mcpToolRows(id)
	if err != nil {
		return view, err
	}
	view.Tools = tools
	rows, err := r.db.Query("SELECT profile_id,server_id,revision,tool_names,enabled FROM profile_mcp_bindings WHERE server_id=? ORDER BY profile_id", id)
	if err != nil {
		return view, err
	}
	bindings := []mcpBindingView{}
	for rows.Next() {
		var binding mcpBindingView
		var names string
		var enabled bool
		if err = rows.Scan(&binding.ProfileID, &binding.ServerID, &binding.Revision, &names, &enabled); err != nil {
			break
		}
		binding.Enabled = enabled
		if err = json.Unmarshal([]byte(names), &binding.Tools); err != nil {
			break
		}
		bindings = append(bindings, binding)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return view, err
	}
	view.Bindings = bindings
	auth, expires, err := r.mcpAuthState(ctx, id)
	if err != nil {
		return view, err
	}
	view.Auth = auth
	view.AuthExpiresAt = expires
	return view, nil
}

// mcpAuthState reports the authorization state of one server without tokens.
func (r *Relay) mcpAuthState(ctx context.Context, serverID string) (string, string, error) {
	var secretID, expires, scopes string
	err := r.db.QueryRowContext(ctx, "SELECT secret_id,expires_at,scopes FROM mcp_auth_tokens WHERE server_id=?", serverID).Scan(&secretID, &expires, &scopes)
	if errors.Is(err, sql.ErrNoRows) {
		return "signed_out", "", nil
	}
	if err != nil {
		return "signed_out", "", err
	}
	state, err := r.secretStatus(ctx, secretID)
	if err != nil || state != "saved" {
		return "sign_in_required", "", nil
	}
	deadline, parseErr := time.Parse(time.RFC3339Nano, expires)
	if parseErr != nil {
		return "signed_out", "", nil
	}
	if !deadline.After(now()) {
		return "stale", expires, nil
	}
	return "authorized", expires, nil
}

// mcpSave stores or updates one external server configuration.
func (r *Relay) mcpSave(ctx context.Context, raw json.RawMessage) (any, error) {
	var request struct {
		Server           externalmcp.Config `json:"server"`
		ExpectedRevision *int               `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &request); err != nil {
		return nil, err
	}
	config := request.Server
	if config.ID == "" {
		config.ID = randomID()
	}
	if config.Revision != 0 {
		return nil, model.Err(400, "revision is assigned by the daemon")
	}
	if config.Enabled {
		return nil, model.Err(400, "a new or changed server stays disabled until it is enabled explicitly")
	}
	if err := config.Validate(); err != nil {
		return nil, model.Err(400, err.Error())
	}
	origins, err := r.mcpReviewedOrigins(ctx, config)
	if err != nil {
		return nil, err
	}
	config.AllowedOrigins = origins
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	if _, err = r.workspace(config.WorkspaceID); err != nil {
		return nil, err
	}
	previous, err := r.mcpServer(config.ID)
	created := mcpNotFound(err)
	if err != nil && !created {
		return nil, err
	}
	if created {
		if request.ExpectedRevision != nil {
			return nil, model.Err(409, "MCP server revision changed")
		}
		config.Revision = 1
	} else {
		if request.ExpectedRevision == nil || *request.ExpectedRevision != previous.Revision {
			return nil, model.Err(409, "MCP server revision changed")
		}
		config.Revision = previous.Revision + 1
		// An edited server keeps its discovered tools only until the next
		// explicit test: the definitions must be reviewed again.
		if _, err = r.db.Exec("DELETE FROM external_mcp_tools WHERE server_id=?", config.ID); err != nil {
			return nil, err
		}
	}
	body, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if created {
		_, err = tx.Exec("INSERT INTO external_mcp_servers(id,revision,name,transport,body) VALUES(?,?,?,?,?)", config.ID, config.Revision, config.Name, string(config.Transport), string(body))
	} else {
		_, err = tx.Exec("UPDATE external_mcp_servers SET revision=?,name=?,transport=?,body=? WHERE id=? AND revision=?", config.Revision, config.Name, string(config.Transport), string(body), config.ID, previous.Revision)
	}
	if err != nil {
		return nil, err
	}
	revoked, err := invalidateConnectionNetworkLocked(ctx, tx, config.ID)
	if err != nil {
		return nil, err
	}
	if err = r.insertMCPGrants(ctx, tx, config); err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, "mcp.saved", config.ID, "", map[string]any{"revision": config.Revision, "transport": string(config.Transport), "enabled": config.Enabled}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.cancelExternalMCPLocked(config.ID, "")
	if !created {
		if err = r.mcpClearTokenLocked(ctx, config.ID); err != nil { return nil, err }
		if _, err = r.db.ExecContext(ctx, "DELETE FROM mcp_auth_flows WHERE server_id=?", config.ID); err != nil { return nil, err }
	}
	r.closeRevokedNetworkGrantsLocked(revoked)
	return r.mcpServerView(ctx, config.ID)
}

// mcpReviewedOrigins validates the operator's origin list against the network
// policy and returns the canonical origins.
func (r *Relay) mcpReviewedOrigins(ctx context.Context, config externalmcp.Config) ([]string, error) {
	origins := []string{}
	r.mu.Lock()
	policy := r.networkPolicyLocked()
	r.mu.Unlock()
	for _, raw := range config.AllowedOrigins {
		if len(raw) > 2048 {
			return nil, model.Err(400, "allowed origin is too long")
		}
		origin, err := netpolicy.ParseOrigin(raw)
		if err != nil {
			return nil, model.Err(400, "allowed origin is not an exact origin")
		}
		canonical := origin.String()
		validationCtx, cancel := context.WithTimeout(ctx, networkValidationTimeout)
		validated, err := policy.ValidateGrant(validationCtx, canonical, config.AllowPrivate)
		cancel()
		if err != nil {
			return nil, model.Err(400, "allowed origin is invalid, private without confirmation, self-listener, or DNS unavailable")
		}
		origins = append(origins, validated)
	}
	if config.Transport == externalmcp.TransportHTTP {
		endpoint, err := config.EndpointOrigin()
		if err != nil {
			return nil, model.Err(400, err.Error())
		}
		if !contains(origins, endpoint) {
			return nil, model.Err(400, "the server origin must be part of the reviewed origin list")
		}
	}
	return origins, nil
}

// insertMCPGrants writes the reviewed origins of one server as network grants.
func (r *Relay) insertMCPGrants(ctx context.Context, tx *sql.Tx, config externalmcp.Config) error {
	for _, origin := range config.AllowedOrigins {
		scope := netpolicy.Scope{ConnectionID: config.ID}
		previous, err := readNetworkGrant(ctx, tx, scope, origin)
		created := errors.Is(err, sql.ErrNoRows)
		if err != nil && !created {
			return err
		}
		grant := netpolicy.Grant{ID: previous.ID, Revision: previous.Revision + 1, Scope: scope, Origin: origin, AllowPrivate: config.AllowPrivate, Enabled: true}
		if created {
			grant.ID = randomID()
			grant.Revision = 1
		}
		body, err := json.Marshal(grant)
		if err != nil {
			return err
		}
		if created {
			_, err = tx.ExecContext(ctx, "INSERT INTO network_grants(id,revision,profile_id,connection_id,origin,enabled,body) VALUES(?,?,null,?,?,1,?)", grant.ID, grant.Revision, config.ID, origin, string(body))
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE network_grants SET revision=?,enabled=1,body=? WHERE id=? AND revision=?", grant.Revision, string(body), grant.ID, previous.Revision)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// mcpTest performs the operator's explicit test of one external server and
// records the tool definitions it advertises.
func (r *Relay) mcpTest(ctx context.Context, raw json.RawMessage) (any, error) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeAutomation(raw, &request); err != nil {
		return nil, err
	}
	if !profileIdentifier(request.ID) {
		return nil, model.Err(400, "MCP server id is required")
	}
	r.mu.Lock()
	config, err := r.mcpServer(request.ID)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	session, snapshot, err := r.mcpSession(ctx, config)
	if err != nil {
		return nil, err
	}
	defer func() {
		session.Close()
		if snapshot != nil {
			snapshot.Close()
		}
	}()
	tools, err := session.Tools(ctx)
	if err != nil {
		return nil, model.Err(503, err.Error())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current, err := r.mcpServer(config.ID)
	if err != nil { return nil, err }
	if current.Revision != config.Revision { return nil, model.Err(409, "MCP server changed during discovery") }
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM external_mcp_tools WHERE server_id=?", config.ID); err != nil {
		return nil, err
	}
	for _, tool := range tools {
		body, err := json.Marshal(tool)
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec("INSERT INTO external_mcp_tools(server_id,name,digest,body) VALUES(?,?,?,?)", config.ID, tool.Name, tool.Digest, string(body)); err != nil {
			return nil, err
		}
	}
	if err = insertAutomationEvent(tx, "mcp.tested", config.ID, "", map[string]any{"tools": len(tools)}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return r.mcpServerView(ctx, config.ID)
}

// mcpSetEnabled enables or disables one external server.
func (r *Relay) mcpSetEnabled(ctx context.Context, raw json.RawMessage) (any, error) {
	var request struct {
		ID               string `json:"id"`
		ExpectedRevision int    `json:"expectedRevision"`
		Enabled          bool   `json:"enabled"`
	}
	if err := decodeAutomation(raw, &request); err != nil {
		return nil, err
	}
	if !profileIdentifier(request.ID) || request.ExpectedRevision < 1 {
		return nil, model.Err(400, "id and expectedRevision are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	config, err := r.mcpServer(request.ID)
	if err != nil {
		return nil, err
	}
	if config.Revision != request.ExpectedRevision {
		return nil, model.Err(409, "MCP server revision changed")
	}
	if config.Enabled == request.Enabled {
		return nil, model.Err(409, "MCP server already has this enabled state")
	}
	// The enabled flag is not part of the reviewed definition: the revision
	// stays, so a binding keeps its review while the server is turned off and
	// on again.
	config.Enabled = request.Enabled
	body, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE external_mcp_servers SET body=? WHERE id=? AND revision=?", string(body), config.ID, request.ExpectedRevision); err != nil {
		return nil, err
	}
	revoked, err := invalidateConnectionNetworkLocked(ctx, tx, config.ID)
	if err != nil {
		return nil, err
	}
	if config.Enabled {
		if err = r.insertMCPGrants(ctx, tx, config); err != nil {
			return nil, err
		}
	}
	if err = insertAutomationEvent(tx, "mcp.enabled", config.ID, "", map[string]any{"enabled": config.Enabled, "revision": config.Revision}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.cancelExternalMCPLocked(config.ID, "")
	r.closeRevokedNetworkGrantsLocked(revoked)
	return r.mcpServerView(ctx, config.ID)
}

// mcpBind reviews the exact discovered tools of one server for one profile.
func (r *Relay) mcpBind(ctx context.Context, raw json.RawMessage) (any, error) {
	var request struct {
		ProfileID        string   `json:"profileId"`
		ServerID         string   `json:"serverId"`
		ExpectedRevision int      `json:"expectedRevision"`
		ToolNames        []string `json:"toolNames"`
		Enabled          bool     `json:"enabled"`
	}
	if err := decodeAutomation(raw, &request); err != nil {
		return nil, err
	}
	if !profileIdentifier(request.ProfileID) || !profileIdentifier(request.ServerID) {
		return nil, model.Err(400, "profileId and serverId are required")
	}
	if request.ExpectedRevision < 1 {
		return nil, model.Err(400, "expectedRevision is required")
	}
	if len(request.ToolNames) == 0 || len(request.ToolNames) > 64 {
		return nil, model.Err(400, "between 1 and 64 exact tool names are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	profile, err := r.profile(request.ProfileID)
	if err != nil {
		return nil, err
	}
	config, err := r.mcpServer(request.ServerID)
	if err != nil {
		return nil, err
	}
	if profile.WorkspaceID != config.WorkspaceID {
		return nil, model.Err(403, "MCP server must use the profile workspace")
	}
	if config.Revision != request.ExpectedRevision {
		return nil, model.Err(409, "MCP server revision changed")
	}
	discovered, err := r.mcpToolRows(config.ID)
	if err != nil {
		return nil, err
	}
	if len(discovered) == 0 {
		return nil, model.Err(409, "the server was not tested yet; its tools are unknown")
	}
	known := map[string]string{}
	for _, tool := range discovered {
		known[tool.Name] = tool.Digest
	}
	selected := []mcpToolBinding{}
	seen := map[string]bool{}
	for _, name := range request.ToolNames {
		digest, ok := known[name]
		if !ok {
			return nil, model.Err(400, "tool "+name+" was not advertised by this server")
		}
		if seen[name] {
			return nil, model.Err(400, "duplicate tool name in binding")
		}
		seen[name] = true
		selected = append(selected, mcpToolBinding{Name: name, Digest: digest})
	}
	names, err := json.Marshal(selected)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO profile_mcp_bindings(profile_id,server_id,revision,tool_names,enabled) VALUES(?,?,?,?,?)
 ON CONFLICT(profile_id,server_id) DO UPDATE SET revision=excluded.revision,tool_names=excluded.tool_names,enabled=excluded.enabled`, request.ProfileID, config.ID, config.Revision, string(names), boolInt(request.Enabled)); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE profile_tool_policies SET revision=revision+1 WHERE profile_id=?", request.ProfileID); err != nil { return nil, err }
	if err = insertAutomationEvent(tx, "mcp.bound", config.ID, request.ProfileID, map[string]any{"revision": config.Revision, "tools": len(selected), "enabled": request.Enabled}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.cancelExternalMCPLocked(config.ID, request.ProfileID)
	binding, err := r.mcpBinding(request.ProfileID, config.ID)
	if err != nil {
		return nil, err
	}
	return binding, nil
}

// mcpRemove deletes one external server. A bound or enabled server is kept.
func (r *Relay) mcpRemove(ctx context.Context, raw json.RawMessage) (any, error) {
	var request struct {
		ID               string `json:"id"`
		ExpectedRevision int    `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &request); err != nil {
		return nil, err
	}
	if !profileIdentifier(request.ID) || request.ExpectedRevision < 1 {
		return nil, model.Err(400, "id and expectedRevision are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	config, err := r.mcpServer(request.ID)
	if err != nil {
		return nil, err
	}
	if config.Revision != request.ExpectedRevision {
		return nil, model.Err(409, "MCP server revision changed")
	}
	if config.Enabled {
		return nil, model.Err(409, "disable the MCP server before removing it")
	}
	var bindings int
	if err = r.db.QueryRow("SELECT count(*) FROM profile_mcp_bindings WHERE server_id=?", config.ID).Scan(&bindings); err != nil {
		return nil, err
	}
	if bindings > 0 {
		return nil, model.Err(409, "the MCP server is still bound to a profile")
	}
	token, err := r.mcpTokenSecret(config.ID)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	revoked, err := invalidateConnectionNetworkLocked(ctx, tx, config.ID)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("DELETE FROM external_mcp_servers WHERE id=? AND revision=?", config.ID, request.ExpectedRevision); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("DELETE FROM external_mcp_tools WHERE server_id=?", config.ID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("DELETE FROM mcp_auth_tokens WHERE server_id=?", config.ID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("DELETE FROM mcp_auth_flows WHERE server_id=?", config.ID); err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, "mcp.removed", config.ID, "", map[string]any{"revision": config.Revision}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.closeRevokedNetworkGrantsLocked(revoked)
	if token != "" {
		if err = r.deleteSecretLocked(ctx, token); err != nil {
			return nil, err
		}
	}
	return map[string]any{"id": config.ID, "removed": true}, nil
}

// mcpSession opens one session with an external server. The returned snapshot
// is the private workspace copy of a stdio server; it is nil for http.
func (r *Relay) mcpSession(ctx context.Context, config externalmcp.Config) (*externalmcp.Session, *sandbox.Snapshot, error) {
	options := externalmcp.Options{Config: config, Timeout: mcpSessionTimeout}
	var snapshot *sandbox.Snapshot
	switch config.Transport {
	case externalmcp.TransportHTTP:
		client, err := r.mcpHTTPClient(ctx, config, "")
		if err != nil {
			return nil, nil, err
		}
		options.HTTPClient = client
		token, err := r.mcpAccessToken(ctx, config)
		if err != nil {
			return nil, nil, err
		}
		options.BearerToken = token
		headers, err := r.mcpSecretHeaders(ctx, config)
		if err != nil {
			return nil, nil, err
		}
		options.Headers = headers
	case externalmcp.TransportStdio:
		launcher, workspace, err := r.mcpStdioLauncher(ctx, config)
		if err != nil {
			return nil, nil, err
		}
		snapshot = workspace
		options.Launcher = launcher
		env, err := r.mcpSecretEnv(ctx, config)
		if err != nil {
			snapshot.Close()
			return nil, nil, err
		}
		options.Env = env
	default:
		return nil, nil, model.Err(400, "unsupported MCP transport")
	}
	session, err := externalmcp.Open(ctx, options)
	if err != nil {
		if snapshot != nil {
			snapshot.Close()
		}
		if errors.Is(err, externalmcp.ErrUnavailable) {
			return nil, nil, model.Err(503, err.Error())
		}
		return nil, nil, model.Err(503, err.Error())
	}
	return session, snapshot, nil
}

// mcpHTTPClient returns a policy-scoped client for one reviewed origin.
func (r *Relay) mcpHTTPClient(ctx context.Context, config externalmcp.Config, origin string) (*http.Client, error) {
	if origin == "" {
		computed, err := config.EndpointOrigin()
		if err != nil {
			return nil, model.Err(400, err.Error())
		}
		origin = computed
	}
	normalized, err := externalmcp.OriginOf(origin)
	if err != nil { return nil, model.Err(400, "invalid MCP credential origin") }
	origin = normalized
	if !config.AllowedOrigin(origin) {
		return nil, model.Err(403, "the origin was not reviewed for this MCP server")
	}
	scope := netpolicy.Scope{ConnectionID: config.ID}
	client, err := r.networkClient(scope, netpolicy.ClientOptions{Origin: origin, CredentialOrigin: origin, Timeout: 30 * time.Second, MaxResponseBytes: 8 << 20, MaxRequestBytes: 8 << 20})
	if err != nil {
		return nil, model.Err(403, "the origin is not authorized for outbound access")
	}
	return client, nil
}

// mcpSecretHeaders resolves the operator-provided header values.
func (r *Relay) mcpSecretHeaders(ctx context.Context, config externalmcp.Config) (map[string]string, error) {
	headers := map[string]string{}
	for _, ref := range config.SecretHeaderRefs {
		name, secretID, ok := strings.Cut(ref, ":")
		if !ok || name == "" || secretID == "" || !externalmcp.SafeText(name) {
			return nil, model.Err(400, "header secret references use the name:secret form")
		}
		value, err := r.readSecret(ctx, secretID)
		if err != nil {
			return nil, model.Err(503, "a referenced header secret is unavailable")
		}
		headers[name] = value
	}
	return headers, nil
}

// mcpSecretEnv resolves the operator-provided environment values.
func (r *Relay) mcpSecretEnv(ctx context.Context, config externalmcp.Config) (map[string]string, error) {
	env := map[string]string{}
	for _, ref := range config.SecretEnvRefs {
		name, secretID, ok := strings.Cut(ref, ":")
		if !ok || name == "" || secretID == "" || !externalmcp.SecretName(name) {
			return nil, model.Err(400, "environment secret references use the NAME:secret form")
		}
		value, err := r.readSecret(ctx, secretID)
		if err != nil {
			return nil, model.Err(503, "a referenced environment secret is unavailable")
		}
		env[name] = value
	}
	return env, nil
}

// mcpStoredGrant is the encrypted payload of one authorized server. It keeps
// the token set and the discovered client together, so a refresh never
// registers a second client behind the operator's back.
type mcpStoredGrant struct {
	Token     externalmcp.Token          `json:"token"`
	Discovery externalmcp.OAuthDiscovery `json:"discovery"`
	ServerDigest string `json:"serverDigest"`
	AuthorizationID string `json:"authorizationId"`
	recordID string
}

// mcpAccessToken returns the current access token of one server, refreshing a
// stale grant when the authorization server still accepts it.
func (r *Relay) mcpAccessToken(ctx context.Context, config externalmcp.Config) (string, error) {
	for {
		r.mu.Lock()
		if r.mcpRefreshes == nil { r.mcpRefreshes = make(map[string]chan struct{}) }
		if pending := r.mcpRefreshes[config.ID]; pending != nil {
			r.mu.Unlock()
			select { case <-pending: continue; case <-ctx.Done(): return "", ctx.Err() }
		}
		pending := make(chan struct{})
		r.mcpRefreshes[config.ID] = pending
		r.mu.Unlock()
		defer func() { r.mu.Lock(); delete(r.mcpRefreshes, config.ID); close(pending); r.mu.Unlock() }()
		break
	}
	grant, err := r.mcpStoredGrant(ctx, config.ID)
	if err != nil {
		return "", err
	}
	if grant == nil {
		return "", nil
	}
	if grant.ServerDigest != mcpCredentialDigest(config) || grant.Discovery.Resource != config.URL {
		return "", model.Err(409, "stored authorization belongs to a changed MCP server; sign in again")
	}
	if !grant.Token.Expired() {
		return grant.Token.AccessToken, nil
	}
	if grant.Token.RefreshToken == "" {
		return "", model.Err(409, "the authorization expired; sign in again")
	}
	refreshed, err := r.mcpRefreshToken(ctx, config, grant)
	if err != nil {
		return "", err
	}
	return refreshed.Token.AccessToken, nil
}

// mcpStoredGrant reads the encrypted grant of one server.
func (r *Relay) mcpStoredGrant(ctx context.Context, serverID string) (*mcpStoredGrant, error) {
	secretID, err := r.mcpTokenSecret(serverID)
	if err != nil {
		return nil, err
	}
	if secretID == "" {
		return nil, nil
	}
	value, err := r.readSecret(ctx, secretID)
	if err != nil {
		return nil, model.Err(503, "the stored authorization is unavailable")
	}
	var grant mcpStoredGrant
	if err = json.Unmarshal([]byte(value), &grant); err != nil {
		return nil, model.Err(503, "the stored authorization is unreadable")
	}
	grant.recordID = secretID
	registerSecretValue(grant.Token.AccessToken)
	registerSecretValue(grant.Token.RefreshToken)
	return &grant, nil
}

// mcpRefreshToken exchanges a refresh token and stores the new token set.
func (r *Relay) mcpRefreshToken(ctx context.Context, config externalmcp.Config, grant *mcpStoredGrant) (*mcpStoredGrant, error) {
	client, err := r.mcpHTTPClient(ctx, config, grant.Discovery.Issuer)
	if err != nil {
		return nil, err
	}
	refreshed, err := externalmcp.RefreshToken(ctx, client, &grant.Discovery, grant.Token.RefreshToken)
	if err != nil {
		if errors.Is(err, externalmcp.ErrInvalidGrant) {
			// The grant is gone: local use stops and the operator signs in again.
			r.mu.Lock()
			current, loadErr := r.mcpTokenSecret(config.ID)
			if loadErr == nil && current == grant.recordID { loadErr = r.mcpClearTokenLocked(ctx, config.ID) }
			r.mu.Unlock()
			if loadErr != nil { return nil, loadErr }
			return nil, model.Err(409, "the authorization is no longer valid; sign in again")
		}
		// A transient failure keeps the stored grant; it is not discarded.
		return nil, model.Err(503, "the authorization server is temporarily unavailable")
	}
	if refreshed.RefreshToken == "" { refreshed.RefreshToken = grant.Token.RefreshToken }
	if len(refreshed.Scopes) == 0 { refreshed.Scopes = grant.Token.Scopes }
	updated := &mcpStoredGrant{Token: *refreshed, Discovery: grant.Discovery, ServerDigest: grant.ServerDigest, AuthorizationID: grant.AuthorizationID, recordID: grant.recordID}
	if err = r.mcpStoreToken(ctx, config.ID, updated, nil); err != nil {
		return nil, err
	}
	return updated, nil
}

// mcpDiscovery resolves the OAuth configuration of one http server.
func (r *Relay) mcpDiscovery(ctx context.Context, config externalmcp.Config, redirectURI string) (*externalmcp.OAuthDiscovery, error) {
	if config.Transport != externalmcp.TransportHTTP {
		return nil, model.Err(400, "OAuth applies to http servers only")
	}
	if !mcpRedirectAllowed(redirectURI) {
		return nil, model.Err(400, "the redirect URI must be the exact loopback console callback")
	}
	client := &http.Client{Transport: &mcpOAuthTransport{relay: r, config: config}, Timeout: 30*time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	discovery, err := externalmcp.DiscoverOAuth(ctx, client, config.URL, redirectURI, nil)
	if err != nil {
		return nil, model.Err(503, "OAuth discovery failed: "+err.Error())
	}
	issuer, err := externalmcp.OriginOf(discovery.Issuer)
	if err != nil {
		return nil, model.Err(503, "the authorization server origin is invalid")
	}
	if !config.AllowedOrigin(issuer) {
		return nil, model.Err(403, "the authorization server origin was not reviewed for this MCP server")
	}
	return discovery, nil
}

// mcpRedirectPath is the console callback path of one OAuth flow.
const mcpRedirectPath = "/console/api/automation/mcp-oauth-callback"

// mcpRedirectAllowed accepts only the exact loopback console callback.
func mcpRedirectAllowed(raw string) bool {
	if raw == "" || len(raw) > 2048 {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return false
	}
	return parsed.Path == mcpRedirectPath
}

// mcpTokenSecret returns the stored secret reference of one server's token.
func (r *Relay) mcpTokenSecret(serverID string) (string, error) {
	var secretID string
	err := r.db.QueryRow("SELECT secret_id FROM mcp_auth_tokens WHERE server_id=?", serverID).Scan(&secretID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return secretID, nil
}

type mcpTokenClaim struct {
	flow           mcpAuthFlow
	body           string
	serverRevision int
}

// mcpStoreToken encrypts and stores the grant of one server.
func (r *Relay) mcpStoreToken(ctx context.Context, serverID string, grant *mcpStoredGrant, claim *mcpTokenClaim) error {
	if claim != nil { grant.AuthorizationID = randomID() }
	registerSecretValue(grant.Token.AccessToken)
	registerSecretValue(grant.Token.RefreshToken)
	body, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	secretID, nonce, ciphertext, err := r.prepareSecret(mcpSecretKindToken, string(body))
	if err != nil {
		return err
	}
	expires := ""
	if !grant.Token.ExpiresAt.IsZero() {
		expires = grant.Token.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	scopes, err := json.Marshal(grant.Token.Scopes)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(map[string]any{"scopes": grant.Token.Scopes, "issuer": grant.Discovery.Issuer, "clientId": grant.Discovery.ClientID, "authorizationId": grant.AuthorizationID})
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return model.Err(503, "relay is stopping")
	}
	if claim == nil {
		current, err := r.mcpServer(serverID)
		if err != nil || mcpCredentialDigest(current) != grant.ServerDigest { return model.Err(409, "MCP server changed during token refresh") }
		stored, err := r.mcpTokenSecret(serverID)
		if err != nil { return err }
		if grant.recordID == "" || stored != grant.recordID { return model.Err(409, "MCP authorization was revoked or replaced during refresh") }
	}
	if claim != nil {
		// No validator callback under relay ownership. Revocation releases
		// sessionMu before waiting for relay ownership.
		r.sessionMu.Lock()
		defer r.sessionMu.Unlock()
		if r.revokedConsoleSessions[claim.flow.SessionOwner] {
			return model.Err(403, "authorization owner session was revoked")
		}
		deadline, err := time.Parse(time.RFC3339Nano, claim.flow.ExpiresAt)
		if err != nil || !deadline.After(now()) {
			return model.Err(400, "authorization flow expired before completion")
		}
		current, err := r.mcpServer(serverID)
		if err != nil || current.Revision != claim.serverRevision {
			return model.Err(409, "MCP server changed during authorization")
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if claim != nil {
		consumed := claim.flow
		consumed.Consumed = true
		consumed.VerifierID = ""
		encoded, err := json.Marshal(consumed)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "UPDATE mcp_auth_flows SET body=? WHERE id=? AND body=? AND session_owner=?", string(encoded), claim.flow.ID, claim.body, claim.flow.SessionOwner)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return model.Err(409, "authorization flow was revoked or changed")
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM automation_secrets WHERE id=?", claim.flow.VerifierID); err != nil {
			return err
		}
	}
	if err = r.insertSecret(ctx, tx, secretID, mcpSecretKindToken, nonce, ciphertext); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO mcp_auth_tokens(server_id,secret_id,expires_at,scopes,body) VALUES(?,?,?,?,?)
 ON CONFLICT(server_id) DO UPDATE SET secret_id=excluded.secret_id,expires_at=excluded.expires_at,scopes=excluded.scopes,body=excluded.body`, serverID, secretID, expires, string(scopes), string(metadata)); err != nil {
		return err
	}
	if err = insertAutomationEvent(tx, "mcp.authorized", serverID, "", map[string]any{"expiresAt": expires}); err != nil {
		return err
	}
	return tx.Commit()
}

// mcpClearTokenLocked removes the stored token of one server.
func (r *Relay) mcpClearTokenLocked(ctx context.Context, serverID string) error {
	secretID, err := r.mcpTokenSecret(serverID)
	if err != nil {
		return err
	}
	config, err := r.mcpServer(serverID)
	if err != nil { return err }
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM mcp_auth_tokens WHERE server_id=?", serverID); err != nil {
		return err
	}
	revoked, err := invalidateConnectionNetworkLocked(ctx, tx, serverID)
	if err != nil { return err }
	if err = r.insertMCPGrants(ctx, tx, config); err != nil { return err }
	if _, err = tx.ExecContext(ctx, "DELETE FROM mcp_auth_flows WHERE server_id=?", serverID); err != nil { return err }
	if err = tx.Commit(); err != nil {
		return err
	}
	r.cancelExternalMCPLocked(serverID, "")
	r.closeRevokedNetworkGrantsLocked(revoked)
	if secretID != "" {
		return r.deleteSecretLocked(ctx, secretID)
	}
	return nil
}

// mcpAuthStart begins one operator-initiated authorization flow.
func (r *Relay) mcpAuthStart(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	var request struct {
		ID          string `json:"id"`
		RedirectURI string `json:"redirectUri,omitempty"`
	}
	if err := decodeAutomation(raw, &request); err != nil {
		return nil, err
	}
	if !profileIdentifier(request.ID) {
		return nil, model.Err(400, "MCP server id is required")
	}
	r.mu.Lock()
	listenerURL := r.mcpURL
	r.mu.Unlock()
	listener, listenerErr := url.Parse(listenerURL)
	if listenerErr != nil || listener.Host == "" {
		return nil, model.Err(503, "the loopback listener is not ready for OAuth")
	}
	redirectURI := listener.Scheme + "://" + listener.Host + mcpRedirectPath
	if !mcpRedirectAllowed(redirectURI) || request.RedirectURI != "" && request.RedirectURI != redirectURI {
		return nil, model.Err(400, "the redirect URI must match this daemon's exact loopback callback")
	}
	r.mu.Lock()
	config, err := r.mcpServer(request.ID)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if config.Transport != externalmcp.TransportHTTP {
		return nil, model.Err(400, "only http servers use an authorization flow")
	}
	discovery, err := r.mcpDiscovery(ctx, config, redirectURI)
	if err != nil {
		return nil, err
	}
	if discovery.ClientID == "" {
		return nil, model.Err(409, "the authorization server does not offer a client registration for this flow")
	}
	state, err := externalmcp.NewState()
	if err != nil {
		return nil, err
	}
	verifier, err := externalmcp.NewVerifier()
	if err != nil {
		return nil, err
	}
	pendingBody, err := json.Marshal(mcpPendingAuthorization{Verifier: verifier, Discovery: *discovery})
	if err != nil { return nil, err }
	verifierID, err := r.storeSecret(ctx, mcpSecretKindVerifier, string(pendingBody))
	if err != nil {
		return nil, err
	}
	authorizationURL, err := externalmcp.AuthorizationURL(discovery, state, verifier, discovery.ScopesSupported)
	if err != nil {
		return nil, model.Err(400, err.Error())
	}
	flow := mcpAuthFlow{ID: randomID(), ServerID: config.ID, ServerRevision: config.Revision, SessionOwner: p.ConsoleSessionID, StateHash: tokenHash(state), VerifierID: verifierID, RedirectURI: redirectURI, Issuer: discovery.Issuer, ClientID: discovery.ClientID, ExpiresAt: now().Add(mcpFlowLifetime).Format(time.RFC3339Nano)}
	body, err := json.Marshal(flow)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current, err := r.mcpServer(config.ID)
	if err != nil || current.Revision != config.Revision { return nil, model.Err(409, "MCP server changed during authorization start") }
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO mcp_auth_flows(id,server_id,session_owner,state_hash,expires_at,body) VALUES(?,?,?,?,?,?)", flow.ID, config.ID, flow.SessionOwner, flow.StateHash, flow.ExpiresAt, string(body)); err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, "mcp.auth.started", config.ID, "", map[string]any{"flowId": flow.ID}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"id": flow.ID, "serverId": config.ID, "authorizationUrl": authorizationURL, "expiresAt": flow.ExpiresAt}, nil
}

// MCPAuthCallback completes one authorization flow. The console route passes
// the provider's code and state; the state is single use and expires.
func (r *Relay) MCPAuthCallback(ctx context.Context, code, state string) error {
	if code == "" || state == "" {
		return model.Err(400, "the authorization response is incomplete")
	}
	r.mu.Lock()
	var flow mcpAuthFlow
	var body string
	var expires string
	err := r.db.QueryRow("SELECT body,expires_at FROM mcp_auth_flows WHERE state_hash=?", tokenHash(state)).Scan(&body, &expires)
	r.mu.Unlock()
	if errors.Is(err, sql.ErrNoRows) {
		return model.Err(400, "the authorization state is unknown or already used")
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(body), &flow); err != nil {
		return err
	}
	if flow.Consumed {
		return model.Err(409, "the authorization state was already used")
	}
	deadline, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !deadline.After(now()) {
		return model.Err(400, "the authorization state expired")
	}
	jobContext, release, err := r.beginConsoleJob(ctx, flow.SessionOwner)
	if err != nil {
		return err
	}
	defer release()
	ctx = jobContext
	// Claim one-use state before any token exchange. A failed exchange requires
	// a new human-initiated flow; concurrent callbacks cannot both redeem it.
	flow.Consumed = true
	claimedBody, claimErr := json.Marshal(flow)
	if claimErr != nil {
		return claimErr
	}
	r.mu.Lock()
	claimed, claimErr := r.db.ExecContext(ctx, "UPDATE mcp_auth_flows SET body=? WHERE id=? AND body=?", string(claimedBody), flow.ID, body)
	r.mu.Unlock()
	if claimErr != nil {
		return claimErr
	}
	claimedCount, claimErr := claimed.RowsAffected()
	if claimErr != nil {
		return claimErr
	}
	if claimedCount != 1 {
		return model.Err(409, "the authorization flow was revoked or already used")
	}
	body = string(claimedBody)
	r.mu.Lock()
	config, err := r.mcpServer(flow.ServerID)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if config.Revision != flow.ServerRevision { return model.Err(409, "MCP server changed during authorization") }
	pendingBody, err := r.readSecret(ctx, flow.VerifierID)
	if err != nil { return model.Err(503, "the pending authorization is unavailable") }
	var pending mcpPendingAuthorization
	if err = json.Unmarshal([]byte(pendingBody), &pending); err != nil { return model.Err(503, "the pending authorization is unreadable") }
	discovery := &pending.Discovery
	if discovery.ClientID != flow.ClientID || discovery.Issuer != flow.Issuer || discovery.Resource != config.URL || discovery.RedirectURI != flow.RedirectURI {
		return model.Err(409, "the authorization client or resource changed; start sign-in again")
	}
	client, err := r.mcpHTTPClient(ctx, config, discovery.Issuer)
	if err != nil {
		return err
	}
	token, err := externalmcp.ExchangeCode(ctx, client, discovery, code, pending.Verifier)
	if err != nil {
		if errors.Is(err, externalmcp.ErrInvalidGrant) {
			return model.Err(409, "the authorization code is no longer valid")
		}
		return model.Err(503, "the authorization server refused the exchange")
	}
	if err = r.requireConsole(model.Principal{Admin: true, ConsoleSessionID: flow.SessionOwner}); err != nil {
		return err
	}
	return r.mcpStoreToken(ctx, config.ID, &mcpStoredGrant{Token: *token, Discovery: *discovery, ServerDigest: mcpCredentialDigest(config)}, &mcpTokenClaim{flow: flow, body: body, serverRevision: config.Revision})
}

// mcpAuthStatus reports the authorization state of one server.
func (r *Relay) mcpAuthStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeAutomation(raw, &request); err != nil {
		return nil, err
	}
	if !profileIdentifier(request.ID) {
		return nil, model.Err(400, "MCP server id is required")
	}
	r.mu.Lock()
	config, err := r.mcpServer(request.ID)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	state, expires, err := r.mcpAuthState(ctx, config.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"serverId": config.ID, "auth": state, "expiresAt": expires}, nil
}

// mcpSignOut stops the local use of one server immediately and attempts a
// bounded upstream revocation.
func (r *Relay) mcpSignOut(ctx context.Context, raw json.RawMessage) (any, error) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeAutomation(raw, &request); err != nil {
		return nil, err
	}
	if !profileIdentifier(request.ID) {
		return nil, model.Err(400, "MCP server id is required")
	}
	r.mu.Lock()
	config, err := r.mcpServer(request.ID)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	revoked := false
	grant, err := r.mcpStoredGrant(ctx, config.ID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	err = r.mcpClearTokenLocked(ctx, config.ID)
	r.mu.Unlock()
	if err != nil { return nil, err }
	if grant != nil && config.Transport == externalmcp.TransportHTTP && grant.Token.AccessToken != "" {
		client, clientErr := r.mcpHTTPClient(ctx, config, grant.Discovery.Issuer)
		if clientErr == nil {
			revokeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if revokeErr := externalmcp.RevokeToken(revokeCtx, client, &grant.Discovery, grant.Token.AccessToken); revokeErr == nil {
				revoked = true
			}
			cancel()
		}
	}
	return map[string]any{"serverId": config.ID, "auth": "signed_out", "upstreamRevoked": revoked}, nil
}

// mcpStdioLauncher prepares a confined worker for one stdio server.
func (r *Relay) mcpStdioLauncher(ctx context.Context, config externalmcp.Config) (externalmcp.Launcher, *sandbox.Snapshot, error) {
	runtimeConfig, _, err := r.runtimeConfig()
	if err != nil {
		return nil, nil, err
	}
	if runtimeConfig.Executables["bwrap"] == "" {
		return nil, nil, model.Err(503, "confined runtime is not configured: bubblewrap is missing")
	}
	allowed := false
	for _, executable := range runtimeConfig.Executables {
		if executable == config.Executable {
			allowed = true
		}
	}
	if !allowed {
		return nil, nil, model.Err(403, "the MCP server executable is not in the operator-reviewed runtime manifest")
	}
	workspace, err := r.prepareWorkspace(ctx, config.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	argv := append([]string{config.Executable}, config.Args...)
	return &mcpConfinedLauncher{relay: r, config: runtimeConfig, workspace: workspace, argv: argv}, workspace, nil
}

// mcpConfinedLauncher starts one stdio server inside the confined worker.
type mcpConfinedLauncher struct {
	relay     *Relay
	config    sandbox.Config
	workspace *sandbox.Snapshot
	argv      []string
}

// Launch starts the confined worker and returns its two pipes.
func (l *mcpConfinedLauncher) Launch(ctx context.Context, config externalmcp.Config, env map[string]string) (externalmcp.Process, error) {
	prepared, err := sandbox.Prepare(ctx, l.config, sandbox.Request{Workspace: l.workspace, Argv: l.argv, Kind: sandbox.ACP, ReadOnly: true, Env: env, Timeout: mcpSessionTimeout})
	if err != nil {
		return nil, sandboxToolError(err)
	}
	stdin, err := prepared.Cmd.StdinPipe()
	if err != nil {
		prepared.Close()
		return nil, fmt.Errorf("confined worker stdin is unavailable: %w", err)
	}
	stdout, err := prepared.Cmd.StdoutPipe()
	if err != nil {
		prepared.Close()
		return nil, fmt.Errorf("confined worker stdout is unavailable: %w", err)
	}
	if err = prepared.Start(); err != nil {
		prepared.Close()
		return nil, sandboxToolError(err)
	}
	return &mcpConfinedProcess{prepared: prepared, stdin: stdin, stdout: stdout}, nil
}

// mcpConfinedProcess is the pipe pair of one confined stdio server.
type mcpConfinedProcess struct {
	prepared *sandbox.Prepared
	stdin    io.WriteCloser
	stdout   io.ReadCloser
}

// Stdin returns the write end of the worker's standard input.
func (p *mcpConfinedProcess) Stdin() io.WriteCloser { return p.stdin }

// Stdout returns the read end of the worker's standard output.
func (p *mcpConfinedProcess) Stdout() io.ReadCloser { return p.stdout }

// Close terminates the worker and its pipes.
func (p *mcpConfinedProcess) Close() error {
	_ = p.stdin.Close()
	_ = p.stdout.Close()
	return p.prepared.Close()
}

// mcpNotFound reports whether an error is a missing resource.
func mcpNotFound(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Status == 404
}

// externalMCPRequest is one model-requested external tool call. Only the
// server and the tool are chosen by the model; the profile, the workspace and
// the policy come from the admitted turn.
type externalMCPRequest struct {
	ServerID  string          `json:"serverId"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// dispatchExternalMCP performs one approved external MCP tool call. The
// broker owns the one-call approval; this function revalidates the server and
// binding revision snapshot immediately before the call is dispatched.
func (r *Relay) dispatchExternalMCP(ctx context.Context, principal model.Principal, profileID, invocationID string, request externalMCPRequest) (model.ToolResult, error) {
	var zero model.ToolResult
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := validateExternalMCPRequest(&request); err != nil {
		return zero, err
	}
	r.mu.Lock()
	_, call, config, pin, err := r.externalMCPCallLocked(principal, profileID, invocationID, request)
	r.mu.Unlock()
	if err != nil {
		return zero, err
	}
	if err = r.recheckExternalMCP(ctx, principal, profileID, invocationID, request); err != nil {
		return zero, err
	}
	session, snapshot, err := r.mcpSession(ctx, config)
	if err != nil {
		return zero, err
	}
	defer func() {
		session.Close()
		if snapshot != nil {
			snapshot.Close()
		}
	}()
	tools, err := session.Tools(ctx)
	if err != nil {
		return zero, model.Err(503, err.Error())
	}
	current := ""
	for _, tool := range tools {
		if tool.Name == request.Tool {
			current = tool.Digest
		}
	}
	if current == "" {
		return zero, model.Err(409, "the server no longer advertises the reviewed tool")
	}
	if current != pin.ToolDigest {
		return zero, model.Err(409, "the tool definition changed; a renewed binding review is required")
	}
	if err = r.recheckExternalMCP(ctx, principal, profileID, invocationID, request); err != nil {
		return zero, err
	}
	r.mu.Lock()
	_, currentCall, _, _, err := r.externalMCPCallLocked(principal, profileID, invocationID, request)
	if err == nil && (currentCall != call || call.externalDispatched) {
		err = model.Err(409, "external MCP action has already been dispatched")
	}
	if err == nil {
		call.externalDispatched = true
	}
	r.mu.Unlock()
	if err != nil {
		return zero, err
	}
	result, err := session.Call(ctx, request.Tool, request.Arguments)
	// Persist private edits even when the remote tool reports a failure.
	// A stdio server never holds a writable original file. Any change it made
	// inside the private snapshot follows the normal changeset flow.
	if snapshot != nil {
		receipt, changesErr := r.mcpSnapshotChangeset(ctx, invocationID, config, snapshot)
		if changesErr != nil {
			return zero, fmt.Errorf("external MCP changeset creation failed: %w", changesErr)
		}
		if result != nil && receipt != nil {
			result.Structured = receipt
		}
	}
	if err != nil {
		return zero, model.Err(503, err.Error())
	}
	body, err := json.Marshal(result)
	if err != nil {
		return zero, model.Err(503, "the external MCP result could not be encoded")
	}
	return model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: redactSecrets(string(body))}}, IsError: result.IsError}, nil
}

// mcpSnapshotChangeset turns the private snapshot of one stdio call into the
// staged changeset of the normal apply flow.
func (r *Relay) mcpSnapshotChangeset(ctx context.Context, invocationID string, config externalmcp.Config, workspace *sandbox.Snapshot) (json.RawMessage, error) {
	manifest, err := workspace.Changes(ctx)
	if err != nil {
		return nil, err
	}
	defer manifest.Close()
	if len(manifest.Changes) == 0 {
		return nil, nil
	}
	changeset, err := r.persistChangeset(ctx, invocationID, config.WorkspaceID, manifest)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(changesetReceipt(changeset))
	if err != nil {
		return nil, err
	}
	return body, nil
}
