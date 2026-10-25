package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"meshra/internal/model"
)

func profileAutomationCall(t *testing.T, r *Relay, op string, q any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return r.Automation(context.Background(), admin, op, raw)
}

func TestProfileAdmissionCannotBypassSnapshot(t *testing.T) {
	r, _ := openTest(t, nil)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	q := map[string]any{"id": profile.ID, "text": "exact profile request", "idempotencyKey": "profile-first"}
	first, err := profileAutomationCall(t, r, "profiles.send", q)
	if err != nil {
		t.Fatal(err)
	}
	task := first.(model.Task)
	if task.Chat || task.ConversationID == "" {
		t.Fatalf("profile turn used disposable legacy chat: %+v", task)
	}
	_, err = r.Send(admin, model.SendRequest{From: "human:operator", To: task.To, Text: "raw admin bypass", ConversationID: task.ConversationID})
	requireStatus(t, err, 403)
	_, err = r.Action(admin, task.To, "start")
	requireStatus(t, err, 403)
	second, err := profileAutomationCall(t, r, "profiles.send", q)
	if err != nil {
		t.Fatal(err)
	}
	if second.(model.Task).ID != task.ID {
		t.Fatal("implicit conversation retry created a second task")
	}
	q["text"] = "different request"
	_, err = profileAutomationCall(t, r, "profiles.send", q)
	requireStatus(t, err, 409)
	var conversations int
	if err = r.db.QueryRow("SELECT count(*) FROM profile_conversations WHERE profile_id=?", profile.ID).Scan(&conversations); err != nil {
		t.Fatal(err)
	}
	if conversations != 1 {
		t.Fatalf("idempotency created %d conversations", conversations)
	}
	r.mu.Lock()
	snapshot, err := r.profileSnapshot(task)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Binding.AgentID != task.To || snapshot.Profile.Revision != 1 {
		t.Fatalf("wrong admitted binding snapshot: %+v", snapshot)
	}
}

func TestProfileContextGrantsOnlyPinnedVersion(t *testing.T) {
	r, _ := openTest(t, nil)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	value, err := profileAutomationCall(t, r, "profiles.createConversation", map[string]any{"title": "Pinned versions", "profileIds": []string{profile.ID}, "kind": "chat"})
	if err != nil {
		t.Fatal(err)
	}
	c := value.(model.Conversation)
	pub, err := r.Collaborate(admin, "context.publish", model.CollaborationRequest{Title: "Pinned data", Content: "version one"})
	if err != nil {
		t.Fatal(err)
	}
	ref := pub.(model.SharedContext)
	_, err = r.Collaborate(admin, "context.attach", model.CollaborationRequest{ConversationID: c.ID, ContextID: ref.ID, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(admin, "context.grant", model.CollaborationRequest{AgentID: c.Participants[0], ContextID: ref.ID, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(admin, "context.publish", model.CollaborationRequest{ContextID: ref.ID, Version: 2, Content: "unshared version two"})
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AgentID: c.Participants[0]}
	_, err = r.Collaborate(p, "context.get", model.CollaborationRequest{ContextID: ref.ID, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(p, "context.get", model.CollaborationRequest{ContextID: ref.ID, Version: 2})
	requireStatus(t, err, 403)
	_, err = r.Collaborate(admin, "context.revoke", model.CollaborationRequest{AgentID: c.Participants[0], ContextID: ref.ID, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(p, "context.get", model.CollaborationRequest{ContextID: ref.ID, Version: 1})
	requireStatus(t, err, 403)
}

func TestProfileMigrationPreservesPopulatedV9(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "meshra.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []string{migration, migrationV2, migrationV3, migrationV4, migrationV5, migrationV6, migrationV7, migrationV8, migrationV9} {
		if _, err = db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	a := model.Agent{ID: "omp:legacy", Harness: "omp", Name: "legacy", Cwd: t.TempDir(), State: "registered"}
	body, _ := json.Marshal(a)
	if _, err = db.Exec("INSERT INTO agents(id,body) VALUES(?,?)", a.ID, string(body)); err != nil {
		t.Fatal(err)
	}
	task := model.Task{SchemaVersion: 1, ID: "v9-task", MessageID: "v9-message", CorrelationID: "v9-correlation", From: a.ID, To: a.ID, Type: "send", Text: "preserved", State: "completed", CreatedAt: now(), UpdatedAt: now(), Deadline: now().Add(time.Hour)}
	body, _ = json.Marshal(task)
	if _, err = db.Exec("INSERT INTO tasks(seq,id,sender,sender_session,recipient,operation,state,body) VALUES(123,?,'omp:legacy','','omp:legacy','send','completed',?)", task.ID, string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO events(id,task_id,kind,data,time) VALUES(456,?,'completed','v9 receipt',?)", task.ID, now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := r.Task(admin, task.ID)
	if err != nil || got.ID != task.ID || got.MessageID != task.MessageID || got.CorrelationID != task.CorrelationID {
		t.Fatalf("lost legacy identity: %+v %v", got, err)
	}
	events, err := r.Events(admin, task.ID, 0)
	if err != nil || len(events) != 1 || events[0].ID != 456 {
		t.Fatalf("lost legacy cursor: %+v %v", events, err)
	}
	var seq, profiles int
	if err = r.db.QueryRow("SELECT seq FROM tasks WHERE id=?", task.ID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if err = r.db.QueryRow("SELECT count(*) FROM agent_profiles").Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if seq != 123 || profiles != 0 {
		t.Fatalf("migration changed ownership/order: seq=%d profiles=%d", seq, profiles)
	}
}
