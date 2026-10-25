package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meshra/internal/model"
	"meshra/internal/netpolicy"
)

func networkCall(t *testing.T, r *Relay, principal model.Principal, operation string, payload any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil { t.Fatal(err) }
	return r.networkOperation(context.Background(), principal, operation, raw)
}
func networkProfile(t *testing.T, r *Relay) model.Profile {
	t.Helper()
	return profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
}

func TestNetworkGrantOperatorExactRevisionAndSelfListener(t *testing.T) {
	r, _ := openTest(t, nil)
	r.SetMCPURL("http://127.0.0.1:37731/mcp")
	principal := admin
	profile := networkProfile(t, r)
	q := map[string]any{"profileId": profile.ID, "origin": "http://127.0.0.1:8080"}
	_, err := networkCall(t, r, model.Principal{AgentID: "omp:unprivileged"}, "network.grant", q)
	requireStatus(t, err, 403)
	_, err = networkCall(t, r, principal, "network.grant", q)
	requireStatus(t, err, 400)
	q["allowPrivate"] = true
	value, err := networkCall(t, r, principal, "network.grant", q)
	if err != nil { t.Fatal(err) }
	grant := value.(netpolicy.Grant)
	if grant.Revision != 1 || !grant.AllowPrivate || !grant.Enabled { t.Fatalf("wrong grant: %+v", grant) }
	_, err = networkCall(t, r, principal, "network.grant", q)
	requireStatus(t, err, 409)
	q["expectedRevision"] = grant.Revision
	value, err = networkCall(t, r, principal, "network.grant", q)
	if err != nil || value.(netpolicy.Grant).Revision != 2 { t.Fatalf("grant revision: %v %v", value, err) }
	_, err = networkCall(t, r, principal, "network.revoke", map[string]any{"id": grant.ID, "expectedRevision": grant.Revision})
	requireStatus(t, err, 409)
	value, err = networkCall(t, r, principal, "network.revoke", map[string]any{"id": grant.ID, "expectedRevision": 2})
	if err != nil || value.(netpolicy.Grant).Enabled || value.(netpolicy.Grant).Revision != 3 { t.Fatalf("revoke: %v %v", value, err) }
	q["origin"], q["expectedRevision"] = "http://[::ffff:127.0.0.1]:37731", nil
	_, err = networkCall(t, r, principal, "network.grant", q)
	requireStatus(t, err, 400)
}

func TestNetworkSchemasAreStrictAndNoGlobalGrant(t *testing.T) {
	r, _ := openTest(t, nil)
	r.SetMCPURL("http://127.0.0.1:37731/mcp")
	principal := admin
	profile := networkProfile(t, r)
	for _, payload := range []map[string]any{
		{"origin": "http://127.0.0.1:8080", "allowPrivate": true},
		{"profileId": profile.ID, "origin": "http://user:pass@127.0.0.1:8080", "allowPrivate": true},
		{"profileId": profile.ID, "origin": "http://127.0.0.1:8080?token=secret", "allowPrivate": true},
		{"profileId": profile.ID, "origin": "http://127.0.0.1:8080#fragment", "allowPrivate": true},
		{"profileId": profile.ID, "origin": "http://127.0.0.1:8080", "allowPrivate": true, "enabled": true},
		{"profileId": profile.ID, "origin": "http://127.0.0.1:8080", "allowPrivate": true, "expectedRevision": 0},
	} {
		_, err := networkCall(t, r, principal, "network.grant", payload)
		requireStatus(t, err, 400)
	}
	_, err := networkCall(t, r, principal, "network.list", map[string]any{"allowPrivate": true})
	requireStatus(t, err, 400)
	_, err = networkCall(t, r, model.Principal{AgentID: "codex:worker"}, "network.list", map[string]any{})
	requireStatus(t, err, 403)
	value, err := networkCall(t, r, admin, "network.list", map[string]any{})
	if err != nil || len(value.([]netpolicy.Grant)) != 0 { t.Fatalf("invalid request created grants: %v %v", value, err) }
}

func TestNetworkGrantEventFailureRollsBack(t *testing.T) {
	r, _ := openTest(t, nil)
	r.SetMCPURL("http://127.0.0.1:37731/mcp")
	principal := admin
	profile := networkProfile(t, r)
	if _, err := r.db.Exec("CREATE TEMP TRIGGER reject_network_event BEFORE INSERT ON automation_events BEGIN SELECT RAISE(ABORT,'event unavailable'); END"); err != nil { t.Fatal(err) }
	_, err := networkCall(t, r, principal, "network.grant", map[string]any{"profileId": profile.ID, "origin": "http://127.0.0.1:8080", "allowPrivate": true})
	if err == nil { t.Fatal("event failure reported success") }
	var count int
	if err = r.db.QueryRow("SELECT count(*) FROM network_grants").Scan(&count); err != nil || count != 0 { t.Fatalf("uncommitted grant remained: %d %v", count, err) }
}

func TestRelayNetworkClientRevokeAndConnectionEdit(t *testing.T) {
	r, _ := openTest(t, nil)
	r.SetMCPURL("http://127.0.0.1:37731/mcp")
	principal := admin
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { _, _ = io.WriteString(w, "real local response") }))
	defer server.Close()
	connectionID := strings.Repeat("a", 32)
	if _, err := r.db.Exec("INSERT INTO provider_endpoints(id,revision,body) VALUES(?,1,'{}')", connectionID); err != nil { t.Fatal(err) }
	value, err := networkCall(t, r, principal, "network.grant", map[string]any{"connectionId": connectionID, "origin": server.URL, "allowPrivate": true})
	if err != nil { t.Fatal(err) }
	grant := value.(netpolicy.Grant)
	scope := netpolicy.Scope{ConnectionID: connectionID}
	client, err := r.networkClient(scope, netpolicy.ClientOptions{Origin: server.URL, CredentialOrigin: server.URL})
	if err != nil { t.Fatal(err) }
	response, err := client.Get(server.URL)
	if err != nil { t.Fatal(err) }
	body, err := io.ReadAll(response.Body); response.Body.Close()
	if err != nil || string(body) != "real local response" { t.Fatalf("HTTP body=%q err=%v", body, err) }
	r.mu.Lock()
	tx, err := r.db.Begin()
	if err != nil { r.mu.Unlock(); t.Fatal(err) }
	grants, err := invalidateConnectionNetworkLocked(context.Background(), tx, connectionID)
	if err == nil { err = tx.Commit() } else { _ = tx.Rollback() }
	if err == nil { r.closeRevokedNetworkGrantsLocked(grants) }
	r.mu.Unlock()
	if err != nil { t.Fatal(err) }
	if len(grants) != 1 || grants[0].ID != grant.ID { t.Fatalf("invalidated grants=%+v", grants) }
	if response, err := client.Get(server.URL); err == nil { response.Body.Close(); t.Fatal("connection edit retained old client authority") }
	listed, err := networkCall(t, r, admin, "network.list", map[string]any{"connectionId": connectionID})
	if err != nil { t.Fatal(err) }
	current := listed.([]netpolicy.Grant)
	if len(current) != 1 || current[0].Enabled || current[0].Revision != 2 { t.Fatalf("grant was not invalidated atomically: %+v", current) }
}
