package relay

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"meshra/internal/model"
	"meshra/internal/sandbox"
)

// These helpers run under r.mu; profile storage never creates execution identities.
func (r *Relay) profile(id string) (model.Profile, error) {
	var v model.Profile
	var body string
	if err := r.db.QueryRow("SELECT body FROM agent_profiles WHERE id=?", id).Scan(&body); err != nil {
		if err == sql.ErrNoRows {
			return v, model.Err(404, "profile not found")
		}
		return v, err
	}
	err := json.Unmarshal([]byte(body), &v)
	return v, err
}

func profileIdentifier(id string) bool {
	return id != "" && len(id) <= 200 && safeRegistryText(id) && redact(id) == id
}

func profileExecution(c model.ProfileCapabilities) bool {
	return c.WorkspaceWrite || c.Command || c.Browser || c.Computer || c.ExternalMCP
}

func (r *Relay) validateProfile(v model.Profile, creating bool) error {
	if strings.TrimSpace(v.Name) == "" || len(v.Name) > 128 || !safeRegistryText(v.Name) || redact(v.Name) != v.Name || strings.Contains(v.Name, "-----BEGIN ") {
		return model.Err(400, "profile name must be 1..128 UTF-8 bytes without credentials or control characters")
	}
	if len(v.Instructions) > 16*1024 {
		return model.Err(413, "profile instructions exceed 16384 bytes")
	}
	if !utf8.ValidString(v.Instructions) || redact(v.Instructions) != v.Instructions || strings.Contains(v.Instructions, "-----BEGIN ") {
		return model.Err(400, "profile instructions must be UTF-8 without detected credentials or private keys")
	}
	for _, ch := range v.Instructions {
		if unicode.IsControl(ch) && ch != '\n' && ch != '\r' && ch != '\t' {
			return model.Err(400, "profile instructions contain control characters")
		}
	}
	if !profileIdentifier(v.WorkspaceID) {
		return model.Err(400, "workspaceId required")
	}
	if _, err := r.workspace(v.WorkspaceID); err != nil {
		return err
	}
	switch v.Binding.Harness {
	case "codex", "claude", "omp", "opencode", "openai", "acp":
	default:
		return model.Err(400, "unsupported profile harness")
	}
	if err := validConfig(v.Binding.Config, v.Binding.Harness); err != nil {
		return err
	}
	if err := r.validateProfileConnectionLocked(v.Binding, false); err != nil {
		return err
	}
	if v.AvatarAttachmentID != "" {
		if err := r.validateProfileAvatar(v.AvatarAttachmentID); err != nil {
			return err
		}
	}
	if len(v.AllowedProfileIDs) > 64 {
		return model.Err(400, "at most 64 allowed profiles")
	}
	seen := make(map[string]bool, len(v.AllowedProfileIDs))
	for _, id := range v.AllowedProfileIDs {
		if !profileIdentifier(id) || id == v.ID || seen[id] {
			return model.Err(400, "allowedProfileIds must contain unique other profile identifiers")
		}
		seen[id] = true
		other, err := r.profile(id)
		if err != nil {
			return err
		}
		if other.Archived {
			return model.Err(409, "allowed profile is archived")
		}
	}
	if creating && profileExecution(v.Capabilities) {
		return model.Err(400, "execution and external capabilities must start disabled")
	}
	if v.Capabilities.Browser || v.Capabilities.Computer {
		return model.Err(503, "requested external tool backend is not configured")
	}
	if v.Capabilities.WorkspaceWrite || v.Capabilities.Command {
		config, revision, err := r.runtimeConfigLocked()
		if err != nil {
			return err
		}
		if revision == 0 || config.Executables["bwrap"] == "" {
			return model.Err(503, "configure the confined runtime before enabling writes or commands")
		}
		if err = sandbox.Validate(config); err != nil {
			return model.Err(503, "configured confined runtime manifest is unavailable or changed")
		}
	}
	return nil
}

func (r *Relay) profileBusy(id string) error {
	var busy bool
	err := r.db.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM tasks t WHERE t.state NOT IN ('completed','failed','cancelled','timed_out')
 AND (EXISTS(SELECT 1 FROM profile_task_snapshots s WHERE s.task_id=t.id AND s.profile_id=?)
 OR EXISTS(SELECT 1 FROM profile_bindings b WHERE b.agent_id=t.recipient AND b.profile_id=?)))
 OR EXISTS(SELECT 1 FROM routine_steps s JOIN profile_bindings b ON b.id=s.binding_id JOIN routine_runs rr ON rr.id=s.run_id
 WHERE b.profile_id=? AND s.state='reserved' AND rr.state IN ('claimed','running'))
 OR EXISTS(SELECT 1 FROM tool_approvals a JOIN tool_invocations i ON i.id=a.invocation_id
 WHERE json_extract(i.body,'$.profileId')=? AND a.status='pending')`, id, id, id, id).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return model.Err(409, "profile has outstanding work")
	}
	return nil
}

func profilePolicyWidens(old, next model.Profile) bool {
	a, b := old.Capabilities, next.Capabilities
	if (!a.WorkspaceRead && b.WorkspaceRead) || (!a.WorkspaceWrite && b.WorkspaceWrite) ||
		(!a.Command && b.Command) || (!a.Browser && b.Browser) || (!a.Computer && b.Computer) ||
		(!a.MemoryWrite && b.MemoryWrite) || (!a.ExternalMCP && b.ExternalMCP) || (!a.Attachments && b.Attachments) {
		return true
	}
	for _, id := range next.AllowedProfileIDs {
		if !contains(old.AllowedProfileIDs, id) {
			return true
		}
	}
	return false
}

// profilePolicyNarrows reports a capability or grant reduction. Narrowing never
// needs a quiescent profile, but it invalidates the admitted catalog of every
// outstanding turn.
func profilePolicyNarrows(old, next model.Profile) bool {
	if profilePolicyWidens(old, next) {
		return false
	}
	a, b := old.Capabilities, next.Capabilities
	if (a.WorkspaceRead && !b.WorkspaceRead) || (a.WorkspaceWrite && !b.WorkspaceWrite) ||
		(a.Command && !b.Command) || (a.Browser && !b.Browser) || (a.Computer && !b.Computer) ||
		(a.MemoryWrite && !b.MemoryWrite) || (a.ExternalMCP && !b.ExternalMCP) || (a.Attachments && !b.Attachments) {
		return true
	}
	for _, id := range old.AllowedProfileIDs {
		if !contains(next.AllowedProfileIDs, id) {
			return true
		}
	}
	return false
}

func (r *Relay) storeProfile(v model.Profile, expectedRevision int, kind string, bindings []profileStoreBinding, reconcile bool) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if expectedRevision == 0 {
		_, err = tx.Exec("INSERT INTO agent_profiles(id,revision,body) VALUES(?,?,?)", v.ID, v.Revision, string(body))
	} else {
		var result sql.Result
		result, err = tx.Exec("UPDATE agent_profiles SET revision=?,body=? WHERE id=? AND revision=?", v.Revision, string(body), v.ID, expectedRevision)
		if err == nil {
			var count int64
			count, err = result.RowsAffected()
			if err == nil && count != 1 {
				err = model.Err(409, "profile revision changed")
			}
		}
	}
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		body, err := json.Marshal(binding.Agent)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE agents SET body=? WHERE id=?", string(body), binding.Agent.ID); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM credentials WHERE agent_id=?", binding.Agent.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO profile_context_epochs(binding_id,epoch,reset_after_seq) VALUES(?,1,0)
 ON CONFLICT(binding_id) DO UPDATE SET epoch=epoch+1`, binding.ID); err != nil {
			return err
		}
	}
	if reconcile {
		if err = r.syncProfilePolicies(tx); err != nil {
			return err
		}
	}
	if err = insertAutomationEvent(tx, kind, v.ID, v.ID, map[string]any{"revision": v.Revision, "archived": v.Archived}); err != nil {
		return err
	}
	return tx.Commit()
}

type profileStoreBinding struct {
	ID    string
	Agent model.Agent
}

// The lifecycle lock prevents new admissions while idle native sessions are
// closed. Keep r.mu out of runtime teardown and retain each canonical identity.
func (r *Relay) prepareProfileStoreBindings(v model.Profile) ([]profileStoreBinding, error) {
	w, err := r.workspace(v.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if err = validateWorkspace(model.Agent{Cwd: w.Path}); err != nil {
		return nil, err
	}
	rows, err := r.db.Query(`SELECT b.id,b.agent_id FROM profile_bindings b WHERE b.profile_id=?
 AND b.generation=(SELECT max(n.generation) FROM profile_bindings n WHERE n.profile_id=b.profile_id AND n.conversation_id=b.conversation_id)`, v.ID)
	if err != nil {
		return nil, err
	}
	bindings := []profileStoreBinding{}
	for rows.Next() {
		var b profileStoreBinding
		if err = rows.Scan(&b.ID, &b.Agent.ID); err != nil {
			rows.Close()
			return nil, err
		}
		bindings = append(bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range bindings {
		id := bindings[i].Agent.ID
		a, err := r.agent(id)
		if err != nil {
			return nil, err
		}
		if !a.ProfileManaged {
			return nil, model.Err(409, "profile binding does not own its execution identity")
		}
		r.mu.Unlock()
		err = r.stop(id)
		r.mu.Lock()
		if err != nil {
			return nil, err
		}
		a, err = r.agent(id)
		if err != nil {
			return nil, err
		}
		a.Cwd, a.SessionID, a.LastSeen = w.Path, "", now()
		if err = validateWorkspace(a); err != nil {
			return nil, err
		}
		bindings[i].Agent = a
	}
	return bindings, nil
}

func (r *Relay) profileStoreOperation(p model.Principal, op string, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) {
		return nil, model.Err(400, "payload must be valid UTF-8")
	}
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	switch op {
	case "profiles.list":
		var q struct {
			After string `json:"after"`
			Limit int    `json:"limit"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if q.After != "" && !profileIdentifier(q.After) {
			return nil, model.Err(400, "invalid profile cursor")
		}
		if q.Limit == 0 {
			q.Limit = 50
		}
		if q.Limit < 1 || q.Limit > 100 {
			return nil, model.Err(400, "limit must be 1..100")
		}
		rows, err := r.db.Query("SELECT body FROM agent_profiles WHERE id>? ORDER BY id LIMIT ?", q.After, q.Limit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		page := model.AutomationPage[model.Profile]{Items: []model.Profile{}}
		for rows.Next() {
			var body string
			var v model.Profile
			if err = rows.Scan(&body); err != nil {
				return nil, err
			}
			if err = json.Unmarshal([]byte(body), &v); err != nil {
				return nil, err
			}
			if len(page.Items) == q.Limit {
				page.HasMore = true
				page.NextAfter = page.Items[len(page.Items)-1].ID
				break
			}
			page.Items = append(page.Items, v)
		}
		return page, rows.Err()
	case "profiles.get":
		var q struct {
			ID string `json:"id"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if !profileIdentifier(q.ID) {
			return nil, model.Err(400, "profile id required")
		}
		return r.profile(q.ID)
	case "profiles.save":
		var q struct {
			ID                   string                    `json:"id"`
			ExpectedRevision     *int                      `json:"expectedRevision"`
			Name                 string                    `json:"name"`
			Instructions         string                    `json:"instructions"`
			AvatarAttachmentID   string                    `json:"avatarAttachmentId"`
			WorkspaceID          string                    `json:"workspaceId"`
			Binding              model.ProfileProvider     `json:"binding"`
			Capabilities         model.ProfileCapabilities `json:"capabilities"`
			AllowedProfileIDs    []string                  `json:"allowedProfileIds"`
			NotificationsEnabled bool                      `json:"notificationsEnabled"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		creating := q.ID == ""
		v := model.Profile{ID: q.ID, Revision: 1, Name: q.Name, Instructions: q.Instructions, AvatarAttachmentID: q.AvatarAttachmentID,
			WorkspaceID: q.WorkspaceID, Binding: q.Binding, Capabilities: q.Capabilities, AllowedProfileIDs: q.AllowedProfileIDs, NotificationsEnabled: q.NotificationsEnabled}
		if v.AllowedProfileIDs == nil {
			v.AllowedProfileIDs = []string{}
		}
		expected := 0
		reconcile := len(v.AllowedProfileIDs) != 0
		resetBindings := false
		narrowing := false
		if creating {
			if q.ExpectedRevision != nil {
				return nil, model.Err(400, "expectedRevision requires an existing profile")
			}
			v.ID = randomID()
		} else {
			if !profileIdentifier(q.ID) {
				return nil, model.Err(400, "invalid profile id")
			}
			old, err := r.profile(q.ID)
			if err != nil {
				return nil, err
			}
			if q.ExpectedRevision == nil || *q.ExpectedRevision != old.Revision {
				return nil, model.Err(409, "profile revision changed")
			}
			if v.Binding != old.Binding {
				return nil, model.Err(409, "binding changes require profiles.switch")
			}
			// Widening a policy requires a quiescent profile; narrowing is always
			// allowed and invalidates the admitted catalog of live turns.
			widening := profilePolicyWidens(old, v)
			narrowing = profilePolicyNarrows(old, v)
			workspaceChanged := v.WorkspaceID != old.WorkspaceID
			resetBindings = workspaceChanged || widening
			reconcile = !slices.Equal(old.AllowedProfileIDs, v.AllowedProfileIDs)
			if workspaceChanged || widening {
				if err = r.profileBusy(old.ID); err != nil {
					return nil, err
				}
			}
			expected, v.Revision, v.Archived = old.Revision, old.Revision+1, old.Archived
		}
		if err := r.validateProfile(v, creating); err != nil {
			return nil, err
		}
		var bindings []profileStoreBinding
		if resetBindings {
			var err error
			bindings, err = r.prepareProfileStoreBindings(v)
			if err != nil {
				return nil, err
			}
			// Workspace removal does not take the lifecycle lock. Check again
			// after teardown before persisting any new profile references.
			if err = r.validateProfile(v, false); err != nil {
				return nil, err
			}
		}
		if err := r.storeProfile(v, expected, "profiles.saved", bindings, reconcile); err != nil {
			return nil, err
		}
		if narrowing {
			// The persisted policy revision no longer admits the catalog of live
			// turns. Pending attention is invalidated by the policy trigger, so
			// wake those calls instead of waiting for their expiry.
			r.invalidateProfileToolPolicy(v.ID)
		}
		return v, nil
	case "profiles.archive":
		var q struct {
			ID               string `json:"id"`
			ExpectedRevision int    `json:"expectedRevision"`
			Archived         *bool  `json:"archived"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if !profileIdentifier(q.ID) || q.Archived == nil {
			return nil, model.Err(400, "profile id and archived required")
		}
		v, err := r.profile(q.ID)
		if err != nil {
			return nil, err
		}
		if q.ExpectedRevision != v.Revision {
			return nil, model.Err(409, "profile revision changed")
		}
		v.Revision++
		v.Archived = *q.Archived
		if err = r.storeProfile(v, q.ExpectedRevision, "profiles.archived", nil, true); err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, model.Err(404, "unknown profile store operation")
}
