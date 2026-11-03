package relay

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

func TestMemoryStagingReviewAndBounds(t *testing.T) {
	script := newBrokerScript(
		brokerStep{call: model.ToolCall{ID: "first", Name: "meshra_memory_save", Arguments: json.RawMessage(`{"content":"stage this"}`)}},
		brokerStep{call: model.ToolCall{ID: "duplicate", Name: "meshra_memory_save", Arguments: json.RawMessage(`{"content":"stage this"}`)}},
		brokerStep{call: model.ToolCall{ID: "reject", Name: "meshra_memory_save", Arguments: json.RawMessage(`{"content":"rejected canary"}`)}},
	)
	r, _ := continuityOpenTest(t, script.run)
	profile := brokerProfile(t, r, model.ProfileCapabilities{MemoryWrite: true})
	if _, err := r.db.Exec(`UPDATE agent_profiles SET body=json_set(body,'$.memoryWriteApproval','on') WHERE id=?`, profile.ID); err != nil {
		t.Fatal(err)
	}
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, admin, profile, conversation, "save notes")
	awaitTask(t, r, task.ID)
	if got := script.await(t, task.ID, 0, 10*time.Second); got.err != nil {
		t.Fatal(got.err)
	}
	if got := script.await(t, task.ID, 1, 10*time.Second); brokerStatus(got.err) != 409 {
		t.Fatalf("duplicate accepted: %v", got.err)
	}
	if got := script.await(t, task.ID, 2, 10*time.Second); got.err != nil {
		t.Fatal(got.err)
	}
	var saved int
	if err := r.db.QueryRow(`SELECT count(*) FROM profile_memories WHERE profile_id=?`, profile.ID).Scan(&saved); err != nil || saved != 0 {
		t.Fatalf("staging bypassed: %d %v", saved, err)
	}
	var first, rejected string
	if err := r.db.QueryRow(`SELECT id FROM profile_memory_pending WHERE profile_id=? AND content='stage this'`, profile.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := r.db.QueryRow(`SELECT id FROM profile_memory_pending WHERE profile_id=? AND content='rejected canary'`, profile.ID).Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	_, err := scopedMemoryCall(t, r, model.Principal{}, "memory.pending.review", map[string]any{"profileId": profile.ID, "id": first, "decision": "approve"})
	requireStatus(t, err, 403)
	_, err = scopedMemoryCall(t, r, admin, "memory.pending.review", map[string]any{"profileId": profile.ID, "id": rejected, "decision": "reject"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := scopedMemoryCall(t, r, admin, "memory.pending.review", map[string]any{"profileId": profile.ID, "id": first, "decision": "approve", "content": "reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	memory := value.(profileMemory)
	if memory.Content != "reviewed" || memory.Revision != 1 || memory.SourceTaskID != task.ID {
		t.Fatalf("bad approved memory: %+v", memory)
	}
	_, err = scopedMemoryCall(t, r, admin, "memory.pending.review", map[string]any{"profileId": profile.ID, "id": first, "decision": "approve"})
	requireStatus(t, err, 404)
	_, err = scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "scope": "conversation", "conversationId": conversation.ID, "content": "reviewed"})
	requireStatus(t, err, 409)
	r.mu.Lock()
	injected, err := r.profileMemoriesForTask(task)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(injected, "rejected canary") {
		t.Fatal("rejected memory reached prompt")
	}
}

func TestMemoryReviewBudget(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	for i := range 64 {
		content := fmt.Sprintf("%04d", i) + strings.Repeat("x", 4092)
		if _, err := r.db.Exec(`INSERT INTO profile_memories(id,profile_id,scope,revision,content,pinned,updated_at) VALUES(?,?,'operator',1,?,0,?)`, fmt.Sprintf("budget-%d", i), profile.ID, content, now().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	err := r.checkProfileMemoryBounds(profileMemory{ID: "new", ProfileID: profile.ID, Scope: "operator", Content: "extra"}, "")
	requireStatus(t, err, 429)
}

func TestProfileMemoryApprovalSetting(t *testing.T) {
	r, _ := openTest(t, nil)
	q := profileStoreRequest(profileStoreWorkspace(t, r))
	q["memoryWriteApproval"] = "on"
	profile := profileStoreCreate(t, r, q)
	if profile.MemoryWriteApproval != "on" {
		t.Fatalf("setting not saved: %+v", profile)
	}
	delete(q, "memoryWriteApproval")
	q["id"], q["expectedRevision"] = profile.ID, profile.Revision
	profile = profileStoreCreate(t, r, q)
	if profile.MemoryWriteApproval != "on" {
		t.Fatal("omitting setting disabled memory review")
	}
	q["expectedRevision"] = profile.Revision
	q["memoryWriteApproval"] = "invalid"
	_, err := profileStoreCall(t, r, "profiles.save", q)
	requireStatus(t, err, 400)
}
