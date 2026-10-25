package relay

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"meshra/internal/externalmcp"
	"meshra/internal/model"
)

// These fixtures drive the production client against a local MCP server. They
// exercise the real operator operations, the real binding review and the real
// dispatch path; nothing here asserts on source text or on echoed model text.

// mcpFixture is one local MCP server with optional OAuth endpoints.
type mcpFixture struct {
	server *httptest.Server
	mu     sync.Mutex
	// current is the served MCP server. Swapping it changes the advertised
	// tool definition without changing the origin.
	current *mcp.Server
	// challenge is the PKCE challenge of the issued authorization code.
	challenge string
	code      string
	// accessToken and refreshToken are the values the fixture hands out.
	accessToken  string
	refreshToken string
	// requireToken refuses unauthenticated tool calls.
	requireToken bool
	calls        int
}

func newMCPFixture(t *testing.T, requireToken bool) *mcpFixture {
	t.Helper()
	fixture := &mcpFixture{accessToken: "fixture-access-secret", refreshToken: "fixture-refresh-secret", requireToken: requireToken}
	fixture.current = mcpFixtureServer(fixture, "Return the received text.")
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeFixtureJSON(w, map[string]any{"resource": fixture.server.URL + "/mcp", "authorization_servers": []string{fixture.origin()}, "scopes_supported": []string{"mcp:read"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		origin := fixture.origin()
		writeFixtureJSON(w, map[string]any{
			"issuer":                                origin,
			"authorization_endpoint":                origin + "/authorize",
			"token_endpoint":                        origin + "/token",
			"registration_endpoint":                 origin + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"scopes_supported":                      []string{"mcp:read"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeFixtureJSON(w, map[string]any{"client_id": "fixture-client", "redirect_uris": []string{"http://127.0.0.1:37731/console/api/automation/mcp-oauth-callback"}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != fixture.code || fixture.challenge != fixture.pkceChallenge(r.Form.Get("code_verifier")) {
				w.WriteHeader(http.StatusBadRequest)
				writeFixtureJSON(w, map[string]any{"error": "invalid_grant"})
				return
			}
			fixture.code = ""
			writeFixtureJSON(w, map[string]any{"access_token": fixture.accessToken, "refresh_token": fixture.refreshToken, "token_type": "Bearer", "expires_in": 3600, "scope": "mcp:read"})
		case "refresh_token":
			if r.Form.Get("refresh_token") != fixture.refreshToken {
				w.WriteHeader(http.StatusBadRequest)
				writeFixtureJSON(w, map[string]any{"error": "invalid_grant"})
				return
			}
			writeFixtureJSON(w, map[string]any{"access_token": fixture.accessToken, "refresh_token": fixture.refreshToken, "token_type": "Bearer", "expires_in": 3600, "scope": "mcp:read"})
		default:
			w.WriteHeader(http.StatusBadRequest)
			writeFixtureJSON(w, map[string]any{"error": "unsupported_grant_type"})
		}
	})
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return fixture.current
	}, nil))
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)
	return fixture
}

// origin returns the exact origin of the fixture.
func (f *mcpFixture) origin() string {
	return strings.TrimSuffix(f.server.URL, "/")
}

// mcpFixtureServer builds one MCP server with a single echo tool.
func mcpFixtureServer(fixture *mcpFixture, description string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: description,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		fixture.mu.Lock()
		fixture.calls++
		fixture.mu.Unlock()
		if fixture.requireToken {
			header := ""
			if request.Extra != nil {
				header = request.Extra.Header.Get("Authorization")
			}
			if header != "Bearer "+fixture.accessToken {
				return nil, fmt.Errorf("the fixture requires an authorized client")
			}
		}
		var arguments struct {
			Text string `json:"text"`
		}
		if request.Params != nil {
			_ = json.Unmarshal(request.Params.Arguments, &arguments)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + arguments.Text}}}, nil
	})
	return server
}

// issueCode records the PKCE challenge of one authorization request.
func (f *mcpFixture) issueCode(challenge string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.challenge = challenge
	f.code = "fixture-code"
	return f.code
}

// pkceChallenge returns the S256 challenge of a verifier.
func (f *mcpFixture) pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// writeFixtureJSON writes one JSON document.
func writeFixtureJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// mcpApplySchema applies the external MCP migration when the daemon does not
// embed it yet. The integration owner applies the same file.
func mcpApplySchema(t *testing.T, r *Relay) {
	t.Helper()
	var count int
	if err := r.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='external_mcp_servers'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count > 0 {
		return
	}
	body, err := os.ReadFile(filepath.Join("migrations", "014.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
}

// mcpCall runs one operator operation.
func mcpCall(t *testing.T, r *Relay, principal model.Principal, operation string, payload any) (any, error) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return r.mcpClientOperation(context.Background(), principal, operation, body)
}

// mcpConsole installs a live console session for the operator.
func mcpConsole(r *Relay, session string) model.Principal {
	r.SetConsoleSessionValidator(func(id string) bool { return id == session })
	return model.Principal{Admin: true, ConsoleSessionID: session}
}

// mcpBrokerFixture uses real operator discovery, policy, binding and admission.
func mcpBrokerFixture(t *testing.T) (*Relay, *mcpFixture, *brokerScript, model.Principal, model.Profile, model.Conversation) {
	t.Helper()
	fixture := newMCPFixture(t, false)
	script := newBrokerScript(brokerStep{call: model.ToolCall{ID: "external-call", Name: "meshra_external_mcp", Arguments: json.RawMessage(`{"serverId":"mcp-fixture","tool":"echo","arguments":{"text":"hello"}}`)}})
	r, _ := continuityOpenTest(t, script.run)
	r.SetMCPURL("http://127.0.0.1:9/mcp")
	mcpApplySchema(t, r)
	console := mcpConsole(r, "mcp-broker-owner")
	workspace := profileStoreWorkspace(t, r)
	request := profileStoreRequest(workspace)
	profile := profileStoreCreate(t, r, request)
	if profile.Capabilities.ExternalMCP {
		t.Fatal("profile creation enabled external MCP")
	}
	if _, err := mcpCall(t, r, admin, "mcp.save", map[string]any{"server": map[string]any{
		"id": "mcp-fixture", "name": "Fixture", "transport": "http", "url": fixture.server.URL + "/mcp",
		"workspaceId": workspace.ID, "allowedOrigins": []string{fixture.origin()}, "allowPrivate": true,
	}}); err != nil {
		t.Fatal(err)
	}
	tested, err := mcpCall(t, r, admin, "mcp.test", map[string]any{"id": "mcp-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if tools := tested.(mcpServerView).Tools; len(tools) != 1 || tools[0].Digest == "" {
		t.Fatalf("missing discovered definition: %+v", tools)
	}
	request["id"], request["expectedRevision"] = profile.ID, profile.Revision
	request["capabilities"] = model.ProfileCapabilities{ExternalMCP: true}
	profile = profileStoreCreate(t, r, request)
	if !profile.Capabilities.ExternalMCP {
		t.Fatal("explicit policy did not enable external MCP")
	}
	if _, err = mcpCall(t, r, admin, "mcp.bind", map[string]any{"profileId": profile.ID, "serverId": "mcp-fixture", "expectedRevision": 1, "toolNames": []string{"echo"}, "enabled": true}); err != nil {
		t.Fatal(err)
	}
	if _, err = mcpCall(t, r, admin, "mcp.setEnabled", map[string]any{"id": "mcp-fixture", "expectedRevision": 1, "enabled": true}); err != nil {
		t.Fatal(err)
	}
	return r, fixture, script, console, profile, brokerConversation(t, r, "chat", profile)
}

func TestExternalMCPServerLifecycleAndBinding(t *testing.T) {
	r, _, script, console, profile, conversation := mcpBrokerFixture(t)
	task := brokerSend(t, r, console, profile, conversation, "use the external server")
	approval := awaitBrokerPendingApproval(t, r, task.ID)
	if script.pending(task.ID, 0) {
		t.Fatal("external call did not wait for human approval")
	}
	hash := brokerActionHash(t, r, approval)
	_, err := brokerDecision(t, r, console, "approvals.decide", map[string]any{"id": approval, "expectedRevision": 1, "decision": "approve", "actionHash": tokenHash("wrong action"), "idempotencyKey": randomID()})
	requireStatus(t, err, 409)
	if _, err = brokerDecision(t, r, console, "approvals.decide", map[string]any{"id": approval, "expectedRevision": 1, "decision": "approve", "actionHash": hash, "idempotencyKey": randomID()}); err != nil {
		t.Fatal(err)
	}
	outcome := script.await(t, task.ID, 0, 20*time.Second)
	if outcome.err != nil || !strings.Contains(brokerText(outcome.result), "echo:hello") {
		t.Fatalf("broker external call: result=%+v err=%v", outcome.result, outcome.err)
	}
	r.mu.Lock()
	snapshot, err := r.profileSnapshot(task)
	r.mu.Unlock()
	if err != nil || len(snapshot.ExternalMCP) != 1 || snapshot.ExternalMCP[0].ToolDigest == "" || snapshot.ExternalMCP[0].BindingDigest == "" || snapshot.ExternalMCP[0].ServerDigest == "" {
		t.Fatalf("missing immutable external reference: %+v %v", snapshot.ExternalMCP, err)
	}
	// Even a fabricated durable approved row is not a live broker grant.
	id := randomID()
	if _, err = r.db.Exec("INSERT INTO tool_invocations(id,task_id,attempt,call_id,action_hash,state,body) VALUES(?,?,1,?,?,'approved',?)", id, task.ID, randomID(), tokenHash(id), `{"profileId":"`+profile.ID+`"}`); err != nil {
		t.Fatal(err)
	}
	_, err = r.dispatchExternalMCP(context.Background(), model.Principal{AgentID: task.To, TaskID: task.ID}, profile.ID, id, externalMCPRequest{ServerID: "mcp-fixture", Tool: "echo", Arguments: json.RawMessage(`{"text":"hello"}`)})
	requireStatus(t, err, 403)
}

func TestExternalMCPChangedDefinitionNeedsARenewedBinding(t *testing.T) {
	r, fixture, script, console, profile, conversation := mcpBrokerFixture(t)
	task := brokerSend(t, r, console, profile, conversation, "use the external server")
	approval := awaitBrokerPendingApproval(t, r, task.ID)
	fixture.mu.Lock()
	fixture.current = mcpFixtureServer(fixture, "Return the received text and its length.")
	fixture.mu.Unlock()
	if _, err := brokerDecision(t, r, console, "approvals.decide", map[string]any{"id": approval, "expectedRevision": 1, "decision": "approve", "actionHash": brokerActionHash(t, r, approval), "idempotencyKey": randomID()}); err != nil {
		t.Fatal(err)
	}
	outcome := script.await(t, task.ID, 0, 20*time.Second)
	requireStatus(t, outcome.err, 409)
}

func TestExternalMCPRevocationsPreventBrokerDispatch(t *testing.T) {
	for _, boundary := range []string{"binding", "server", "edit", "signout", "session", "denied", "forged"} {
		t.Run(boundary, func(t *testing.T) {
			r, fixture, script, console, profile, conversation := mcpBrokerFixture(t)
			task := brokerSend(t, r, console, profile, conversation, "use the external server")
			approval := awaitBrokerPendingApproval(t, r, task.ID)
			var err error
			switch boundary {
			case "forged":
				r.mu.Lock()
				_, err = r.db.Exec("UPDATE tool_approvals SET status='approved' WHERE id=?", approval)
				if err == nil {
					_, err = r.db.Exec("UPDATE tool_invocations SET state='approved' WHERE task_id=?", task.ID)
				}
				if b := r.toolBrokers[task.ID]; b != nil {
					signalToolCall(b.calls["external-call"])
				}
				r.mu.Unlock()
			case "binding":
				_, err = mcpCall(t, r, admin, "mcp.bind", map[string]any{"profileId": profile.ID, "serverId": "mcp-fixture", "expectedRevision": 1, "toolNames": []string{"echo"}, "enabled": false})
			case "server":
				_, err = mcpCall(t, r, admin, "mcp.setEnabled", map[string]any{"id": "mcp-fixture", "expectedRevision": 1, "enabled": false})
			case "edit":
				r.mu.Lock()
				config, loadErr := r.mcpServer("mcp-fixture")
				r.mu.Unlock()
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				config.Enabled, config.Revision, config.Name = false, 0, "Reviewed again"
				_, err = mcpCall(t, r, admin, "mcp.save", map[string]any{"server": config, "expectedRevision": 1})
			case "signout":
				_, err = mcpCall(t, r, admin, "mcp.signOut", map[string]any{"id": "mcp-fixture"})
			case "session":
				r.RevokeConsoleSession(console.ConsoleSessionID)
			case "denied":
				_, err = brokerDecision(t, r, console, "approvals.decide", map[string]any{"id": approval, "expectedRevision": 1, "decision": "deny", "actionHash": brokerActionHash(t, r, approval), "idempotencyKey": randomID()})
			}
			if err != nil {
				t.Fatal(err)
			}
			outcome := script.await(t, task.ID, 0, 20*time.Second)
			if outcome.err == nil {
				t.Fatalf("%s failed to prevent external dispatch: %+v", boundary, outcome.result)
			}
			fixture.mu.Lock()
			calls := fixture.calls
			fixture.mu.Unlock()
			if calls != 0 {
				t.Fatalf("%s reached the external server %d times", boundary, calls)
			}
		})
	}
}

func TestExternalMCPOAuthFlowIsSingleUseAndHidesTokens(t *testing.T) {
	fixture := newMCPFixture(t, true)
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "done", nil })
	mcpApplySchema(t, r)
	r.SetMCPURL("http://127.0.0.1:9/mcp")
	workspace := profileStoreWorkspace(t, r)
	console := mcpConsole(r, "mcp-session")
	if _, err := mcpCall(t, r, admin, "mcp.save", map[string]any{"server": map[string]any{
		"id": "mcp-fixture", "name": "Fixture", "transport": "http", "url": fixture.server.URL + "/mcp",
		"workspaceId": workspace.ID, "allowedOrigins": []string{fixture.origin()}, "allowPrivate": true,
	}}); err != nil {
		t.Fatal(err)
	}
	// The operator starts the flow from a live console session.
	started, err := mcpCall(t, r, console, "mcp.authStart", map[string]any{"id": "mcp-fixture"})
	if err != nil {
		t.Fatalf("the authorization flow did not start: %v", err)
	}
	flow := started.(map[string]any)
	authorizationURL := flow["authorizationUrl"].(string)
	if !strings.Contains(authorizationURL, "code_challenge_method=S256") {
		t.Fatalf("the authorization URL is incomplete: %s", authorizationURL)
	}
	state := authorizationURL[strings.Index(authorizationURL, "state=")+len("state="):]
	if index := strings.Index(state, "&"); index >= 0 {
		state = state[:index]
	}
	// The provider binds the code to the public challenge, not vault internals.
	var verifierID string
	if err = r.db.QueryRow("SELECT json_extract(body,'$.verifierSecretId') FROM mcp_auth_flows WHERE id=?", flow["id"]).Scan(&verifierID); err != nil {
		t.Fatal(err)
	}
	authorization, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	code := fixture.issueCode(authorization.Query().Get("code_challenge"))

	// The initiating console must remain live even though the redirect itself
	// carries no cookie.
	r.SetConsoleSessionValidator(func(string) bool { return false })
	requireStatus(t, r.MCPAuthCallback(context.Background(), code, state), 403)
	r.SetConsoleSessionValidator(func(id string) bool { return id == console.ConsoleSessionID })

	if err = r.MCPAuthCallback(context.Background(), code, state); err != nil {
		t.Fatalf("the authorization callback failed: %v", err)
	}
	_, err = r.readSecret(context.Background(), verifierID)
	requireStatus(t, err, 404)
	status, err := mcpCall(t, r, admin, "mcp.authStatus", map[string]any{"id": "mcp-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if status.(map[string]any)["auth"] != "authorized" {
		t.Fatalf("unexpected authorization state: %+v", status)
	}
	body, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), fixture.accessToken) || strings.Contains(string(body), fixture.refreshToken) {
		t.Fatal("the authorization status exposed a token")
	}

	// The same state cannot be replayed.
	requireStatus(t, r.MCPAuthCallback(context.Background(), code, state), 409)

	// An expired flow is refused.
	expired, err := mcpCall(t, r, console, "mcp.authStart", map[string]any{"id": "mcp-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	expiredFlow := expired.(map[string]any)
	if _, err = r.db.Exec("UPDATE mcp_auth_flows SET expires_at=? WHERE id=?", time.Now().Add(-time.Minute).Format(time.RFC3339Nano), expiredFlow["id"]); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, r.MCPAuthCallback(context.Background(), "fixture-code", "unknown-state"), 400)
	expiredURL, parseErr := url.Parse(expiredFlow["authorizationUrl"].(string))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	requireStatus(t, r.MCPAuthCallback(context.Background(), "fixture-code", expiredURL.Query().Get("state")), 400)

	// The signed-out server blocks the authenticated call immediately.
	if _, err = mcpCall(t, r, admin, "mcp.signOut", map[string]any{"id": "mcp-fixture"}); err != nil {
		t.Fatalf("the sign-out failed: %v", err)
	}
	status, err = mcpCall(t, r, admin, "mcp.authStatus", map[string]any{"id": "mcp-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if status.(map[string]any)["auth"] != "signed_out" {
		t.Fatalf("unexpected authorization state after sign-out: %+v", status)
	}
	if _, err = mcpCall(t, r, admin, "mcp.setEnabled", map[string]any{"id": "mcp-fixture", "expectedRevision": 1, "enabled": true}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	config, err := r.mcpServer("mcp-fixture")
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	session, snapshot, err := r.mcpSession(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if snapshot != nil {
		defer snapshot.Close()
	}
	result, callErr := session.Call(context.Background(), "echo", json.RawMessage(`{"text":"hello"}`))
	if callErr == nil && !result.IsError {
		t.Fatal("signed-out client retained upstream authorization")
	}

	// No token value reaches the event log.
	var events string
	if err = r.db.QueryRow("SELECT group_concat(data) FROM automation_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(events, fixture.accessToken) || strings.Contains(events, fixture.refreshToken) {
		t.Fatal("an event carried a token value")
	}
}

func TestExternalMCPStdioServerNeverRunsOnTheHost(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "done", nil })
	mcpApplySchema(t, r)
	workspace := profileStoreWorkspace(t, r)
	executable := filepath.Join(t.TempDir(), "server")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := mcpCall(t, r, admin, "mcp.save", map[string]any{"server": map[string]any{
		"id": "mcp-stdio", "name": "Stdio fixture", "transport": "stdio", "executable": executable,
		"workspaceId": workspace.ID,
	}}); err != nil {
		t.Fatalf("the stdio server could not be saved: %v", err)
	}
	// Launching a stdio server executes code: without the confined runtime the
	// daemon refuses instead of running it on the host.
	_, err := mcpCall(t, r, admin, "mcp.test", map[string]any{"id": "mcp-stdio"})
	if err == nil {
		t.Fatal("a stdio server was launched without the confined runtime")
	}
	requireStatus(t, err, 503)
}

func TestExternalMCPOAuthRevokedOwnerCannotPersistGrant(t *testing.T) {
	fixture := newMCPFixture(t, true)
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "done", nil })
	r.SetMCPURL("http://127.0.0.1:9/mcp")
	workspace := profileStoreWorkspace(t, r)
	console := mcpConsole(r, "revoked-oauth-owner")
	if _, err := mcpCall(t, r, admin, "mcp.save", map[string]any{"server": map[string]any{"id": "mcp-fixture", "name": "Fixture", "transport": "http", "url": fixture.server.URL + "/mcp", "workspaceId": workspace.ID, "allowedOrigins": []string{fixture.origin()}, "allowPrivate": true}}); err != nil {
		t.Fatal(err)
	}
	started, err := mcpCall(t, r, console, "mcp.authStart", map[string]any{"id": "mcp-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	id := started.(map[string]any)["id"].(string)
	var body string
	if err = r.db.QueryRow("SELECT body FROM mcp_auth_flows WHERE id=?", id).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var flow mcpAuthFlow
	if err = json.Unmarshal([]byte(body), &flow); err != nil {
		t.Fatal(err)
	}
	r.RevokeConsoleSession(console.ConsoleSessionID)
	err = r.mcpStoreToken(context.Background(), "mcp-fixture", &mcpStoredGrant{Token: externalmcp.Token{AccessToken: "must-not-become-an-active-grant"}}, &mcpTokenClaim{flow: flow, body: body, serverRevision: 1})
	requireStatus(t, err, 403)
	status, err := mcpCall(t, r, admin, "mcp.authStatus", map[string]any{"id": "mcp-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if status.(map[string]any)["auth"] != "signed_out" {
		t.Fatal("revoked owner created a usable authorization")
	}
	_, err = r.readSecret(context.Background(), flow.VerifierID)
	requireStatus(t, err, 404)
	callback, err := url.Parse(started.(map[string]any)["authorizationUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, r.MCPAuthCallbackAt(context.Background(), flow.RedirectURI, "unused-code", callback.Query().Get("state")), 403)
}
