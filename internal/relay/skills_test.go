package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"meshra/internal/model"
)

func skillCall(t *testing.T, r *Relay, op string, q any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return r.Automation(context.Background(), admin, op, raw)
}

func TestSkillVersionPinsSurviveLibraryUpdatesAndRevocation(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	value, err := skillCall(t, r, "skills.save", map[string]any{"name": "Review policy", "description": "Quoted review instructions", "markdown": "Version one: report uncertainty."})
	if err != nil {
		t.Fatal(err)
	}
	first := value.(localSkillVersion)
	_, err = skillCall(t, r, "skills.bind", map[string]any{"profileId": profile.ID, "skillId": first.ID, "version": 1, "enabled": true, "expectedRevision": 0})
	if err != nil {
		t.Fatal(err)
	}
	conversation := continuityConversation(t, r, "chat", profile)
	r.mu.Lock()
	binding, err := r.profileBinding(profile.ID, conversation.ID)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	snapshot, err := r.prepareProfileSnapshot(profile, binding)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	value, err = skillCall(t, r, "skills.save", map[string]any{"id": first.ID, "expectedRevision": 1, "name": "Review policy", "description": "Updated", "markdown": "Version two: different instructions."})
	if err != nil {
		t.Fatal(err)
	}
	second := value.(localSkillVersion)
	if second.Version != 2 || second.SHA256 == first.SHA256 {
		t.Fatal("updated content did not create a distinct immutable version")
	}
	r.mu.Lock()
	pinned, err := r.readPinnedSkill(snapshot, first.ID)
	instructions, injectErr := r.profileSkillInstructions(snapshot)
	r.mu.Unlock()
	if err != nil || injectErr != nil {
		t.Fatalf("pinned skill unavailable: %v %v", err, injectErr)
	}
	if pinned.Markdown != first.Markdown || !strings.Contains(instructions, first.Markdown) || strings.Contains(instructions, second.Markdown) {
		t.Fatal("library update silently replaced admitted instructions")
	}
	_, err = skillCall(t, r, "skills.remove", map[string]any{"id": first.ID, "expectedRevision": 2})
	requireStatus(t, err, 409)
	_, err = skillCall(t, r, "skills.bind", map[string]any{"profileId": profile.ID, "skillId": first.ID, "version": 2, "enabled": false, "expectedRevision": 1})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	_, err = r.readPinnedSkill(snapshot, first.ID)
	r.mu.Unlock()
	requireStatus(t, err, 403)
}

func TestSkillInjectionBoundAndScopedLookup(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	value, err := skillCall(t, r, "skills.save", map[string]any{"name": "Large reference", "description": "Read the full pinned version through the scoped tool.", "markdown": strings.Repeat("界", 20000)})
	if err != nil {
		t.Fatal(err)
	}
	skill := value.(localSkillVersion)
	_, err = skillCall(t, r, "skills.bind", map[string]any{"profileId": profile.ID, "skillId": skill.ID, "version": 1, "enabled": true, "expectedRevision": 0})
	if err != nil {
		t.Fatal(err)
	}
	conversation := continuityConversation(t, r, "chat", profile)
	r.mu.Lock()
	binding, err := r.profileBinding(profile.ID, conversation.ID)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	snapshot, err := r.prepareProfileSnapshot(profile, binding)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	injected, err := r.profileSkillInstructions(snapshot)
	_, denied := r.readPinnedSkill(snapshot, randomID())
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(injected) > 16*1024 || strings.Contains(injected, skill.Markdown) || !strings.Contains(injected, skill.ID) {
		t.Fatal("large skill was not replaced with a bounded pinned reference")
	}
	requireStatus(t, denied, 403)
	raw, _ := json.Marshal(map[string]any{"id": skill.ID})
	_, err = r.Automation(context.Background(), model.Principal{AgentID: "omp:untrusted"}, "skills.read", raw)
	requireStatus(t, err, 403)
}
