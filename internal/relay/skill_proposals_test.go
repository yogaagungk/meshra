package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"meshra/internal/model"
)

func TestSkillProposalReviewDoesNotChangePinsOrBindDraft(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	v, err := skillCall(t, r, "skills.save", map[string]any{"name": "Original", "description": "reviewed", "markdown": "Original instructions"})
	if err != nil {
		t.Fatal(err)
	}
	original := v.(localSkillVersion)
	_, err = skillCall(t, r, "skills.bind", map[string]any{"profileId": profile.ID, "skillId": original.ID, "version": 1, "enabled": true, "expectedRevision": 0})
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
	insert := func(text string) skillProposal {
		p := skillProposal{ID: randomID(), ProfileID: profile.ID, TaskID: randomID(), ConversationID: conversation.ID, Name: "Proposed", Description: "draft", Instructions: text, BasedOn: &skillProposalBase{ID: original.ID, Version: 1}, Status: "pending", CreatedAt: "2026-01-01T00:00:00Z"}
		body, _ := json.Marshal(p)
		if _, err := r.db.Exec("INSERT INTO skill_proposals(id,profile_id,task_id,status,body) VALUES(?,?,?,?,?)", p.ID, p.ProfileID, p.TaskID, p.Status, string(body)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rejected := insert("REJECTED-CANARY")
	v, err = skillCall(t, r, "skills.proposals.review", map[string]any{"id": rejected.ID, "decision": "reject"})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(skillProposal); got.Status != "rejected" || got.SkillID != "" || got.ReviewedAt == "" {
		t.Fatalf("bad rejection: %+v", got)
	}
	_, err = skillCall(t, r, "skills.proposals.review", map[string]any{"id": rejected.ID, "decision": "approve"})
	requireStatus(t, err, 409)
	proposal := insert("Approved instructions")
	v, err = skillCall(t, r, "skills.proposals.review", map[string]any{"id": proposal.ID, "decision": "approve"})
	if err != nil {
		t.Fatal(err)
	}
	approved := v.(skillProposal)
	if approved.Status != "approved" || approved.SkillID == "" || approved.SkillID == original.ID || approved.TaskID != proposal.TaskID || approved.ConversationID != conversation.ID {
		t.Fatalf("bad approval: %+v", approved)
	}
	r.mu.Lock()
	pinned, err := r.readPinnedSkill(snapshot, original.ID)
	injected, injectErr := r.profileSkillInstructions(snapshot)
	created, createErr := r.readLocalSkill(approved.SkillID, 1)
	var bindings int
	countErr := r.db.QueryRow("SELECT count(*) FROM profile_skill_bindings WHERE skill_id=?", approved.SkillID).Scan(&bindings)
	r.mu.Unlock()
	if err != nil || injectErr != nil || createErr != nil || countErr != nil {
		t.Fatalf("reading reviewed skills: %v %v %v %v", err, injectErr, createErr, countErr)
	}
	if pinned.SHA256 != original.SHA256 || pinned.Markdown != original.Markdown || created.Markdown != proposal.Instructions || bindings != 0 || strings.Contains(injected, "REJECTED-CANARY") || strings.Contains(injected, proposal.Instructions) {
		t.Fatal("proposal changed pinned instructions or was implicitly bound")
	}
	pageValue, err := skillCall(t, r, "skills.proposals.list", map[string]any{"limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	page := pageValue.(model.AutomationPage[skillProposal])
	if len(page.Items) != 1 || !page.HasMore || page.NextAfter == "" {
		t.Fatalf("invalid page: %+v", page)
	}
	_, err = r.Automation(context.Background(), model.Principal{AgentID: "untrusted"}, "skills.proposals.list", json.RawMessage(`{}`))
	requireStatus(t, err, 403)
}

func TestSkillProposalValidation(t *testing.T) {
	for _, q := range []toolSkillProposeArgs{
		{Name: "", Instructions: "x"},
		{Name: "safe", Instructions: "  "},
		{Name: "safe", Instructions: strings.Repeat("x", 65537)},
		{Name: "safe", Instructions: "x", BasedOn: &skillProposalBase{ID: randomID(), Version: 0}},
	} {
		if err := validateSkillProposal(&q); err == nil {
			t.Fatalf("accepted invalid proposal: %+v", q)
		}
	}
	valid := toolSkillProposeArgs{Name: "safe", Instructions: "x", BasedOn: &skillProposalBase{ID: randomID(), Version: 1}}
	if err := validateSkillProposal(&valid); err != nil {
		t.Fatal(err)
	}
}

func TestSkillProposalToolRequiresActiveTurnAndPinnedBase(t *testing.T) {
	type outcomes struct {
		valid                            model.ToolResult
		validErr, foreignErr, versionErr error
	}
	results := make(chan outcomes, 1)
	var r *Relay
	var baseID string
	run := func(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
		r.mu.Lock()
		b := r.toolBrokers[turn.Task.ID]
		r.mu.Unlock()
		q := toolSkillProposeArgs{Name: "Draft", Description: "proposal", Instructions: "New instructions", BasedOn: &skillProposalBase{ID: baseID, Version: 1}}
		valid, validErr := r.skillProposeForTool(ctx, b, q)
		q.BasedOn = &skillProposalBase{ID: randomID(), Version: 1}
		_, foreignErr := r.skillProposeForTool(ctx, b, q)
		q.BasedOn = &skillProposalBase{ID: baseID, Version: 2}
		_, versionErr := r.skillProposeForTool(ctx, b, q)
		results <- outcomes{valid, validErr, foreignErr, versionErr}
		return "finished", nil
	}
	r, _ = continuityOpenTest(t, run)
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	value, err := skillCall(t, r, "skills.save", map[string]any{"name": "Original", "description": "reviewed", "markdown": "Original instructions"})
	if err != nil {
		t.Fatal(err)
	}
	baseID = value.(localSkillVersion).ID
	_, err = skillCall(t, r, "skills.bind", map[string]any{"profileId": profile.ID, "skillId": baseID, "version": 1, "enabled": true, "expectedRevision": 0})
	if err != nil {
		t.Fatal(err)
	}
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, admin, profile, conversation, "Propose a skill")
	got := <-results
	if got.validErr != nil {
		t.Fatal(got.validErr)
	}
	requireStatus(t, got.foreignErr, 403)
	requireStatus(t, got.versionErr, 403)
	var proposal skillProposal
	if err := json.Unmarshal([]byte(brokerText(got.valid)), &proposal); err != nil {
		t.Fatal(err)
	}
	if proposal.ProfileID != profile.ID || proposal.TaskID != task.ID || proposal.ConversationID != conversation.ID || proposal.Status != "pending" || proposal.BasedOn == nil || proposal.BasedOn.ID != baseID {
		t.Fatalf("incorrect proposal provenance: %+v", proposal)
	}
	if completed := awaitTask(t, r, task.ID); completed.State != "completed" {
		t.Fatal(completed.Error)
	}
	var count int
	if err := r.db.QueryRow("SELECT count(*) FROM skill_proposals").Scan(&count); err != nil || count != 1 {
		t.Fatalf("unexpected proposal count %d: %v", count, err)
	}
}
