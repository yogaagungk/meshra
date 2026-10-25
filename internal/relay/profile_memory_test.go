package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

func scopedMemoryCall(t *testing.T, r *Relay, p model.Principal, op string, q any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return r.profileMemoryOperation(context.Background(), p, op, raw)
}

func TestProfileMemoryDefaultsRevisionsAndExternalInjection(t *testing.T) {
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
	private := save(map[string]any{"content": "operator-private-canary", "pinned": true})
	if private.Scope != "operator" || private.ConversationID != "" || private.Revision != 1 {
		t.Fatalf("unsafe default memory: %+v", private)
	}
	shared := save(map[string]any{"content": "thread-one-canary", "scope": "conversation", "conversationId": one.ID})
	save(map[string]any{"content": "thread-two-canary", "scope": "conversation", "conversationId": two.ID})
	_, err := scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "id": private.ID, "expectedRevision": 0, "content": "stale"})
	requireStatus(t, err, 409)
	private = save(map[string]any{"id": private.ID, "expectedRevision": 1, "content": "operator-private-canary-updated", "pinned": true})
	_, err = scopedMemoryCall(t, r, admin, "memory.remove", map[string]any{"profileId": profile.ID, "id": private.ID, "expectedRevision": 1})
	requireStatus(t, err, 409)
	task := awaitTask(t, r, continuitySend(t, r, profile, one, "personal").ID)
	r.mu.Lock()
	injected, err := r.profileMemoriesForTask(task)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(injected, "untrusted data") || !strings.Contains(injected, private.Content) || !strings.Contains(injected, shared.Content) || strings.Contains(injected, "thread-two-canary") {
		t.Fatalf("personal memory scope failed: %s", injected)
	}
	r.mu.Lock()
	_, err = r.db.Exec("UPDATE profile_task_snapshots SET body=json_set(body,'$.external',json('true')) WHERE task_id=?", task.ID)
	if err == nil {
		injected, err = r.profileMemoriesForTask(task)
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(injected, private.Content) || !strings.Contains(injected, shared.Content) || strings.Contains(injected, "thread-two-canary") {
		t.Fatalf("external memory leaked: %s", injected)
	}
	v, err := scopedMemoryCall(t, r, admin, "memory.list", map[string]any{"profileId": profile.ID, "conversationId": one.ID})
	if err != nil {
		t.Fatal(err)
	}
	items := v.(profileMemoryPage).Items
	if len(items) != 2 || items[0].ID != private.ID {
		t.Fatalf("pinned ordering or scope failed: %+v", items)
	}
}

func TestProfileMemoryAgentCannotWidenOrReadOtherThread(t *testing.T) {
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
	two := continuityConversation(t, r, "task", profile)
	v, err := scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "content": "operator-only"})
	if err != nil {
		t.Fatal(err)
	}
	private := v.(profileMemory)
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
	v, err = scopedMemoryCall(t, r, p, "memory.save", map[string]any{"content": "agent-thread-canary"})
	if err != nil {
		t.Fatal(err)
	}
	m := v.(profileMemory)
	if m.Scope != "conversation" || m.ConversationID != one.ID || m.ProfileID != profile.ID {
		t.Fatalf("agent chose scope: %+v", m)
	}
	for _, payload := range []map[string]any{
		{"content": "widen", "scope": "operator"},
		{"content": "cross-thread", "scope": "conversation", "conversationId": two.ID},
		{"content": "replace operator", "id": private.ID, "expectedRevision": private.Revision},
	} {
		_, err = scopedMemoryCall(t, r, p, "memory.save", payload)
		requireStatus(t, err, 403)
	}
	_, err = scopedMemoryCall(t, r, p, "memory.remove", map[string]any{"id": private.ID, "expectedRevision": private.Revision})
	requireStatus(t, err, 403)
	v, err = scopedMemoryCall(t, r, p, "memory.list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	items := v.(profileMemoryPage).Items
	if len(items) != 1 || items[0].ID != m.ID {
		t.Fatalf("agent listed operator memory: %+v", items)
	}
	_, err = scopedMemoryCall(t, r, p, "memory.list", map[string]any{"conversationId": two.ID})
	requireStatus(t, err, 403)
}

func TestProfileMemorySourcesRetentionAndBounds(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "done", nil })
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	one := continuityConversation(t, r, "chat", profile)
	two := continuityConversation(t, r, "task", profile)
	source := awaitTask(t, r, continuitySend(t, r, profile, one, "source-canary").ID)
	_, err := scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "content": "cross-source", "scope": "conversation", "conversationId": two.ID, "sourceTaskId": source.ID})
	requireStatus(t, err, 403)
	v, err := scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "content": "saved-source-canary", "scope": "conversation", "conversationId": one.ID, "sourceTaskId": source.ID})
	if err != nil {
		t.Fatal(err)
	}
	memory := v.(profileMemory)
	if memory.SourceAvailable == nil || !*memory.SourceAvailable {
		t.Fatal("existing source not available")
	}
	r.mu.Lock()
	_, err = r.db.Exec("DELETE FROM profile_conversations WHERE profile_id=? AND conversation_id=?", profile.ID, one.ID)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	v, err = scopedMemoryCall(t, r, admin, "memory.list", map[string]any{"profileId": profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.(profileMemoryPage).Items) != 0 {
		t.Fatal("memory leaked after source conversation association was revoked")
	}
	_, err = scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "content": "copy revoked source", "sourceTaskId": source.ID})
	requireStatus(t, err, 403)
	r.mu.Lock()
	_, err = r.db.Exec("INSERT INTO profile_conversations(profile_id,conversation_id) VALUES(?,?)", profile.ID, one.ID)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	source.UpdatedAt = now().Add(-2 * time.Hour)
	body, marshalErr := json.Marshal(source)
	if marshalErr != nil {
		r.mu.Unlock()
		t.Fatal(marshalErr)
	}
	r.options.Retention = time.Hour
	_, err = r.db.Exec("UPDATE tasks SET body=? WHERE id=?", string(body), source.ID)
	if err == nil {
		err = r.prune()
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	v, err = scopedMemoryCall(t, r, admin, "memory.list", map[string]any{"profileId": profile.ID, "conversationId": one.ID})
	if err != nil {
		t.Fatal(err)
	}
	items := v.(profileMemoryPage).Items
	if len(items) != 1 || items[0].Content != memory.Content || items[0].SourceTaskID != source.ID || items[0].SourceAvailable == nil || *items[0].SourceAvailable {
		t.Fatalf("retained memory/provenance incorrect: %+v", items)
	}
	_, err = scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "content": strings.Repeat("a", 4097)})
	requireStatus(t, err, 413)
	_, err = r.profileMemoryOperation(context.Background(), admin, "memory.list", json.RawMessage(`{"profileId":"`+profile.ID+`","scope":"operator"}`))
	requireStatus(t, err, 400)
	// Fill through the real operation, so both the per-profile limit and the
	// bounded, pinned-first injection exercise production SQL and serialization.
	for i := range 255 {
		_, err = scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "content": strings.Repeat("x", 4000), "pinned": i == 0})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = scopedMemoryCall(t, r, admin, "memory.save", map[string]any{"profileId": profile.ID, "content": "overflow"})
	requireStatus(t, err, 429)
	task := awaitTask(t, r, continuitySend(t, r, profile, one, "inject bounded").ID)
	r.mu.Lock()
	injected, err := r.profileMemoriesForTask(task)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(injected) > 8192 || !strings.Contains(injected, `"pinned":true`) {
		t.Fatalf("memory injection unbounded or lost pinned memory: %d", len(injected))
	}
}
