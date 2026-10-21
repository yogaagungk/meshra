package relay

import (
	"encoding/json"
	"fmt"
	"meshra/internal/model"
	"strings"
	"testing"
)

func TestConsoleHumanChatIsolationAndReplay(t *testing.T) {
	r, _ := openTest(t, nil)
	prod := createTest(t, r, "claude", "prod")
	wallet := createTest(t, r, "claude", "wallet")
	worker := createTest(t, r, "omp", "worker")
	startTest(t, r, prod.ID)
	startTest(t, r, wallet.ID)
	startTest(t, r, worker.ID)
	v, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "human chat", Participants: []string{prod.ID, wallet.ID, worker.ID}})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.Conversation)
	q := model.ConsoleRequest{ConversationID: c.ID, Recipients: []string{prod.ID}, Text: "Human message", IdempotencyKey: "stable-browser-key"}
	v, err = r.Console(admin, "send", q)
	if err != nil {
		t.Fatal(err)
	}
	tasks := v.(map[string]any)["tasks"].([]model.Task)
	if len(tasks) != 1 {
		t.Fatal(v)
	}
	first := awaitTask(t, r, tasks[0].ID)
	if first.State != "completed" || first.From != "human:operator" || first.OriginAgentID != "human:operator" || first.ResultRef == nil {
		t.Fatalf("human origin lost %+v", first)
	}
	v, err = r.Console(admin, "send", q)
	if err != nil || v.(map[string]any)["tasks"].([]model.Task)[0].ID != first.ID {
		t.Fatal("duplicate send", v, err)
	}
	v, err = r.Console(admin, "timeline", model.ConsoleRequest{ConversationID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	page := v.(model.ConsolePage)
	if len(page.Items) != 1 || page.Items[0].To != prod.ID || !strings.Contains(page.Items[0].Result, "Human message") {
		t.Fatal("timeline leakage", page)
	}
	v, err = r.Console(admin, "feed", model.ConsoleRequest{ConversationID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	events := v.([]map[string]any)
	if len(events) == 0 {
		t.Fatal("no real task events")
	}
	last := events[len(events)-1]["id"].(int64)
	v, err = r.Console(admin, "feed", model.ConsoleRequest{ConversationID: c.ID, After: last})
	if err != nil || len(v.([]map[string]any)) != 0 {
		t.Fatal("duplicate replay", v, err)
	}
	_, err = r.Console(model.Principal{AgentID: wallet.ID}, "timeline", model.ConsoleRequest{ConversationID: c.ID})
	requireStatus(t, err, 403)
	agents, err := r.Agents(admin)
	if err != nil || len(agents) != 3 {
		t.Fatal("human registered as harness", agents, err)
	}
}
func TestConsoleRecipientContextAuthorization(t *testing.T) {
	r, _ := openTest(t, nil)
	a := createTest(t, r, "claude", "prod")
	b := createTest(t, r, "claude", "wallet")
	v, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "private", Participants: []string{a.ID, b.ID}})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.Conversation)
	v, err = r.Collaborate(model.Principal{AgentID: a.ID}, "context.publish", model.CollaborationRequest{Content: "private data"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := v.(model.SharedContext)
	q := model.ConsoleRequest{ConversationID: c.ID, Recipients: []string{a.ID, b.ID}, Text: "Use data", ContextRefs: []model.ContextRef{{ContextID: ctx.ID, Version: 1}}, IdempotencyKey: "context-browser-key"}
	v, err = r.Console(admin, "context-check", q)
	if err != nil {
		t.Fatal(err)
	}
	denied := v.(map[string]any)["denied"].([]string)
	if len(denied) != 1 || denied[0] != b.ID {
		t.Fatal("recipient check bypass", v)
	}
	v, err = r.Console(admin, "send", q)
	if err != nil {
		t.Fatal(err)
	}
	out := v.(map[string]any)
	if len(out["tasks"].([]model.Task)) != 1 || len(out["errors"].(map[string]string)) != 1 {
		t.Fatal("partial admission hidden", out)
	}
}

func TestConsoleThousandMessagePaginationAndBurstReplay(t *testing.T) {
	r, _ := openTest(t, nil)
	a := createTest(t, r, "omp", "paged")
	v, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "pagination", Participants: []string{a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.Conversation)
	// Seed persisted terminal tasks directly in this isolated storage regression;
	// no native execution or provider success is claimed by the fixture.
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		tx, err := r.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for i := 0; i < 1005; i++ {
			task := model.Task{ID: randomID(), MessageID: randomID(), ConversationID: c.ID, From: "human:operator", FromSessionID: "console", To: a.ID, Text: fmt.Sprintf("Paged message %04d", i), Result: "Stored result", State: "completed", CreatedAt: now(), UpdatedAt: now(), Deadline: now()}
			body, _ := json.Marshal(task)
			if _, err = tx.Exec("INSERT INTO tasks(id,sender,sender_session,recipient,operation,state,body) VALUES(?,?,?,?,?,?,?)", task.ID, task.From, "console", task.To, "request", "completed", string(body)); err != nil {
				t.Fatal(err)
			}
			if err = insertEvent(tx, task, "completed", ""); err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}()
	seen := map[string]bool{}
	before := int64(0)
	pages := 0
	for {
		v, err = r.Console(admin, "timeline", model.ConsoleRequest{ConversationID: c.ID, Before: before, Limit: 40})
		if err != nil {
			t.Fatal(err)
		}
		page := v.(model.ConsolePage)
		if len(page.Items) > 40 {
			t.Fatal("unbounded timeline")
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate page boundary")
			}
			seen[item.ID] = true
		}
		pages++
		if !page.HasMore {
			break
		}
		if page.NextBefore <= 0 || page.NextBefore == before {
			t.Fatal("cursor did not advance")
		}
		before = page.NextBefore
	}
	if len(seen) != 1005 || pages != 26 {
		t.Fatalf("missing messages: %d across %d pages", len(seen), pages)
	}
	after := int64(0)
	events := 0
	for {
		v, err = r.Console(admin, "feed", model.ConsoleRequest{ConversationID: c.ID, After: after})
		if err != nil {
			t.Fatal(err)
		}
		batch := v.([]map[string]any)
		if len(batch) > 256 {
			t.Fatal("unbounded event batch")
		}
		if len(batch) == 0 {
			break
		}
		for _, event := range batch {
			seq := event["id"].(int64)
			if seq <= after {
				t.Fatal("event replay duplicated cursor")
			}
			after = seq
			events++
		}
	}
	if events != 1005 {
		t.Fatalf("lost burst events: %d", events)
	}
}

func TestConversationMetadataAndRemovalGuards(t *testing.T) {
	r, _ := openTest(t, nil)
	owner := createTest(t, r, "claude", "owner")
	other := createTest(t, r, "claude", "wallet")
	p := model.Principal{AgentID: owner.ID}
	v, err := r.Collaborate(p, "conversation.create", model.CollaborationRequest{Title: "owned", Participants: []string{other.ID}})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.Conversation)
	_, err = r.Collaborate(model.Principal{AgentID: other.ID}, "conversation.rename", model.CollaborationRequest{ID: c.ID, Title: "escalated"})
	requireStatus(t, err, 403)
	v, err = r.Console(admin, "send", model.ConsoleRequest{ConversationID: c.ID, Recipients: []string{other.ID}, Text: "queued", IdempotencyKey: "member-guard-001"})
	if err != nil {
		t.Fatal(err)
	}
	task := v.(map[string]any)["tasks"].([]model.Task)[0]
	_, err = r.Collaborate(p, "conversation.removeParticipant", model.CollaborationRequest{ID: c.ID, AgentID: other.ID})
	requireStatus(t, err, 409)
	if _, err = r.Cancel(admin, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Collaborate(p, "conversation.removeParticipant", model.CollaborationRequest{ID: c.ID, AgentID: other.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(model.Principal{AgentID: other.ID}, "conversation.get", model.CollaborationRequest{ID: c.ID})
	requireStatus(t, err, 403)
	if _, err = r.Collaborate(p, "conversation.rename", model.CollaborationRequest{ID: c.ID, Title: "renamed"}); err != nil {
		t.Fatal(err)
	}
	v, err = r.Console(admin, "send", model.ConsoleRequest{ConversationID: c.ID, Recipients: []string{other.ID}, Text: "must not route", IdempotencyKey: "member-guard-002"})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.(map[string]any)["tasks"].([]model.Task)) != 0 {
		t.Fatal("removed participant received a task")
	}
}
