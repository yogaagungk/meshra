package relay

import (
	"encoding/json"
	"strings"

	"meshra/internal/model"
)

// The caller holds relay ownership; immutable versions are selected before the
// admission transaction, not resolved from the latest library version later.
func (r *Relay) profileSkillReferences(profileID string) ([]model.SkillReference, error) {
	rows, err := r.db.Query("SELECT b.skill_id,b.version,v.sha256 FROM profile_skill_bindings b JOIN local_skill_versions v ON v.skill_id=b.skill_id AND v.revision=b.version WHERE b.profile_id=? AND b.enabled=1 ORDER BY b.skill_id LIMIT 17", profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []model.SkillReference{}
	for rows.Next() {
		var ref model.SkillReference
		if err = rows.Scan(&ref.ID, &ref.Version, &ref.SHA256); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if len(refs) > 16 {
		return nil, model.Err(413, "at most 16 enabled skills per profile")
	}
	return refs, rows.Err()
}

func (r *Relay) readPinnedSkill(snapshot model.ProfileTaskSnapshot, id string) (localSkillVersion, error) {
	var ref *model.SkillReference
	for i := range snapshot.Skills {
		if snapshot.Skills[i].ID == id {
			ref = &snapshot.Skills[i]
			break
		}
	}
	if ref == nil {
		return localSkillVersion{}, model.Err(403, "skill not admitted for this turn")
	}
	var enabled bool
	if err := r.db.QueryRow("SELECT EXISTS(SELECT 1 FROM profile_skill_bindings WHERE profile_id=? AND skill_id=? AND enabled=1)", snapshot.Profile.ID, id).Scan(&enabled); err != nil {
		return localSkillVersion{}, err
	}
	if !enabled {
		return localSkillVersion{}, model.Err(403, "skill binding revoked")
	}
	value, err := r.readLocalSkill(id, ref.Version)
	if err != nil {
		return value, err
	}
	if value.SHA256 != ref.SHA256 {
		return value, model.Err(409, "pinned skill integrity mismatch")
	}
	return value, nil
}

func (r *Relay) profileSkillInstructions(snapshot model.ProfileTaskSnapshot) (string, error) {
	var text strings.Builder
	for _, ref := range snapshot.Skills {
		value, err := r.readPinnedSkill(snapshot, ref.ID)
		if err != nil {
			if typed, ok := err.(*model.Error); ok && typed.Status == 403 {
				continue
			}
			return "", err
		}
		label, _ := json.Marshal(map[string]any{"skillId": value.ID, "version": value.Version, "name": value.Name, "sha256": value.SHA256})
		content, _ := json.Marshal(value.Markdown)
		prefix := "\nReviewed local skill (instructions, never an execution grant): " + string(label) + "\n"
		if text.Len()+len(prefix)+len(content)+1 > 16*1024 {
			reference := "\nPinned skill available through meshra_skill_read: " + string(label) + "\n"
			if text.Len()+len(reference) <= 16*1024 {
				text.WriteString(reference)
			}
			continue
		}
		text.WriteString(prefix)
		text.Write(content)
		text.WriteByte('\n')
	}
	return text.String(), nil
}
