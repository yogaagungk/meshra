package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"meshra/internal/model"
	"meshra/internal/netpolicy"
)

// Enabling or changing an enabled endpoint reviews only this exact public
// origin. Literal local/LAN endpoints still need a separate private-access
// confirmation. DNS-pinned connect checks remain authoritative for hostnames.
func insertEndpointOriginGrantTx(ctx context.Context, tx *sql.Tx, current, previous ProviderEndpoint, found bool) error {
	if !current.Enabled {
		return nil
	}
	origin, err := EndpointOriginOf(current)
	if err != nil {
		return err
	}
	if localOrLANHost(origin.Host) {
		return nil
	}
	if found && previous.Enabled {
		old, err := EndpointOriginOf(previous)
		if err != nil {
			return err
		}
		if old == origin {
			return nil
		} // A metadata edit must not undo network.revoke.
	}
	scope := netpolicy.Scope{ConnectionID: current.ID}
	old, err := readNetworkGrant(ctx, tx, scope, origin.String())
	creating := errors.Is(err, sql.ErrNoRows)
	if err != nil && !creating {
		return err
	}
	if !creating && old.Enabled {
		return nil
	}
	grant := netpolicy.Grant{ID: old.ID, Revision: old.Revision + 1, Scope: scope, Origin: origin.String(), Enabled: true}
	if creating {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM network_grants").Scan(&count); err != nil {
			return err
		}
		if count >= 4096 {
			return model.Err(429, "network grant capacity reached")
		}
		grant.ID = randomID()
	}
	body, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	if creating {
		_, err = tx.ExecContext(ctx, "INSERT INTO network_grants(id,revision,profile_id,connection_id,origin,enabled,body) VALUES(?,?,NULL,?,?,1,?)", grant.ID, grant.Revision, current.ID, grant.Origin, string(body))
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE network_grants SET revision=?,enabled=1,body=? WHERE id=? AND revision=?", grant.Revision, string(body), grant.ID, old.Revision)
	}
	if err != nil {
		return err
	}
	return insertAutomationEvent(tx, "network.granted", grant.ID, "", map[string]any{"revision": grant.Revision, "connectionId": grant.ConnectionID, "origin": grant.Origin, "allowPrivate": false})
}
