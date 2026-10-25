package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"meshra/internal/model"
)

type localSkillVersion struct {
	ID                 string   `json:"id"`
	Version            int      `json:"version"`
	Revision           int      `json:"revision"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	Markdown           string   `json:"markdown"`
	AssetAttachmentIDs []string `json:"assetAttachmentIds"`
	SHA256             string   `json:"sha256"`
	// Archive assets live in the immutable version, never in a shared filesystem.
	Assets []skillAsset `json:"assets,omitempty"`
}

type profileSkillBinding struct {
	ProfileID string `json:"profileId"`
	SkillID   string `json:"skillId"`
	Version   int    `json:"version"`
	Revision  int    `json:"revision"`
	Enabled   bool   `json:"enabled"`
}

func (r *Relay) readLocalSkill(id string, version int) (localSkillVersion, error) {
	var value localSkillVersion
	var body string
	err := r.db.QueryRow("SELECT v.body FROM local_skill_versions v JOIN local_skills s ON s.id=v.skill_id WHERE v.skill_id=? AND (?<>0 OR s.archived=0) AND v.revision=CASE WHEN ?=0 THEN s.revision ELSE ? END", id, version, version, version).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return value, model.Err(404, "skill version not found")
	}
	if err != nil {
		return value, err
	}
	err = json.Unmarshal([]byte(body), &value)
	return value, err
}

func (r *Relay) skillOperation(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	switch operation {
	case "skills.list":
		var q struct {
			After string `json:"after"`
			Limit int    `json:"limit"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if q.Limit == 0 {
			q.Limit = 50
		}
		if q.Limit < 1 || q.Limit > 100 {
			return nil, model.Err(400, "limit must be 1..100")
		}
		rows, err := r.db.QueryContext(ctx, "SELECT v.body FROM local_skills s JOIN local_skill_versions v ON v.skill_id=s.id AND v.revision=s.revision WHERE s.archived=0 AND s.id>? ORDER BY s.id LIMIT ?", q.After, q.Limit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		page := model.AutomationPage[localSkillVersion]{Items: []localSkillVersion{}}
		for rows.Next() {
			var body string
			var value localSkillVersion
			if err = rows.Scan(&body); err != nil {
				return nil, err
			}
			if err = json.Unmarshal([]byte(body), &value); err != nil {
				return nil, err
			}
			if len(page.Items) == q.Limit {
				page.HasMore = true
				break
			}
			page.Items = append(page.Items, value)
		}
		if page.HasMore {
			page.NextAfter = page.Items[len(page.Items)-1].ID
		}
		return page, rows.Err()
	case "skills.read":
		var q struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if !profileIdentifier(q.ID) || q.Version < 0 {
			return nil, model.Err(400, "invalid skill reference")
		}
		return r.readLocalSkill(q.ID, q.Version)
	case "skills.save":
		var q struct {
			ID                 string   `json:"id"`
			ExpectedRevision   *int     `json:"expectedRevision"`
			Name               string   `json:"name"`
			Description        string   `json:"description"`
			Markdown           string   `json:"markdown"`
			AssetAttachmentIDs []string `json:"assetAttachmentIds"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if err := validateReviewedSkill(q.Name, q.Description, q.Markdown); err != nil {
			return nil, err
		}
		if len(q.Markdown) > 65536 || len(q.AssetAttachmentIDs) > 100 {
			return nil, model.Err(413, "skill content or asset bound exceeded")
		}
		seen := map[string]bool{}
		total := int64(len(q.Markdown))
		for _, id := range q.AssetAttachmentIDs {
			if seen[id] {
				return nil, model.Err(400, "duplicate skill asset")
			}
			seen[id] = true
			asset, err := r.attachment(id)
			if err != nil {
				return nil, err
			}
			if err = r.attachmentAccess(p, asset); err != nil {
				return nil, err
			}
			if asset.Unavailable {
				return nil, model.Err(409, "skill asset unavailable")
			}
			total += asset.Size
			if total > templateMaxBytes {
				return nil, model.Err(413, "skill assets exceed 25 MiB")
			}
		}
		creating := q.ID == ""
		revision := 1
		if creating {
			if q.ExpectedRevision != nil {
				return nil, model.Err(400, "expectedRevision requires a skill")
			}
			q.ID = randomID()
		} else {
			old, err := r.readLocalSkill(q.ID, 0)
			if err != nil {
				return nil, err
			}
			if q.ExpectedRevision == nil || *q.ExpectedRevision != old.Revision {
				return nil, model.Err(409, "skill revision changed")
			}
			revision = old.Revision + 1
		}
		value := localSkillVersion{ID: q.ID, Version: revision, Revision: revision, Name: q.Name, Description: q.Description, Markdown: redactSecrets(q.Markdown), AssetAttachmentIDs: q.AssetAttachmentIDs}
		content, _ := json.Marshal(struct {
			Markdown string
			Assets   []string
		}{value.Markdown, value.AssetAttachmentIDs})
		value.SHA256 = tokenHash(string(content))
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if creating {
			_, err = tx.Exec("INSERT INTO local_skills(id,revision,name,description) VALUES(?,?,?,?)", value.ID, revision, value.Name, value.Description)
		} else {
			_, err = tx.Exec("UPDATE local_skills SET revision=?,name=?,description=? WHERE id=? AND revision=?", revision, value.Name, value.Description, value.ID, revision-1)
		}
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec("INSERT INTO local_skill_versions(skill_id,revision,sha256,body) VALUES(?,?,?,?)", value.ID, revision, value.SHA256, string(body)); err != nil {
			return nil, err
		}
		if err = insertAutomationEvent(tx, "skills.saved", value.ID, "", map[string]any{"revision": revision}); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return value, nil
	case "skills.bind":
		var q struct {
			ProfileID        string `json:"profileId"`
			SkillID          string `json:"skillId"`
			Version          int    `json:"version"`
			Enabled          bool   `json:"enabled"`
			ExpectedRevision int    `json:"expectedRevision"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if q.Version < 1 {
			return nil, model.Err(400, "pin an explicit skill version")
		}
		if _, err := r.profile(q.ProfileID); err != nil {
			return nil, err
		}
		if _, err := r.readLocalSkill(q.SkillID, q.Version); err != nil {
			return nil, err
		}
		var revision int
		err := r.db.QueryRow("SELECT revision FROM profile_skill_bindings WHERE profile_id=? AND skill_id=?", q.ProfileID, q.SkillID).Scan(&revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if revision != q.ExpectedRevision {
			return nil, model.Err(409, "skill binding revision changed")
		}
		value := profileSkillBinding{ProfileID: q.ProfileID, SkillID: q.SkillID, Version: q.Version, Revision: revision + 1, Enabled: q.Enabled}
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("INSERT INTO profile_skill_bindings(profile_id,skill_id,version,enabled,revision) VALUES(?,?,?,?,?) ON CONFLICT(profile_id,skill_id) DO UPDATE SET version=excluded.version,enabled=excluded.enabled,revision=excluded.revision", q.ProfileID, q.SkillID, q.Version, boolInt(q.Enabled), value.Revision); err != nil {
			return nil, err
		}
		if err = insertAutomationEvent(tx, "skills.bound", q.SkillID, q.ProfileID, map[string]any{"version": q.Version, "revision": value.Revision}); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return value, nil
	case "skills.remove":
		var q struct {
			ID               string `json:"id"`
			ExpectedRevision int    `json:"expectedRevision"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		value, err := r.readLocalSkill(q.ID, 0)
		if err != nil {
			return nil, err
		}
		if value.Revision != q.ExpectedRevision {
			return nil, model.Err(409, "skill revision changed")
		}
		var bound bool
		if err = r.db.QueryRow("SELECT EXISTS(SELECT 1 FROM profile_skill_bindings WHERE skill_id=?)", q.ID).Scan(&bound); err != nil {
			return nil, err
		}
		if bound {
			return nil, model.Err(409, "skill is still bound")
		}
		// Retain immutable version rows; hiding the library entry never invalidates
		// a task's pinned content or turns a deleted skill into a different version.
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("UPDATE local_skills SET archived=1 WHERE id=?", q.ID); err != nil {
			return nil, err
		}
		if err = insertAutomationEvent(tx, "skills.removed", q.ID, "", map[string]any{"revision": value.Revision}); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return map[string]bool{"removed": true}, nil
	}
	return nil, model.Err(404, "unknown skill operation")
}
