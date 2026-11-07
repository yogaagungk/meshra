package relay

import (
	"encoding/json"
	"context"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

// Reviewed reusable knowledge reaches external/scheduled runs; owner-private
// notes and other conversations' memories never do, however relevant they rank.
func TestReusableKnowledgeReachesExternalRunsButNotPrivateOrOtherConversations(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "done", nil })
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	one := continuityConversation(t, r, "chat", profile)
	two := continuityConversation(t, r, "task", profile)
	save := func(q map[string]any) profileMemory {
		t.Helper()
		q["profileId"] = profile.ID
		v, err := scopedMemoryCall(t, r, admin, "memory.save", q)
		if err != nil {
			t.Fatal(err)
		}
		return v.(profileMemory)
	}
	save(map[string]any{"content": "refund policy owner private margin notes"})
	knowledge := save(map[string]any{"content": "refund policy thirty days approved wording", "reusable": true})
	save(map[string]any{"content": "refund policy customer two secret fact", "scope": "conversation", "conversationId": two.ID})
	if !knowledge.Reusable || knowledge.Scope != "operator" {
		t.Fatalf("knowledge not recorded: %+v", knowledge)
	}
	task := awaitTask(t, r, continuitySend(t, r, profile, one, "refund policy question").ID)
	r.mu.Lock()
	_, err := r.db.Exec("UPDATE profile_task_snapshots SET body=json_set(body,'$.external',json('true')) WHERE task_id=?", task.ID)
	var injected string
	if err == nil {
		injected, err = r.profileMemoriesForTask(task)
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(injected, "approved wording") {
		t.Fatalf("approved knowledge missing for external run: %s", injected)
	}
	if strings.Contains(injected, "owner private") || strings.Contains(injected, "customer two") {
		t.Fatalf("private or cross-customer memory leaked: %s", injected)
	}
}

// A model-authority principal can neither mark knowledge reusable nor edit it;
// the denial is an authorization failure, not a model refusal.
func TestModelCannotPromoteOrEditReusableKnowledge(t *testing.T) {
	entered := make(chan model.Turn, 1)
	release := make(chan struct{})
	r, _ := continuityOpenTest(t, func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
		entered <- turn
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	q := profileStoreRequest(profileStoreWorkspace(t, r))
	q["capabilities"] = model.ProfileCapabilities{MemoryWrite: true}
	profile := profileStoreCreate(t, r, q)
	one := continuityConversation(t, r, "chat", profile)
	v, err := scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "content": "approved wording", "reusable": true})
	if err != nil {
		t.Fatal(err)
	}
	knowledge := v.(profileMemory)
	continuitySend(t, r, profile, one, "active")
	var turn model.Turn
	select {
	case turn = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
	}
	defer close(release)
	p, err := r.Authenticate(turn.Token)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []map[string]any{
		{"content": "promote me", "reusable": true},
		{"content": "overwrite", "id": knowledge.ID, "expectedRevision": knowledge.Revision},
	} {
		_, err = scopedMemoryCall(t, r, p, "memory.save", payload)
		requireStatus(t, err, 403)
	}
	_, err = scopedMemoryCall(t, r, p, "memory.remove", map[string]any{"id": knowledge.ID, "expectedRevision": knowledge.Revision})
	requireStatus(t, err, 403)
}

// Limits and duplicate rules do not depend on review mode or writer; duplicates
// ignore case and whitespace, and over-limit data can be corrected downward.
func TestMemoryBoundsAreIdenticalForOperatorWritesAndShrinkingCorrectsOverLimit(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "done", nil })
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	save := func(q map[string]any) (any, error) {
		q["profileId"] = profile.ID
		return scopedMemoryCall(t, r, admin, "memory.save", q)
	}
	if _, err := save(map[string]any{"content": "Refund  Policy: thirty days"}); err != nil {
		t.Fatal(err)
	}
	_, err := save(map[string]any{"content": "refund policy: THIRTY days"})
	requireStatus(t, err, 409)
	// Simulate data that predates the budget: a profile already over the limit.
	big := strings.Repeat("z", 4000)
	for i := 0; i < 70; i++ {
		if _, err = r.db.Exec("INSERT INTO profile_memories(id,profile_id,scope,revision,content,pinned,updated_at) VALUES(?,?,?,?,?,0,?)", randomID(), profile.ID, "operator", 1, big+string(rune('a'+i%26))+strings.Repeat("q", i), now().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	_, err = save(map[string]any{"content": "one more distinct memory"})
	requireStatus(t, err, 429)
	var id string
	var revision int
	if err = r.db.QueryRow("SELECT id,revision FROM profile_memories WHERE profile_id=? AND length(content)>4000 LIMIT 1", profile.ID).Scan(&id, &revision); err != nil {
		t.Fatal(err)
	}
	if _, err = save(map[string]any{"id": id, "expectedRevision": revision, "content": "shrunk to a short note"}); err != nil {
		t.Fatalf("an over-limit memory must be correctable by shrinking: %v", err)
	}
	var count int
	if err = r.db.QueryRow("SELECT count(*) FROM profile_memories WHERE profile_id=?", profile.ID).Scan(&count); err != nil || count != 71 {
		t.Fatalf("existing data was discarded: %d %v", count, err)
	}
}

// Reset, forget and conversation deletion are different operations: a context
// reset keeps saved memories, forgetting removes retrieval eligibility while the
// audit event remains, and deleting a conversation removes its scoped memories.
func TestResetForgetAndConversationDeleteHaveDistinctMemoryOutcomes(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "done", nil })
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	one := continuityConversation(t, r, "chat", profile)
	v, err := scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "content": "pricing tier note", "pinned": true})
	if err != nil {
		t.Fatal(err)
	}
	kept := v.(profileMemory)
	count := func() int {
		var n int
		if err := r.db.QueryRow("SELECT count(*) FROM profile_memories WHERE profile_id=?", profile.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	raw, _ := json.Marshal(map[string]any{"id": profile.ID, "conversationId": one.ID, "expectedRevision": profile.Revision})
	if _, err = r.profileResetContext(admin, raw); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("a context reset must not forget saved memories")
	}
	if _, err = scopedMemoryCall(t, r, admin, "memory.remove", map[string]any{"profileId": profile.ID, "id": kept.ID, "expectedRevision": kept.Revision}); err != nil {
		t.Fatal(err)
	}
	var events int
	if err = r.db.QueryRow("SELECT count(*) FROM automation_events WHERE kind='memory.removed' AND resource_id=?", kept.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if count() != 0 || events != 1 {
		t.Fatalf("forget: memories=%d audit=%d", count(), events)
	}
}
