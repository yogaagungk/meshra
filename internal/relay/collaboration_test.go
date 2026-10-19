package relay

import (
	"database/sql"
	"meshra/internal/model"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConversationIsolation(t *testing.T) {
	r, _ := openTest(t, nil)
	a := createTest(t, r, "claude", "prod")
	b := createTest(t, r, "claude", "wallet")
	p := model.Principal{AgentID: a.ID}
	other := model.Principal{AgentID: b.ID}
	v, err := r.Collaborate(p, "conversation.create", model.CollaborationRequest{Title: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.Conversation)
	_, err = r.Collaborate(other, "conversation.get", model.CollaborationRequest{ID: c.ID})
	requireStatus(t, err, 403)
	v, err = r.Collaborate(other, "conversation.list", model.CollaborationRequest{})
	if err != nil || len(v.([]model.Conversation)) != 0 {
		t.Fatalf("leak: %v %v", v, err)
	}
	_, err = r.Collaborate(other, "conversation.addParticipant", model.CollaborationRequest{ID: c.ID, AgentID: b.ID})
	requireStatus(t, err, 403)
	_, err = r.Collaborate(p, "conversation.addParticipant", model.CollaborationRequest{ID: c.ID, AgentID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(other, "conversation.get", model.CollaborationRequest{ID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContextAuthorizationAndPinning(t *testing.T) {
	r, _ := openTest(t, nil)
	a := createTest(t, r, "claude", "prod")
	b := createTest(t, r, "claude", "wallet")
	p := model.Principal{AgentID: a.ID}
	other := model.Principal{AgentID: b.ID}
	v, err := r.Collaborate(p, "context.publish", model.CollaborationRequest{Title: "contract", Content: "version one", IdempotencyKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.SharedContext)
	_, err = r.Collaborate(other, "context.get", model.CollaborationRequest{ContextID: c.ID, Version: 1})
	requireStatus(t, err, 403)
	_, err = r.Collaborate(other, "context.grant", model.CollaborationRequest{ContextID: c.ID, AgentID: b.ID})
	requireStatus(t, err, 403)
	_, err = r.Collaborate(p, "context.grant", model.CollaborationRequest{ContextID: c.ID, AgentID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(p, "context.publish", model.CollaborationRequest{ContextID: c.ID, Version: 2, Content: "version two"})
	if err != nil {
		t.Fatal(err)
	}
	v, err = r.Collaborate(other, "context.get", model.CollaborationRequest{ContextID: c.ID, Version: 1})
	if err != nil || *v.(model.SharedContext).Content != "version one" {
		t.Fatalf("pin changed: %v %v", v, err)
	}
	_, err = r.Collaborate(p, "context.revoke", model.CollaborationRequest{ContextID: c.ID, AgentID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(other, "context.get", model.CollaborationRequest{ContextID: c.ID, Version: 1})
	requireStatus(t, err, 403)
	var n int
	if err = r.db.QueryRow("SELECT count(*) FROM context_access_log WHERE outcome='denied'").Scan(&n); err != nil || n != 3 {
		t.Fatalf("audit: %d %v", n, err)
	}
	if _, err = r.db.Exec("UPDATE artifacts SET content='corrupt'"); err == nil {
		t.Fatal("immutable artifact updated")
	}
}
func TestContextLimitsAndIntegrity(t *testing.T) {
	r, _ := openTest(t, nil)
	_, err := r.Collaborate(admin, "context.publish", model.CollaborationRequest{Content: string(make([]byte, 65537))})
	requireStatus(t, err, 413)
	_, err = r.Collaborate(admin, "context.publish", model.CollaborationRequest{Content: "password=secret"})
	requireStatus(t, err, 400)
	v, err := r.Collaborate(admin, "context.publish", model.CollaborationRequest{Content: "valid"})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.SharedContext)
	if _, err = r.db.Exec("DROP TRIGGER artifacts_immutable"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.db.Exec("UPDATE artifacts SET content='bad'"); err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(admin, "context.get", model.CollaborationRequest{ContextID: c.ID, Version: 1})
	requireStatus(t, err, 500)
}

func TestDelegationDependenciesAndRevocation(t *testing.T) {
	r, _ := openTest(t, nil)
	a := createTest(t, r, "claude", "prod")
	b := createTest(t, r, "codex", "implementer")
	c := createTest(t, r, "omp", "reviewer")
	v, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "delivery", Participants: []string{a.ID, b.ID, c.ID}})
	if err != nil {
		t.Fatal(err)
	}
	conv := v.(model.Conversation)
	v, err = r.Collaborate(model.Principal{AgentID: a.ID}, "context.publish", model.CollaborationRequest{Content: "pinned contract"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := v.(model.SharedContext)
	for _, id := range []string{b.ID, c.ID} {
		_, err = r.Collaborate(model.Principal{AgentID: a.ID}, "context.grant", model.CollaborationRequest{ContextID: ctx.ID, AgentID: id})
		if err != nil {
			t.Fatal(err)
		}
	}
	q := model.SendRequest{From: a.ID, To: b.ID, Text: "implement", Type: "request", ConversationID: conv.ID, ContextRefs: []model.ContextRef{{ContextID: ctx.ID, Version: 1}}, IdempotencyKey: "impl"}
	first, err := r.Send(admin, q)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := r.Send(admin, q)
	if err != nil || duplicate.ID != first.ID {
		t.Fatalf("dedup %v %v", duplicate, err)
	}
	q.To = c.ID
	q.Text = "review"
	q.IdempotencyKey = "review"
	q.DependsOn = []string{first.ID}
	second, err := r.Send(admin, q)
	if err != nil {
		t.Fatal(err)
	}
	startTest(t, r, c.ID)
	time.Sleep(50 * time.Millisecond)
	second, err = r.Task(admin, second.ID)
	if err != nil || second.Attempts != 0 {
		t.Fatalf("blocked task ran %v %v", second, err)
	}
	startTest(t, r, b.ID)
	first = awaitTask(t, r, first.ID)
	second = awaitTask(t, r, second.ID)
	if first.State != "completed" || second.State != "completed" || !strings.Contains(second.Result, "pinned contract") {
		t.Fatalf("delivery %v %v", first, second)
	}
	q.IdempotencyKey = "invalid"
	q.DependsOn = []string{"future-task"}
	_, err = r.Send(admin, q)
	requireStatus(t, err, 400)
	q.DependsOn = nil
	q.To = b.ID
	q.IdempotencyKey = "revoked"
	// Stop the destination so revocation occurs before any delivery.
	if _, err = r.Action(admin, b.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	pending, err := r.Send(admin, q)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(model.Principal{AgentID: a.ID}, "context.revoke", model.CollaborationRequest{ContextID: ctx.ID, AgentID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	startTest(t, r, b.ID)
	pending = awaitTask(t, r, pending.ID)
	if pending.State != "failed" || pending.Attempts != 0 || pending.ErrorCategory != "context_denied" {
		t.Fatalf("revoked execution: %+v", pending)
	}
}

func TestCollaborationRestartAndPagination(t *testing.T) {
	r, dir := openTest(t, nil)
	a := createTest(t, r, "claude", "prod")
	b := createTest(t, r, "codex", "implementer")
	owner := model.Principal{AgentID: a.ID}
	for i := 0; i < 3; i++ {
		if _, err := r.Collaborate(owner, "conversation.create", model.CollaborationRequest{Title: "persist", Participants: []string{b.ID}}); err != nil {
			t.Fatal(err)
		}
	}
	v, err := r.Collaborate(owner, "conversation.list", model.CollaborationRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	page := v.([]model.Conversation)
	if len(page) != 2 {
		t.Fatal(page)
	}
	v, err = r.Collaborate(owner, "conversation.list", model.CollaborationRequest{After: page[1].ID, Limit: 2})
	if err != nil || len(v.([]model.Conversation)) != 1 {
		t.Fatalf("pagination %v %v", v, err)
	}
	v, err = r.Collaborate(owner, "context.publish", model.CollaborationRequest{Content: "durable context"})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.SharedContext)
	if _, err = r.Collaborate(owner, "context.grant", model.CollaborationRequest{ContextID: c.ID, AgentID: b.ID}); err != nil {
		t.Fatal(err)
	}
	task, err := r.Send(admin, model.SendRequest{From: a.ID, To: b.ID, Text: "use pinned data", ConversationID: page[0].ID, ContextRefs: []model.ContextRef{{ContextID: c.ID, Version: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v, err = reopened.Collaborate(model.Principal{AgentID: b.ID}, "context.get", model.CollaborationRequest{ContextID: c.ID, Version: 1})
	if err != nil || *v.(model.SharedContext).Content != "durable context" {
		t.Fatalf("context recovery %v %v", v, err)
	}
	startTest(t, reopened, b.ID)
	task = awaitTask(t, reopened, task.ID)
	if task.State != "completed" || task.ResultRef == nil {
		t.Fatalf("task recovery %+v", task)
	}
	if _, err = reopened.Collaborate(owner, "context.get", model.CollaborationRequest{ContextID: task.ResultRef.ContextID, Version: task.ResultRef.Version}); err != nil {
		t.Fatal(err)
	}
}
func TestMigrationFailureRollsBack(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(migration); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE artifacts(bad TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err == nil {
		t.Fatal("conflicting schema accepted")
	}
	var version int
	if err = db.QueryRow("SELECT max(version) FROM schema_version").Scan(&version); err != nil || version != 3 {
		t.Fatalf("partial migration version %d %v", version, err)
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='contexts'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial migration leaked table %d %v", count, err)
	}
	if _, err = db.Exec("DROP TABLE artifacts"); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err != nil {
		t.Fatal("migration not idempotent", err)
	}
}

func TestDependencyCancellationAndEncodedBudget(t *testing.T) {
	r, _ := openTest(t, nil)
	a := createTest(t, r, "claude", "prod")
	b := createTest(t, r, "codex", "implementer")
	c := createTest(t, r, "omp", "reviewer")
	v, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "bounded work", Participants: []string{a.ID, b.ID, c.ID}})
	if err != nil {
		t.Fatal(err)
	}
	conv := v.(model.Conversation)
	first, err := r.Send(admin, model.SendRequest{From: a.ID, To: b.ID, Text: "first", ConversationID: conv.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Send(admin, model.SendRequest{From: a.ID, To: c.ID, Text: "second", ConversationID: conv.ID, DependsOn: []string{first.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Cancel(admin, first.ID); err != nil {
		t.Fatal(err)
	}
	second = awaitTask(t, r, second.ID)
	if second.State != "failed" || second.ErrorCategory != "dependency_failed" || second.Attempts != 0 {
		t.Fatalf("dependent ran after cancellation: %+v", second)
	}
	v, err = r.Collaborate(admin, "context.publish", model.CollaborationRequest{Content: strings.Repeat("\x01", 6000)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := v.(model.SharedContext)
	if _, err = r.Collaborate(admin, "context.grant", model.CollaborationRequest{ContextID: ctx.ID, AgentID: b.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = r.Send(admin, model.SendRequest{From: a.ID, To: b.ID, Text: "encoded budget", ConversationID: conv.ID, ContextRefs: []model.ContextRef{{ContextID: ctx.ID, Version: 1}}})
	requireStatus(t, err, 413)
	// Malformed resource IDs must not persist secret-looking strings in the audit.
	_, err = r.Collaborate(admin, "context.get", model.CollaborationRequest{ContextID: "sk-" + strings.Repeat("a", 20), Version: 1})
	requireStatus(t, err, 400)
	var n int
	if err = r.db.QueryRow("SELECT count(*) FROM context_access_log WHERE context_id LIKE 'sk-%'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("secret audit identifier persisted %d %v", n, err)
	}
}
