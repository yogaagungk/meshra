package relay

import (
	"encoding/json"
	"testing"

	"meshra/internal/model"
)

func TestActivityExcludesChatWithoutAffectingTimeline(t *testing.T) {
	r, _ := openTest(t, nil)
	agent := createTest(t, r, "claude", "activity-test")
	conversation := createChat(t, r, agent.ID)
	insert := func(id, sender, conversationID string) {
		t.Helper()
		task := model.Task{ID: id, ConversationID: conversationID, From: sender, To: agent.ID, Text: id, State: "completed", CreatedAt: now(), UpdatedAt: now(), Deadline: now()}
		body, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.db.Exec("INSERT INTO tasks(id,sender,sender_session,recipient,operation,state,body) VALUES(?,?,?,?,?,?,?)", id, sender, "test", agent.ID, "request", task.State, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	insert("standalone", "agent:external", "")
	insert("chat-one", "human:operator", conversation.ID)
	insert("connector", "connector:external", conversation.ID)
	insert("chat-two", "human:operator", conversation.ID)

	value, err := r.Console(admin, "activity", model.ConsoleRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	page := value.(model.ConsolePage)
	if len(page.Items) != 1 || page.Items[0].ID != "connector" || !page.HasMore {
		t.Fatalf("first activity page: %+v", page)
	}
	value, err = r.Console(admin, "activity", model.ConsoleRequest{Limit: 1, Before: page.NextBefore})
	if err != nil {
		t.Fatal(err)
	}
	page = value.(model.ConsolePage)
	if len(page.Items) != 1 || page.Items[0].ID != "standalone" || page.HasMore {
		t.Fatalf("second activity page: %+v", page)
	}
	value, err = r.Console(admin, "timeline", model.ConsoleRequest{ConversationID: conversation.ID})
	if err != nil || len(value.(model.ConsolePage).Items) != 3 {
		t.Fatalf("chat timeline changed: %v, %v", value, err)
	}
	value, err = r.Console(admin, "tasks", model.ConsoleRequest{})
	if err != nil || len(value.(model.ConsolePage).Items) != 4 {
		t.Fatalf("task listing changed: %v, %v", value, err)
	}
}
