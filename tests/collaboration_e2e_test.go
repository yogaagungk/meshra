package tests

import (
	"encoding/json"
	"fmt"
	"meshra/internal/api"
	"meshra/internal/model"
	"meshra/internal/relay"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollaborationHTTPMCPAuthorization(t *testing.T) {
	dir := t.TempDir()
	r, err := relay.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	server := httptest.NewServer(api.New(r))
	defer server.Close()
	client := server.Client()
	bytes, err := os.ReadFile(filepath.Join(dir, "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	admin := string(bytes)
	tokens := map[string]string{}
	for _, name := range []string{"claude:apmobileprod", "claude:apmobilewallet", "opencode:reader"} {
		parts := strings.Split(name, ":")
		httpJSON(t, client, server.URL, admin, "POST", "/v1/agents", model.CreateAgent{Harness: parts[0], Name: parts[1], Cwd: t.TempDir()}, nil, 201)
		var v map[string]string
		httpJSON(t, client, server.URL, admin, "POST", "/v1/agents/"+name+"/token", nil, &v, 200)
		tokens[name] = v["token"]
	}
	prod := tokens["claude:apmobileprod"]
	wallet := tokens["claude:apmobilewallet"]
	reader := tokens["opencode:reader"]
	var c model.SharedContext
	httpJSON(t, client, server.URL, prod, "POST", "/v1/collaboration/context.publish", model.CollaborationRequest{Content: "private contract", Title: "API"}, &c, 200)
	request := model.CollaborationRequest{ContextID: c.ID, Version: 1}
	httpJSON(t, client, server.URL, wallet, "POST", "/v1/collaboration/context.get", request, nil, 403)
	// Membership deliberately includes wallet; it still cannot fetch private bytes.
	var conv model.Conversation
	httpJSON(t, client, server.URL, prod, "POST", "/v1/collaboration/conversation.create", model.CollaborationRequest{Title: "shared channel", Participants: []string{"claude:apmobilewallet", "opencode:reader"}}, &conv, 200)
	httpJSON(t, client, server.URL, prod, "POST", "/v1/collaboration/context.attach", model.CollaborationRequest{ContextID: c.ID, Version: 1, ConversationID: conv.ID}, nil, 200)
	httpJSON(t, client, server.URL, wallet, "POST", "/v1/collaboration/context.get", request, nil, 403)
	var visible model.Conversation
	httpJSON(t, client, server.URL, wallet, "POST", "/v1/collaboration/conversation.get", model.CollaborationRequest{ID: conv.ID}, &visible, 200)
	if len(visible.ContextRefs) != 0 {
		t.Fatal("conversation exposed a restricted binding")
	}
	rpc := func(token, method string, params any) map[string]any {
		t.Helper()
		var response map[string]any
		httpJSON(t, client, server.URL, token, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, &response, 200)
		return response
	}
	denied := rpc(wallet, "tools/call", map[string]any{"name": "meshra_collaboration_context_get", "arguments": request})
	if denied["result"].(map[string]any)["isError"] != true {
		t.Fatal("MCP bypassed context ACL")
	}
	uri := fmt.Sprintf("meshra://contexts/%s/versions/1", c.ID)
	if rpc(reader, "resources/read", map[string]any{"uri": uri})["error"] == nil {
		t.Fatal("resource bypassed ACL")
	}
	httpJSON(t, client, server.URL, prod, "POST", "/v1/collaboration/context.grant", model.CollaborationRequest{ContextID: c.ID, AgentID: "opencode:reader"}, nil, 200)
	got := rpc(reader, "resources/read", map[string]any{"uri": uri})
	encoded, _ := json.Marshal(got)
	if got["error"] != nil || !strings.Contains(string(encoded), "private contract") {
		t.Fatalf("authorized resource failed %s", encoded)
	}
	httpJSON(t, client, server.URL, prod, "POST", "/v1/collaboration/context.revoke", model.CollaborationRequest{ContextID: c.ID, AgentID: "opencode:reader"}, nil, 200)
	if rpc(reader, "resources/read", map[string]any{"uri": uri})["error"] == nil {
		t.Fatal("revoked resource accessible")
	}
	var audit []map[string]any
	httpJSON(t, client, server.URL, prod, "POST", "/v1/collaboration/context.audit", model.CollaborationRequest{ContextID: c.ID}, &audit, 200)
	denials := 0
	for _, row := range audit {
		if row["outcome"] == "denied" {
			denials++
		}
	}
	if denials != 5 {
		t.Fatalf("denied access audit count %d", denials)
	}
}
