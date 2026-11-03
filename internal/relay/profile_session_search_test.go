package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
)

func TestSessionSearchProfileAuthorizationAndBounds(t *testing.T) {
	entered := make(chan model.Turn, 1)
	release := make(chan struct{})
	r, _ := continuityOpenTest(t, func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
		if strings.Contains(turn.Task.Text, "hold-active") {
			entered <- turn
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return "reply", nil
	})
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	other := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	one := continuityConversation(t, r, "chat", profile)
	two := continuityConversation(t, r, "chat", profile)
	private := continuityConversation(t, r, "chat", other)
	first := awaitTask(t, r, continuitySend(t, r, profile, one, "sessionneedle "+strings.Repeat("界", 500)).ID)
	second := awaitTask(t, r, continuitySend(t, r, profile, two, "sessionneedle second").ID)
	awaitTask(t, r, continuitySend(t, r, other, private, "sessionneedle secret").ID)
	continuitySend(t, r, profile, one, "hold-active")
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
	search := func(q map[string]any) model.AutomationPage[profileSessionHit] {
		t.Helper()
		v, err := scopedHistoryCall(t, r, p, "session.search", q)
		if err != nil {
			t.Fatal(err)
		}
		return v.(model.AutomationPage[profileSessionHit])
	}
	page := search(map[string]any{"query": "sessionneedle", "limit": 1})
	if len(page.Items) != 1 || !page.HasMore || page.NextAfter == "" || page.Items[0].TaskID != second.ID || page.Items[0].ConversationID != two.ID || page.Items[0].Timestamp == "" {
		t.Fatalf("unexpected first page: %+v", page)
	}
	page = search(map[string]any{"query": "sessionneedle", "limit": 1, "after": page.NextAfter})
	if len(page.Items) != 1 || page.HasMore || page.Items[0].TaskID != first.ID || page.Items[0].ConversationID != one.ID || len(page.Items[0].Excerpt) > 400 || !utf8.ValidString(page.Items[0].Excerpt) {
		t.Fatalf("unexpected second page: %+v", page)
	}
	if _, err := r.db.Exec(`UPDATE profile_task_snapshots SET body=json_set(body,'$.external',json('true')) WHERE task_id=?`, turn.Task.ID); err != nil {
		t.Fatal(err)
	}
	if page = search(map[string]any{"query": "sessionneedle"}); len(page.Items) != 1 || page.Items[0].TaskID != first.ID {
		t.Fatalf("external audience received another conversation: %+v", page)
	}
	if _, err := r.db.Exec(`UPDATE profile_task_snapshots SET body=json_set(body,'$.external',json('false')) WHERE task_id=?`, turn.Task.ID); err != nil {
		t.Fatal(err)
	}
	_, err = scopedHistoryCall(t, r, p, "session.search", map[string]any{"profileId": other.ID, "query": "sessionneedle"})
	requireStatus(t, err, 403)
	if _, err := r.db.Exec(`DELETE FROM profile_conversations WHERE profile_id=? AND conversation_id=?`, profile.ID, two.ID); err != nil {
		t.Fatal(err)
	}
	if page = search(map[string]any{"query": "sessionneedle"}); len(page.Items) != 1 || page.Items[0].TaskID != first.ID {
		t.Fatalf("unlinked conversation leaked: %+v", page)
	}
	if _, err := r.db.Exec(`INSERT INTO profile_conversations(profile_id,conversation_id) VALUES(?,?)`, profile.ID, two.ID); err != nil {
		t.Fatal(err)
	}
	binding := continuityBinding(t, r, profile, two)
	if _, err := r.db.Exec(`DELETE FROM conversation_participants WHERE conversation_id=? AND agent_id=?`, two.ID, binding.AgentID); err != nil {
		t.Fatal(err)
	}
	if page = search(map[string]any{"query": "sessionneedle"}); len(page.Items) != 1 || page.Items[0].TaskID != first.ID {
		t.Fatalf("conversation without binding membership leaked: %+v", page)
	}
	_, err = scopedHistoryCall(t, r, p, "session.search", map[string]any{"query": "sessionneedle", "conversationId": private.ID})
	requireStatus(t, err, 400)
	_, err = scopedHistoryCall(t, r, p, "session.search", map[string]any{"query": "sessionneedle", "limit": 21})
	requireStatus(t, err, 400)
	stale := p
	stale.SessionID = "stale"
	_, err = scopedHistoryCall(t, r, stale, "session.search", map[string]any{"query": "sessionneedle"})
	requireStatus(t, err, 403)
}

func TestSessionSearchRetentionResetAndDeletion(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "reply", nil })
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	one := continuityConversation(t, r, "chat", profile)
	two := continuityConversation(t, r, "chat", profile)
	old := awaitTask(t, r, continuitySend(t, r, profile, one, "retentionneedle").ID)
	recent := awaitTask(t, r, continuitySend(t, r, profile, two, "retentionneedle").ID)
	if _, err := r.db.Exec(`UPDATE tasks SET body=json_set(body,'$.updatedAt','2000-01-01T00:00:00Z') WHERE id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`INSERT INTO profile_memories(id,profile_id,scope,revision,content,pinned,updated_at) VALUES('operator-memory',?,'operator',1,'retentionneedle secret',0,'2020-01-01T00:00:00Z')`, profile.ID); err != nil {
		t.Fatal(err)
	}
	search := func() model.AutomationPage[profileSessionHit] {
		t.Helper()
		v, err := scopedHistoryCall(t, r, admin, "session.search", map[string]any{"profileId": profile.ID, "query": "retentionneedle"})
		if err != nil {
			t.Fatal(err)
		}
		return v.(model.AutomationPage[profileSessionHit])
	}
	page := search()
	if len(page.Items) != 1 || page.Items[0].TaskID != recent.ID {
		t.Fatalf("retention or memory leak: %+v", page)
	}
	binding := continuityBinding(t, r, profile, two)
	if _, err := r.db.Exec(`UPDATE profile_context_epochs SET reset_after_seq=(SELECT seq FROM tasks WHERE id=?) WHERE binding_id=?`, recent.ID, binding.ID); err != nil {
		t.Fatal(err)
	}
	if page = search(); len(page.Items) != 0 {
		t.Fatalf("reset history leaked: %+v", page)
	}
	if _, err := continuityCall(t, r, "profiles.bindingAction", map[string]any{"profileId": profile.ID, "conversationId": two.ID, "action": "stop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Console(admin, "chat-delete", model.ConsoleRequest{ConversationID: two.ID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := r.db.QueryRow(`SELECT count(*) FROM profile_history WHERE conversation_id=?`, two.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted FTS rows: %d %v", count, err)
	}
	if err := r.db.QueryRow(`SELECT count(*) FROM profile_memories WHERE id='operator-memory'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("operator memory removed: %d %v", count, err)
	}
	if page = search(); len(page.Items) != 0 {
		b, _ := json.Marshal(page)
		t.Fatalf("deleted history leaked: %s", b)
	}
}
