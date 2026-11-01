package relay

import (
	"meshra/internal/model"
	"strings"
	"testing"
)

func TestDeleteConversationConfirmationActiveTasksAndPersistence(t *testing.T) {
	r, dir := openTest(t, nil)
	value, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "Delete me"})
	if err != nil {
		t.Fatal(err)
	}
	id := value.(model.Conversation).ID
	q := model.ConsoleRequest{ConversationID: id}
	_, err = r.Console(admin, "chat-delete", q)
	requireStatus(t, err, 400)
	q.Confirm = true
	_, err = r.Console(model.Principal{}, "chat-delete", q)
	requireStatus(t, err, 403)
	_, err = r.db.Exec(`INSERT INTO tasks(id,sender,sender_session,recipient,operation,state,body) VALUES('delete-task','human','session','agent','request','queued',?)`, `{"conversationId":"`+id+`","text":"private transcript"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Console(admin, "chat-delete", q)
	requireStatus(t, err, 409)
	if _, err = r.db.Exec(`UPDATE tasks SET state='completed' WHERE id='delete-task'`); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Console(admin, "chat-delete", q); err != nil {
		t.Fatal(err)
	}
	_, err = r.Collaborate(admin, "conversation.get", model.CollaborationRequest{ID: id})
	requireStatus(t, err, 404)
	var count int
	if err = r.db.QueryRow(`SELECT count(*) FROM tasks WHERE id='delete-task'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("task count %d: %v", count, err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, err = reopened.Collaborate(admin, "conversation.get", model.CollaborationRequest{ID: id})
	requireStatus(t, err, 404)
}

func TestDeleteConversationPreservesChangesetAudit(t *testing.T) {
	r, _ := openTest(t, nil)
	value, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "Changes"})
	if err != nil {
		t.Fatal(err)
	}
	id := value.(model.Conversation).ID
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tasks(id,sender,sender_session,recipient,operation,state,body) VALUES('change-task','human','session','agent','request','completed',?)`, []any{`{"conversationId":"` + id + `","text":"private","result":"private reply"}`}},
		{`INSERT INTO tool_invocations(id,task_id,attempt,call_id,action_hash,state,body) VALUES('change-call','change-task',1,'call','hash','completed','{}')`, nil},
		{`INSERT INTO workspace_changesets(id,invocation_id,manifest_hash,state,body) VALUES('change-set','change-call','hash','applied','{}')`, nil},
	} {
		if _, err = r.db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = r.Console(admin, "chat-delete", model.ConsoleRequest{ConversationID: id, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	var body string
	if err = r.db.QueryRow(`SELECT body FROM tasks WHERE id='change-task'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "private") || strings.Contains(body, id) {
		t.Fatalf("retained task contains transcript: %s", body)
	}
	var count int
	if err = r.db.QueryRow(`SELECT count(*) FROM workspace_changesets WHERE id='change-set'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("changeset count %d: %v", count, err)
	}
}
