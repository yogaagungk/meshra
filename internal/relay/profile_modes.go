package relay

import (
	"encoding/json"
	"fmt"
	"slices"

	"meshra/internal/model"
)

// Execution modes separate two trust domains.
//
//	restricted: only the admitted broker catalog; may serve external or unattended work.
//	trusted:    operator-selected native full access; never serves external or unattended work.
//
// A profile saved before modes existed has no mode. It is reported as trusted
// and unreviewed, never silently reinterpreted as restricted.
const blockedModeReason = "target profile is not in restricted execution mode; review the profile (restricted mode) before enabling external or unattended work"

// restrictedNativeBlocked reports why a harness cannot run restricted work.
// Direct API has no native tools: every authority flows through the broker.
// Claude and OMP need verified native-tool suppression plus OS confinement of
// the harness process itself. Until a confined launcher has been proven for
// the installed version the answer is "unavailable"; there is no host fallback.
func (r *Relay) restrictedNativeBlocked(harness string) error {
	if harness == "openai" {
		return nil
	}
	ad, ok := r.adapters[harness]
	if ok && ad.Capabilities["restrictedConfinement"] {
		return nil
	}
	if harness == "claude" {
		// Real confinement: evidence comes from the live OS probe, not a flag.
		return r.confinedClaudeReadyLocked()
	}
	if harness == "omp" {
		return r.confinedOMPReadyLocked()
	}
	return model.Err(503, fmt.Sprintf("restricted execution is unavailable for %s: confinement of the native harness process has not been proven on this installation (no host fallback); use a Direct API profile or trusted mode with an explicit acknowledgment", harness))
}

// enforceModeAdmission is the single admission gate for execution authority.
// It runs for every profile-managed destination, whatever API reached it.
func (r *Relay) enforceModeAdmission(p model.Principal, ingress bool, destination model.Agent, snapshot model.ProfileTaskSnapshot) error {
	current, err := r.profile(snapshot.Profile.ID)
	if err != nil {
		return err
	}
	if snapshot.ExecutionMode != current.EffectiveMode() {
		return model.Err(409, "profile execution mode changed before admission")
	}
	unattended := ingress || snapshot.External
	if unattended && !current.Restricted() {
		return model.Err(403, "blocked: "+blockedModeReason)
	}
	if current.Restricted() {
		if err = r.restrictedNativeBlocked(snapshot.Binding.Harness); err != nil {
			return err
		}
	}
	if p.TaskID != "" {
		// Nested delegation never escapes a restricted or external origin.
		parent, e := r.task(p.TaskID)
		if e != nil {
			return model.Err(401, "authenticated turn is no longer active")
		}
		origin, e := r.profileSnapshot(parent)
		if e == nil && (origin.ExecutionMode == model.ModeRestricted || origin.External || origin.ExecutionMode == "") && !current.Restricted() {
			return model.Err(403, "delegation from a restricted or external task cannot reach a trusted profile")
		}
	}
	_ = destination
	return nil
}

// checkModeAtDispatch re-validates an already queued task. Work admitted under
// earlier authority is blocked for review, never silently reauthorized.
func (r *Relay) checkModeAtDispatch(t model.Task) error {
	snapshot, err := r.profileSnapshot(t)
	if err != nil {
		return err
	}
	if snapshot.External && snapshot.ExecutionMode != model.ModeRestricted {
		return model.Err(403, "blocked for review: this task was admitted before execution modes existed or under trusted authority; switch the profile to restricted mode and resubmit the work")
	}
	current, err := r.profile(snapshot.Profile.ID)
	if err != nil {
		return err
	}
	if current.EffectiveMode() != snapshot.ExecutionMode {
		return model.Err(409, "blocked for review: profile execution mode changed after admission")
	}
	return nil
}

func (r *Relay) restrictedProfileIDs(ids []string) (string, error) {
	for _, id := range ids {
		p, err := r.profile(id)
		if err != nil {
			return "", err
		}
		if !p.Restricted() {
			return id, nil
		}
	}
	return "", nil
}

func (r *Relay) routineProfileIDs(d routineDefinition) []string {
	ids := []string{d.ProfileID}
	if d.FlowID != "" {
		if flow, err := r.loadRoutineFlow(d.FlowID, d.FlowRevision); err == nil {
			for _, n := range flow.Nodes {
				if !slices.Contains(ids, n.ProfileID) {
					ids = append(ids, n.ProfileID)
				}
			}
		}
	}
	return ids
}

// requireRestrictedProfiles rejects enabling unattended work on unrestricted targets.
func (r *Relay) requireRestrictedProfiles(ids []string) error {
	bad, err := r.restrictedProfileIDs(ids)
	if err != nil {
		return err
	}
	if bad != "" {
		return model.Err(409, blockedModeReason+" ("+bad+")")
	}
	return nil
}

// reviewUnsafeAutomation is the startup migration. It disables enabled routes and
// routines whose destination is not restricted, records an actionable reason and
// keeps every accepted event, task and run. It is idempotent.
func (r *Relay) reviewUnsafeAutomation() error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type route struct {
		id, body, profile string
		revision          int
	}
	rows, err := tx.Query("SELECT id,body,profile_id,revision FROM connector_routes")
	if err != nil {
		return err
	}
	var routes []route
	for rows.Next() {
		var v route
		if err = rows.Scan(&v.id, &v.body, &v.profile, &v.revision); err != nil {
			rows.Close()
			return err
		}
		routes = append(routes, v)
	}
	rows.Close()
	audit := 0
	for _, v := range routes {
		var c connectorRoute
		if json.Unmarshal([]byte(v.body), &c) != nil || !c.Enabled {
			continue
		}
		var body string
		if tx.QueryRow("SELECT body FROM agent_profiles WHERE id=?", v.profile).Scan(&body) != nil {
			continue
		}
		var prof model.Profile
		if json.Unmarshal([]byte(body), &prof) != nil || prof.Restricted() {
			continue
		}
		c.Enabled, c.BlockedReason, c.Revision = false, blockedModeReason, c.Revision+1
		out, _ := json.Marshal(c)
		if _, err = tx.Exec("UPDATE connector_routes SET revision=?,body=? WHERE id=?", c.Revision, string(out), v.id); err != nil {
			return err
		}
		if err = insertAutomationEvent(tx, "modes.routeBlocked", v.id, v.profile, map[string]any{"reason": "unrestricted destination"}); err != nil {
			return err
		}
		audit++
	}
	rrows, err := tx.Query("SELECT id,body FROM routines WHERE enabled=1")
	if err != nil {
		return err
	}
	type rt struct{ id, body string }
	var rts []rt
	for rrows.Next() {
		var v rt
		if err = rrows.Scan(&v.id, &v.body); err != nil {
			rrows.Close()
			return err
		}
		rts = append(rts, v)
	}
	rrows.Close()
	for _, v := range rts {
		var d routineDefinition
		if json.Unmarshal([]byte(v.body), &d) != nil {
			continue
		}
		unsafe := false
		for _, id := range r.routineProfileIDs(d) {
			var body string
			var prof model.Profile
			if tx.QueryRow("SELECT body FROM agent_profiles WHERE id=?", id).Scan(&body) == nil && json.Unmarshal([]byte(body), &prof) == nil && !prof.Restricted() {
				unsafe = true
			}
		}
		if !unsafe {
			continue
		}
		d.Enabled, d.BlockedReason, d.Revision = false, blockedModeReason, d.Revision+1
		out, _ := json.Marshal(d)
		if _, err = tx.Exec("UPDATE routines SET revision=?,body=?,enabled=0,next_due_at='' WHERE id=?", d.Revision, string(out), v.id); err != nil {
			return err
		}
		if err = insertAutomationEvent(tx, "modes.routineBlocked", v.id, d.ProfileID, map[string]any{"reason": "unrestricted destination"}); err != nil {
			return err
		}
		audit++
	}
	if audit == 0 {
		return nil
	}
	return tx.Commit()
}
