package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
	"meshra/internal/netpolicy"
)

// networkPolicyLocked is called only with relay ownership. Authorization lookup
// itself takes relay ownership and is never called while policy ownership is held.
func (r *Relay) networkPolicyLocked() *netpolicy.Policy {
	if r.networkPolicy == nil {
		r.networkPolicy = netpolicy.New(r.lookupNetworkGrant, netpolicy.Options{})
		if r.mcpURL != "" { _ = r.networkPolicy.SetListener(r.mcpURL) }
	}
	return r.networkPolicy
}

func (r *Relay) setNetworkListenerLocked(raw string) {
	_ = r.networkPolicyLocked().SetListener(raw)
}
func (r *Relay) closeNetworkPolicy() {
	r.mu.Lock()
	policy := r.networkPolicy
	r.mu.Unlock()
	if policy != nil { _ = policy.Close() }
}

func (r *Relay) lookupNetworkGrant(ctx context.Context, scope netpolicy.Scope, origin string) (netpolicy.Grant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed { return netpolicy.Grant{}, netpolicy.ErrRevoked }
	grant, err := readNetworkGrant(ctx, r.db, scope, origin)
	if errors.Is(err, sql.ErrNoRows) { return netpolicy.Grant{}, netpolicy.ErrDenied }
	return grant, err
}

type networkRowReader interface { QueryRowContext(context.Context, string, ...any) *sql.Row }
func readNetworkGrant(ctx context.Context, reader networkRowReader, scope netpolicy.Scope, origin string) (netpolicy.Grant, error) {
	var grant netpolicy.Grant
	var id, body string
	var revision int
	var enabled bool
	err := reader.QueryRowContext(ctx, "SELECT id,revision,enabled,body FROM network_grants WHERE coalesce(profile_id,'')=? AND coalesce(connection_id,'')=? AND origin=?", scope.ProfileID, scope.ConnectionID, origin).Scan(&id, &revision, &enabled, &body)
	if err != nil { return grant, err }
	if err = json.Unmarshal([]byte(body), &grant); err != nil { return grant, err }
	if grant.ID != id || grant.Revision != revision || grant.Scope != scope || grant.Origin != origin || grant.Enabled != enabled { return netpolicy.Grant{}, errors.New("network grant metadata mismatch") }
	return grant, nil
}

// networkClient is the daemon-side connection entry point. Origin and credential
// origin come from an admitted configuration, never from model identity fields.
func (r *Relay) networkClient(scope netpolicy.Scope, options netpolicy.ClientOptions) (*http.Client, error) {
	r.mu.Lock()
	if r.closed { r.mu.Unlock(); return nil, model.Err(503, "relay is stopping") }
	policy := r.networkPolicyLocked()
	r.mu.Unlock()
	return policy.Client(scope, options)
}

func (r *Relay) networkProxy(ctx context.Context, path string, scope netpolicy.Scope, options netpolicy.ProxyOptions) (*netpolicy.Proxy, error) {
	r.mu.Lock()
	if r.closed { r.mu.Unlock(); return nil, model.Err(503, "relay is stopping") }
	policy := r.networkPolicyLocked()
	r.mu.Unlock()
	return policy.ServeUnix(ctx, path, scope, options)
}

// invalidateConnectionNetworkLocked belongs in connection settings edits and
// secret revocation. It disables every exact grant for the old configuration in
// the caller's transaction; stream cancellation follows commit via the result.
// No database call through r.db may occur while this transaction is open.
func invalidateConnectionNetworkLocked(ctx context.Context, tx *sql.Tx, connectionID string) ([]netpolicy.Grant, error) {
	rows, err := tx.QueryContext(ctx, "SELECT body FROM network_grants WHERE connection_id=? AND enabled=1", connectionID)
	if err != nil { return nil, err }
	var grants []netpolicy.Grant
	for rows.Next() {
		var body string
		var grant netpolicy.Grant
		if err = rows.Scan(&body); err == nil { err = json.Unmarshal([]byte(body), &grant) }
		if err != nil { _ = rows.Close(); return nil, err }
		grants = append(grants, grant)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil { return nil, err }
	for _, previous := range grants {
		updated := previous
		updated.Revision++
		updated.Enabled = false
		body, e := json.Marshal(updated)
		if e != nil { return nil, e }
		if _, err = tx.ExecContext(ctx, "UPDATE network_grants SET revision=?,enabled=0,body=? WHERE id=? AND revision=?", updated.Revision, string(body), updated.ID, previous.Revision); err != nil { return nil, err }
		if err = insertAutomationEvent(tx, "network.revoked", updated.ID, updated.ProfileID, map[string]any{"revision": updated.Revision, "connectionId": connectionID}); err != nil { return nil, err }
	}
	return grants, nil
}

// Call after commit and before releasing r.mu. No callbacks occur under the
// policy mutex, so this cannot invert the grant lookup's relay lock order.
func (r *Relay) closeRevokedNetworkGrantsLocked(grants []netpolicy.Grant) {
	if r.networkPolicy != nil { for _, grant := range grants { r.networkPolicy.Revoke(grant.ID, grant.Revision) } }
}

func (r *Relay) networkOperation(ctx context.Context, principal model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := requireAdmin(principal); err != nil { return nil, err }
	if !utf8.Valid(raw) || len(raw) > 64 << 10 { return nil, model.Err(400, "invalid network payload") }
	if operation == "network.list" {
		var q struct { ProfileID string `json:"profileId"`; ConnectionID string `json:"connectionId"` }
		if err := decodeAutomation(raw, &q); err != nil { return nil, err }
		if q.ProfileID != "" && !profileIdentifier(q.ProfileID) || q.ConnectionID != "" && !profileIdentifier(q.ConnectionID) { return nil, model.Err(400, "invalid network scope") }
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed { return nil, model.Err(503, "relay is stopping") }
		rows, err := r.db.QueryContext(ctx, "SELECT body FROM network_grants WHERE (?='' OR profile_id=?) AND (?='' OR connection_id=?) ORDER BY id LIMIT 4097", q.ProfileID, q.ProfileID, q.ConnectionID, q.ConnectionID)
		if err != nil { return nil, err }
		defer rows.Close()
		grants := []netpolicy.Grant{}
		for rows.Next() {
			var body string
			var grant netpolicy.Grant
			if err = rows.Scan(&body); err == nil { err = json.Unmarshal([]byte(body), &grant) }
			if err != nil { return nil, err }
			if len(grants) == 4096 { return nil, model.Err(429, "network grant capacity exceeded") }
			grants = append(grants, grant)
		}
		return grants, rows.Err()
	}
	if operation != "network.grant" && operation != "network.revoke" { return nil, model.Err(404, "unknown network operation") }
	if operation == "network.revoke" { return r.revokeNetworkGrant(ctx, raw) }
	var q struct {
		ProfileID string `json:"profileId"`
		ConnectionID string `json:"connectionId"`
		Origin string `json:"origin"`
		AllowPrivate bool `json:"allowPrivate"`
		ExpectedRevision *int `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &q); err != nil { return nil, err }
	scope := netpolicy.Scope{ProfileID: q.ProfileID, ConnectionID: q.ConnectionID}
	if scope.ProfileID == "" && scope.ConnectionID == "" || q.ProfileID != "" && !profileIdentifier(q.ProfileID) || q.ConnectionID != "" && !profileIdentifier(q.ConnectionID) || len(q.Origin) > 2048 { return nil, model.Err(400, "an exact profile or connection scope and origin are required") }
	if q.ExpectedRevision != nil && *q.ExpectedRevision < 1 { return nil, model.Err(400, "expectedRevision must be positive") }
	r.mu.Lock()
	policy := r.networkPolicyLocked()
	r.mu.Unlock()
	validationCtx, cancel := context.WithTimeout(ctx, networkValidationTimeout)
	origin, err := policy.ValidateGrant(validationCtx, q.Origin, q.AllowPrivate)
	cancel()
	if err != nil { return nil, model.Err(400, "origin is invalid, private without confirmation, self-listener, or DNS unavailable") }
	// DNS resolution occurs outside relay ownership.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed { return nil, model.Err(503, "relay is stopping") }
	if q.ProfileID != "" {
		if _, err = r.profile(q.ProfileID); err != nil { return nil, err }
	}
	if q.ConnectionID != "" {
		var count int
		if err = r.db.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM provider_endpoints WHERE id=?)+(SELECT count(*) FROM acp_agents WHERE id=?)", q.ConnectionID, q.ConnectionID).Scan(&count); err != nil { return nil, err }
		if count != 1 { return nil, model.Err(404, "connection not found") }
	}
	previous, err := readNetworkGrant(ctx, r.db, scope, origin)
	if err != nil && !errors.Is(err, sql.ErrNoRows) { return nil, err }
	creating := errors.Is(err, sql.ErrNoRows)
	if creating && q.ExpectedRevision != nil || !creating && (q.ExpectedRevision == nil || *q.ExpectedRevision != previous.Revision) { return nil, model.Err(409, "network grant revision changed") }
	grant := netpolicy.Grant{ID: previous.ID, Revision: previous.Revision+1, Scope: scope, Origin: origin, AllowPrivate: q.AllowPrivate, Enabled: true}
	if creating {
		var count int
		if err = r.db.QueryRowContext(ctx, "SELECT count(*) FROM network_grants").Scan(&count); err != nil { return nil, err }
		if count >= 4096 { return nil, model.Err(429, "network grant capacity reached") }
		grant.ID = randomID()
	}
	body, err := json.Marshal(grant)
	if err != nil { return nil, err }
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil { return nil, err }
	defer tx.Rollback()
	if creating {
		_, err = tx.ExecContext(ctx, "INSERT INTO network_grants(id,revision,profile_id,connection_id,origin,enabled,body) VALUES(?,?,nullif(?,''),nullif(?,''),?,1,?)", grant.ID, grant.Revision, q.ProfileID, q.ConnectionID, origin, string(body))
	} else { _, err = tx.ExecContext(ctx, "UPDATE network_grants SET revision=?,enabled=1,body=? WHERE id=? AND revision=?", grant.Revision, string(body), grant.ID, previous.Revision) }
	if err != nil { return nil, err }
	if err = insertAutomationEvent(tx, "network.granted", grant.ID, grant.ProfileID, map[string]any{"revision": grant.Revision, "connectionId": grant.ConnectionID, "origin": grant.Origin, "allowPrivate": grant.AllowPrivate}); err != nil { return nil, err }
	if err = tx.Commit(); err != nil { return nil, err }
	if !creating { policy.Revoke(previous.ID, previous.Revision) }
	return grant, nil
}

const networkValidationTimeout = 5 * time.Second

func (r *Relay) revokeNetworkGrant(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct { ID string `json:"id"`; ExpectedRevision int `json:"expectedRevision"` }
	if err := decodeAutomation(raw, &q); err != nil { return nil, err }
	if !profileIdentifier(q.ID) || q.ExpectedRevision < 1 { return nil, model.Err(400, "grant id and expectedRevision are required") }
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed { return nil, model.Err(503, "relay is stopping") }
	var body string
	err := r.db.QueryRowContext(ctx, "SELECT body FROM network_grants WHERE id=?", q.ID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) { return nil, model.Err(404, "network grant not found") }
	if err != nil { return nil, err }
	var grant netpolicy.Grant
	if err = json.Unmarshal([]byte(body), &grant); err != nil { return nil, err }
	if grant.Revision != q.ExpectedRevision { return nil, model.Err(409, "network grant revision changed") }
	previousRevision := grant.Revision
	grant.Revision++
	grant.Enabled = false
	encoded, err := json.Marshal(grant)
	if err != nil { return nil, err }
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil { return nil, err }
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE network_grants SET revision=?,enabled=0,body=? WHERE id=? AND revision=?", grant.Revision, string(encoded), grant.ID, previousRevision); err != nil { return nil, err }
	if err = insertAutomationEvent(tx, "network.revoked", grant.ID, grant.ProfileID, map[string]any{"revision": grant.Revision, "connectionId": grant.ConnectionID}); err != nil { return nil, err }
	if err = tx.Commit(); err != nil { return nil, err }
	if r.networkPolicy != nil { r.networkPolicy.Revoke(grant.ID, previousRevision) }
	return grant, nil
}
