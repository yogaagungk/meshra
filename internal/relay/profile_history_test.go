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

func scopedHistoryCall(t *testing.T, r *Relay, p model.Principal, op string, q any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return r.profileHistoryOperation(context.Background(), p, op, raw)
}

func TestProfileHistoryFTSLiteralIsolationAndReset(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "reply canary", nil })
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	one := continuityConversation(t, r, "chat", profile)
	two := continuityConversation(t, r, "task", profile)
	first := awaitTask(t, r, continuitySend(t, r, profile, one, "private canary alpha OR beta "+strings.Repeat("界", 200)).ID)
	second := awaitTask(t, r, continuitySend(t, r, profile, two, "thread canary alpha").ID)
	search := func(conversation, query string) model.AutomationPage[profileHistoryHit] {
		t.Helper()
		v, err := scopedHistoryCall(t, r, admin, "history.search", map[string]any{"profileId": profile.ID, "conversationId": conversation, "query": query})
		if err != nil {
			t.Fatal(err)
		}
		return v.(model.AutomationPage[profileHistoryHit])
	}
	page := search(one.ID, "alpha OR beta")
	if len(page.Items) != 1 || page.Items[0].TaskID != first.ID || len(page.Items[0].Excerpt) > 400 || !utf8.ValidString(page.Items[0].Excerpt) {
		t.Fatalf("literal phrase/bound failure: %+v", page)
	}
	if got := search(two.ID, "alpha OR beta"); len(got.Items) != 0 {
		t.Fatalf("FTS interpreted OR or crossed conversations: %+v", got)
	}
	if got := search(one.ID, "thread"); len(got.Items) != 0 {
		t.Fatal("thread canary leaked")
	}
	_, err := scopedHistoryCall(t, r, admin, "history.read", map[string]any{"profileId": profile.ID, "conversationId": one.ID, "taskId": second.ID})
	requireStatus(t, err, 404)
	binding := continuityBinding(t, r, profile, one)
	r.mu.Lock()
	_, err = r.db.Exec("UPDATE profile_context_epochs SET reset_after_seq=(SELECT seq FROM tasks WHERE id=?) WHERE binding_id=?", first.ID, binding.ID)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := search(one.ID, "private"); len(got.Items) != 0 {
		t.Fatal("reset history remained searchable")
	}
	_, err = scopedHistoryCall(t, r, admin, "history.read", map[string]any{"profileId": profile.ID, "conversationId": one.ID, "taskId": first.ID})
	requireStatus(t, err, 404)
	for _, payload := range []string{
		`{"profileId":"` + profile.ID + `","conversationId":"` + one.ID + `","query":"alpha","limit":21}`,
		`{"profileId":"` + profile.ID + `","conversationId":"` + one.ID + `","query":"alpha","taskId":"forged"}`,
	} {
		_, err = r.profileHistoryOperation(context.Background(), admin, "history.search", json.RawMessage(payload))
		requireStatus(t, err, 400)
	}
}

func TestProfileHistoryActiveTurnScopeAndSequence(t *testing.T) {
	entered := make(chan model.Turn, 1)
	release := make(chan struct{})
	r, _ := continuityOpenTest(t, func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
		if strings.Contains(turn.Task.Text, "hold-current") {
			entered <- turn
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "completed", nil
	})
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	one := continuityConversation(t, r, "chat", profile)
	two := continuityConversation(t, r, "chat", profile)
	prior := awaitTask(t, r, continuitySend(t, r, profile, one, "prior canary").ID)
	awaitTask(t, r, continuitySend(t, r, profile, two, "other-thread canary").ID)
	current := continuitySend(t, r, profile, one, "hold-current canary")
	var turn model.Turn
	select {
	case turn = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
	}
	defer close(release)
	queued := continuitySend(t, r, profile, one, "queued canary")
	p, err := r.Authenticate(turn.Token)
	if err != nil {
		t.Fatal(err)
	}
	v, err := scopedHistoryCall(t, r, p, "history.read", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	page := v.(profileHistoryPage)
	if len(page.Items) != 1 || page.Items[0].TaskID != prior.ID {
		t.Fatalf("active/current/queued scope leaked: %+v", page)
	}
	for _, task := range []model.Task{current, queued} {
		_, err = scopedHistoryCall(t, r, p, "history.read", map[string]any{"taskId": task.ID})
		requireStatus(t, err, 404)
	}
	_, err = scopedHistoryCall(t, r, p, "history.read", map[string]any{"profileId": profile.ID, "conversationId": two.ID})
	requireStatus(t, err, 403)
	stale := p
	stale.SessionID = "old-session"
	_, err = scopedHistoryCall(t, r, stale, "history.read", map[string]any{})
	requireStatus(t, err, 403)
	stale = p
	stale.TaskID = prior.ID
	_, err = scopedHistoryCall(t, r, stale, "history.search", map[string]any{"query": "canary"})
	requireStatus(t, err, 403)
}

func TestProfileHistoryReadPaginationAndByteCap(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return strings.Repeat("界", 5000), nil })
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	conversation := continuityConversation(t, r, "chat", profile)
	for range 4 {
		awaitTask(t, r, continuitySend(t, r, profile, conversation, strings.Repeat("界", 5000)).ID)
	}
	v, err := scopedHistoryCall(t, r, admin, "history.read", map[string]any{"profileId": profile.ID, "conversationId": conversation.ID, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	page := v.(profileHistoryPage)
	encoded, _ := json.Marshal(page)
	if len(encoded) > 16384 || len(page.Items) > 2 || len(page.Items) == 0 || !page.HasMore || page.NextBefore == 0 {
		t.Fatalf("unbounded or unpageable history: %d %+v", len(encoded), page)
	}
	v, err = scopedHistoryCall(t, r, admin, "history.read", map[string]any{"profileId": profile.ID, "conversationId": conversation.ID, "before": page.NextBefore})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range v.(profileHistoryPage).Items {
		if item.Seq >= page.NextBefore {
			t.Fatal("history cursor repeated an exchange")
		}
	}
}
