package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"meshra/internal/model"
)

func createChat(t *testing.T, r *Relay, ai string) model.Conversation {
	t.Helper()
	v, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "New chat", Mode: "chat", Participants: []string{ai}})
	if err != nil {
		t.Fatal(err)
	}
	return v.(model.Conversation)
}
func sendChat(t *testing.T, r *Relay, c model.Conversation, text string) model.Task {
	t.Helper()
	v, err := r.Console(admin, "send", model.ConsoleRequest{ConversationID: c.ID, Text: text, Recipients: c.Participants, IdempotencyKey: randomID()})
	if err != nil {
		t.Fatal(err)
	}
	out := v.(map[string]any)
	ts := out["tasks"].([]model.Task)
	if len(ts) != 1 {
		t.Fatalf("admission: %+v", out)
	}
	return ts[0]
}
func waitChatIdle(t *testing.T, r *Relay, ai string) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		a, err := r.Agent(admin, ai)
		if err == nil && a.State == "idle" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("chat did not release its turn")
}

func TestChatIsolationContextAndRegeneration(t *testing.T) {
	prompts := make(chan model.Turn, 10)
	r, _ := openTest(t, func(_ context.Context, turn model.Turn, _ model.Emit) (string, error) {
		prompts <- turn
		return "A useful answer", nil
	})
	ai := createTest(t, r, "claude", "assistant")
	original := startTest(t, r, ai.ID)
	one := createChat(t, r, ai.ID)
	two := createChat(t, r, ai.ID)
	if one.Mode != "chat" {
		t.Fatal("mode lost")
	}
	first := awaitTask(t, r, sendChat(t, r, one, "My favorite color is indigo").ID)
	if first.State != "completed" || !first.Chat {
		t.Fatalf("first %+v", first)
	}
	prompt1 := <-prompts
	second := awaitTask(t, r, sendChat(t, r, one, "What color did I mention?").ID)
	prompt2 := <-prompts
	if second.State != "completed" || !strings.Contains(prompt2.Task.Text, "indigo") || !strings.Contains(prompt2.Task.Text, "A useful answer") {
		t.Fatal("follow-up context missing", prompt2.Task.Text)
	}
	if prompt1.Task.DestinationSessionID == prompt2.Task.DestinationSessionID {
		t.Fatal("native session accumulated history")
	}
	other := awaitTask(t, r, sendChat(t, r, two, "Hello from a separate chat").ID)
	prompt3 := <-prompts
	if other.State != "completed" || strings.Contains(prompt3.Task.Text, "indigo") {
		t.Fatal("cross-chat history leak")
	}
	waitChatIdle(t, r, ai.ID)
	a, _ := r.Agent(admin, ai.ID)
	if a.SessionID != original.SessionID {
		t.Fatal("chat replaced durable agent session")
	}
	_, err := r.Console(admin, "regenerate", model.ConsoleRequest{TaskID: first.ID})
	requireStatus(t, err, 409)
	_, err = r.Console(admin, "regenerate", model.ConsoleRequest{TaskID: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	regenerated := awaitTask(t, r, second.ID)
	prompt4 := <-prompts
	if regenerated.Attempts != 2 || prompt4.Task.Text != prompt2.Task.Text {
		t.Fatal("regeneration included replaced answer", prompt4.Task.Text)
	}
	page, err := r.Console(admin, "timeline", model.ConsoleRequest{ConversationID: one.ID})
	if err != nil || len(page.(model.ConsolePage).Items) != 2 {
		t.Fatal("regeneration duplicated user message", page, err)
	}
	_, err = r.Collaborate(admin, "conversation.addParticipant", model.CollaborationRequest{ID: one.ID, AgentID: ai.ID})
	requireStatus(t, err, 409)
	_, err = r.Collaborate(admin, "conversation.removeParticipant", model.CollaborationRequest{ID: one.ID, AgentID: ai.ID})
	requireStatus(t, err, 409)
}

func TestChatCancellationAndSingleActiveTurn(t *testing.T) {
	entered := make(chan model.Turn, 1)
	var runs atomic.Int32
	r, _ := openTest(t, func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
		if runs.Add(1) == 1 {
			entered <- turn
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "Continued successfully", nil
	})
	ai := createTest(t, r, "codex", "assistant")
	startTest(t, r, ai.ID)
	c := createChat(t, r, ai.ID)
	pending := sendChat(t, r, c, "Wait until stopped")
	turn := <-entered
	_, err := r.Send(model.Principal{AgentID: ai.ID, SessionID: turn.Task.DestinationSessionID, TaskID: pending.ID}, model.SendRequest{To: ai.ID, Text: "Delegate", ConversationID: c.ID})
	requireStatus(t, err, 403)
	v, err := r.Console(admin, "send", model.ConsoleRequest{ConversationID: c.ID, Text: "Too early", Recipients: c.Participants, IdempotencyKey: randomID()})
	if err != nil || len(v.(map[string]any)["tasks"].([]model.Task)) != 0 {
		t.Fatal("overlapping chat accepted", v, err)
	}
	// Admission retry must still return the existing task while it is active.
	status, err := r.Console(admin, "chat-state", model.ConsoleRequest{ConversationID: c.ID})
	if err != nil || status.(map[string]any)["taskId"] != pending.ID {
		t.Fatal("active state missing", status, err)
	}
	if _, err = r.Cancel(admin, pending.ID); err != nil {
		t.Fatal(err)
	}
	waitChatIdle(t, r, ai.ID)
	next := awaitTask(t, r, sendChat(t, r, c, "Continue").ID)
	if next.State != "completed" {
		t.Fatal("cannot continue after cancellation", next)
	}
}

func TestChatHistoryBudgetAndTimelineByteBudget(t *testing.T) {
	r, _ := openTest(t, nil)
	ai := createTest(t, r, "omp", "assistant")
	c := createChat(t, r, ai.ID)
	// Seed large completed turns; context building must not grow with history.
	r.mu.Lock()
	for i := 0; i < 30; i++ {
		task := model.Task{ID: randomID(), ConversationID: c.ID, From: "human:operator", To: ai.ID, Text: strings.Repeat("界", 10000), Result: strings.Repeat("a", 100000), State: "completed", CreatedAt: now(), UpdatedAt: now(), Deadline: now()}
		body, _ := json.Marshal(task)
		if _, err := r.db.Exec("INSERT INTO tasks(id,sender,sender_session,recipient,operation,state,body) VALUES(?,?,?,?,?,?,?)", task.ID, task.From, "console", task.To, "request", task.State, string(body)); err != nil {
			r.mu.Unlock()
			t.Fatal(err)
		}
	}
	r.mu.Unlock()
	queued := sendChat(t, r, c, "Current question")
	r.mu.Lock()
	prompt, err := r.chatPrompt(queued, "")
	r.mu.Unlock()
	if err != nil || len(prompt) > chatHistoryBytes+2048 || !strings.Contains(prompt, "Current question") || !json.Valid([]byte(strings.SplitN(prompt, "\n\n", 2)[1])) {
		t.Fatal("unbounded or invalid prompt", len(prompt), err)
	}
	v, err := r.Console(admin, "timeline", model.ConsoleRequest{ConversationID: c.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	page := v.(model.ConsolePage)
	if !page.HasMore || len(page.Items) >= 30 {
		t.Fatal("byte budget did not constrain page")
	}
	total := 0
	for _, item := range page.Items {
		body, _ := json.Marshal(item.Task)
		total += len(body)
	}
	if total > 1024*1024 {
		t.Fatal("timeline byte budget exceeded", total)
	}
	v, err = r.Console(admin, "timeline", model.ConsoleRequest{ConversationID: c.ID, Before: page.NextBefore, Limit: 100})
	if err != nil || len(v.(model.ConsolePage).Items) == 0 {
		t.Fatal("byte-limited pagination lost cursor", v, err)
	}
}

type countedChatRuntime struct {
	*fakeRuntime
	open   *atomic.Int32
	closed atomic.Bool
}

func (c *countedChatRuntime) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.open.Add(-1)
	}
	return nil
}

func TestChatRuntimeResourcesReleasedAcrossLongConversation(t *testing.T) {
	var open atomic.Int32
	adapters := map[string]model.Adapter{"claude": {Factory: func(_ context.Context, a model.Agent, _ model.Emit) (model.Runtime, error) {
		if open.Add(1) > 2 {
			t.Error("chat retained old native runtimes")
		}
		return &countedChatRuntime{fakeRuntime: &fakeRuntime{session: "native-" + randomID(), run: func(_ context.Context, turn model.Turn, _ model.Emit) (string, error) {
			if len(turn.Task.Text) > chatHistoryBytes+2048 {
				t.Error("history grew beyond budget")
			}
			return strings.Repeat("response ", 1000), nil
		}}, open: &open}, nil
	}}}
	r, err := Open(t.TempDir(), adapters)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	ai := createTest(t, r, "claude", "bounded")
	startTest(t, r, ai.ID)
	c := createChat(t, r, ai.ID)
	for i := 0; i < 30; i++ {
		reply := awaitTask(t, r, sendChat(t, r, c, "Follow-up question").ID)
		if reply.State != "completed" {
			t.Fatal(reply)
		}
		waitChatIdle(t, r, ai.ID)
		if open.Load() != 1 {
			t.Fatal("temporary native runtime retained", open.Load())
		}
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if open.Load() != 0 {
		t.Fatal("shutdown retained native runtimes", open.Load())
	}
}

func TestChatSchemaUpgradePreservesExistingConversations(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "meshra.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{migration, migrationV2, migrationV3, migrationV4, migrationV5} {
		if _, err = db.Exec(source); err != nil {
			t.Fatal(err)
		}
	}
	ai := model.Agent{ID: "claude:existing", Harness: "claude", Name: "existing", Cwd: t.TempDir(), State: "registered"}
	body, _ := json.Marshal(ai)
	if _, err = db.Exec("INSERT INTO agents(id,body) VALUES(?,?)", ai.ID, string(body)); err != nil {
		t.Fatal(err)
	}
	existing := model.Conversation{ID: randomID(), Title: "Existing conversation"}
	if _, err = db.Exec("INSERT INTO conversations(id,title,owner,created_at) VALUES(?,?,?,?)", existing.ID, existing.Title, "admin", now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO conversation_participants(conversation_id,agent_id) VALUES(?,?)", existing.ID, ai.ID); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v, err := reopened.Collaborate(admin, "conversation.get", model.CollaborationRequest{ID: existing.ID})
	if err != nil {
		t.Fatal(err)
	}
	loaded := v.(model.Conversation)
	if loaded.Title != existing.Title || loaded.Mode != "" || len(loaded.Participants) != 1 {
		t.Fatal("existing conversation changed during migration", loaded)
	}
	chat := createChat(t, reopened, ai.ID)
	if chat.Mode != "chat" {
		t.Fatal("upgraded database does not support chat")
	}
}

func TestChatCancelDuringNativeStartupDoesNotReviveTask(t *testing.T) {
	var factories atomic.Int32
	var open atomic.Int32
	starting := make(chan struct{})
	adapters := map[string]model.Adapter{"claude": {Factory: func(ctx context.Context, a model.Agent, _ model.Emit) (model.Runtime, error) {
		if factories.Add(1) > 1 {
			close(starting)
			<-ctx.Done()
		}
		open.Add(1)
		return &countedChatRuntime{fakeRuntime: &fakeRuntime{session: randomID(), run: func(context.Context, model.Turn, model.Emit) (string, error) {
			t.Error("cancelled startup was executed")
			return "", nil
		}}, open: &open}, nil
	}}}
	r, err := Open(t.TempDir(), adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ai := createTest(t, r, "claude", "startup")
	startTest(t, r, ai.ID)
	c := createChat(t, r, ai.ID)
	task := sendChat(t, r, c, "Cancel while starting")
	<-starting
	if _, err = r.Cancel(admin, task.ID); err != nil {
		t.Fatal(err)
	}
	waitChatIdle(t, r, ai.ID)
	saved, err := r.Task(admin, task.ID)
	if err != nil || saved.State != "cancelled" {
		t.Fatal("native startup revived cancelled task", saved, err)
	}
	if open.Load() != 1 {
		t.Fatal("startup runtime was not released", open.Load())
	}
}
