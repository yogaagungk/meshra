package relay

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const retentionOld = "2000-01-01T00:00:00Z"
const retentionRecent = "2099-01-01T00:00:00Z"

func retentionExec(t *testing.T, r *Relay, query string, args ...any) {
	t.Helper()
	if _, err := r.db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func retentionCount(t *testing.T, r *Relay, query string, want int, args ...any) {
	t.Helper()
	var count int
	if err := r.db.QueryRow(query, args...).Scan(&count); err != nil || count != want {
		t.Fatalf("%s: count=%d want=%d err=%v", query, count, want, err)
	}
}

func retentionFixture(t *testing.T) *Relay {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err = migrate(db); err != nil {
		t.Fatal(err)
	}
	r := &Relay{db: db, slots: map[string]*slot{}}
	retentionExec(t, r, `INSERT INTO agent_profiles(id,revision,body) VALUES('profile',1,'{}')`)
	retentionExec(t, r, `INSERT INTO conversations(id,title,owner,created_at) VALUES('conversation','','console',?)`, retentionOld)
	retentionExec(t, r, `INSERT INTO routines(id,revision,body,enabled) VALUES('routine',1,'{}',0)`)
	retentionExec(t, r, `INSERT INTO messaging_connectors(id,revision,kind,enabled,body) VALUES('connector',1,'telegram',0,'{}')`)
	return r
}

func retentionTask(t *testing.T, r *Relay, id, state string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"id": id, "state": state, "updatedAt": retentionOld})
	if err != nil {
		t.Fatal(err)
	}
	retentionExec(t, r, `INSERT INTO tasks(id,sender,sender_session,recipient,operation,state,body) VALUES(?,'source','','target','request',?,?)`, id, state, string(body))
}

func retentionRun(t *testing.T, r *Relay, id, task, stepState string, released bool) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"updatedAt": retentionOld, "conversationId": "conversation", "bindingsReleased": released, "taskIds": []string{task}})
	if err != nil {
		t.Fatal(err)
	}
	retentionExec(t, r, `INSERT INTO routine_runs(id,routine_id,revision,scheduled_at,source,state,body) VALUES(?,'routine',1,?,'webhook','completed',?)`, id, id, string(body))
	retentionExec(t, r, `INSERT INTO routine_steps(run_id,node_id,binding_id,task_id,state,body) VALUES(?,'node','binding',?,?,'{}')`, id, task, stepState)
	retentionExec(t, r, `INSERT INTO routine_trigger_receipts(routine_id,source,event_id,request_hash,run_id) VALUES('routine','webhook',?,'hash',?)`, id, id)
}

func TestAutomationRetentionTerminalAndProtected(t *testing.T) {
	r := retentionFixture(t)
	for _, task := range []struct{ id, state string }{{"done", "completed"}, {"active", "running"}, {"uncertain", "completed"}, {"returning", "completed"}} {
		retentionTask(t, r, task.id, task.state)
	}
	r.slots["returning"] = &slot{taskID: "returning"}
	retentionExec(t, r, `INSERT INTO tool_invocations(id,task_id,attempt,call_id,action_hash,state,body) VALUES('effect','uncertain',1,'call','hash','uncertain',?)`, `{"createdAt":"`+retentionOld+`"}`)
	retentionRun(t, r, "terminal", "done", "completed", true)
	retentionRun(t, r, "reserved", "", "reserved", false)
	retentionRun(t, r, "active-run", "active", "admitted", false)
	retentionRun(t, r, "uncertain-run", "uncertain", "completed", true)
	retentionRun(t, r, "returning-run", "returning", "completed", true)
	retentionRun(t, r, "unreleased", "done", "completed", false)
	for _, item := range []struct{ id, task, state string }{{"pending", "", "pending"}, {"active-inbox", "active", "admitted"}, {"done-inbox", "done", "admitted"}, {"ignored", "", "ignored_bot"}, {"reserved", "", "denied"}} {
		retentionExec(t, r, `INSERT INTO connector_inbox(connector_id,event_id,received_at,state,task_id,body) VALUES('connector',?,?,?,?, '{}')`, item.id, retentionOld, item.state, item.task)
	}
	for _, item := range []struct {
		id, task, state string
		chunk           int
	}{{"marker", "done", "sent", 0}, {"extra", "done", "sent", 1}, {"uncertain-outbox", "uncertain", "uncertain_delivery", 0}, {"pending-outbox", "active", "pending", 0}} {
		retentionExec(t, r, `INSERT INTO connector_outbox(id,task_id,chunk,state,body) VALUES(?,?,?,?,?)`, item.id, item.task, item.chunk, item.state, `{"updatedAt":"`+retentionOld+`"}`)
	}
	for _, resource := range []string{"gone", "terminal", "reserved", "active", "effect", "uncertain-outbox", "returning"} {
		retentionExec(t, r, `INSERT INTO automation_events(kind,resource_id,data,time) VALUES('feature.terminal',?,'{}',?)`, resource, retentionOld)
	}
	retentionExec(t, r, `INSERT INTO profile_memories(id,profile_id,scope,revision,content,pinned,updated_at) VALUES('memory','profile','operator',1,'keep me',0,?)`, retentionOld)
	retentionExec(t, r, `INSERT INTO local_templates(id,revision,kind,body) VALUES('template',1,'profile','{}')`)
	retentionExec(t, r, `INSERT INTO routine_flow_versions(id,revision,body) VALUES('flow',1,'{}'),('flow',2,'{}')`)
	retentionExec(t, r, `INSERT INTO template_import_previews(id,digest,expires_at,body) VALUES('preview','hash',?,'{}')`, retentionOld)
	retentionExec(t, r, `INSERT INTO routine_execution_tokens(id,token_hash,expires_at,body) VALUES('expired','expired',?,'{}'),('live','live','','{}')`, retentionOld)
	retentionExec(t, r, `INSERT INTO routine_token_grants(token_id,routine_id) VALUES('expired','routine')`)
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := r.pruneAutomationHistory(before); err != nil {
		t.Fatal(err)
	}
	retentionCount(t, r, `SELECT count(*) FROM routine_runs`, 5)
	retentionCount(t, r, `SELECT count(*) FROM routine_steps WHERE run_id='terminal'`, 0)
	retentionCount(t, r, `SELECT count(*) FROM routine_trigger_receipts WHERE run_id='terminal'`, 0)
	retentionCount(t, r, `SELECT count(*) FROM connector_inbox WHERE event_id IN ('pending','active-inbox','done-inbox')`, 3)
	// Empty task IDs and unrelated routine event IDs are not ownership links.
	retentionCount(t, r, `SELECT count(*) FROM connector_inbox WHERE event_id IN ('ignored','reserved')`, 0)
	retentionCount(t, r, `SELECT count(*) FROM connector_outbox`, 3)
	retentionCount(t, r, `SELECT count(*) FROM automation_events WHERE resource_id IN ('gone','terminal')`, 0)
	retentionCount(t, r, `SELECT count(*) FROM automation_events`, 5)
	retentionCount(t, r, `SELECT count(*) FROM profile_memories`, 1)
	retentionCount(t, r, `SELECT count(*) FROM local_templates`, 1)
	retentionCount(t, r, `SELECT count(*) FROM routine_flow_versions`, 2)
	retentionCount(t, r, `SELECT count(*) FROM template_import_previews`, 0)
	retentionCount(t, r, `SELECT count(*) FROM routine_execution_tokens`, 1)
	retentionCount(t, r, `SELECT count(*) FROM routine_token_grants`, 0)
	retentionCount(t, r, `SELECT count(*) FROM pragma_foreign_key_check`, 0)
}

func TestAutomationRetentionRequiresExpiredSensitiveArtifacts(t *testing.T) {
	r := retentionFixture(t)
	for _, item := range []struct{ id, body string }{
		{"expired", `{"updatedAt":"` + retentionOld + `","artifactsExpired":true}`},
		{"private-state", `{"updatedAt":"` + retentionOld + `"}`},
		{"snapshot", `{"updatedAt":"` + retentionOld + `","artifactsExpired":true,"snapshotText":"sensitive"}`},
		{"lease", `{"updatedAt":"` + retentionOld + `","artifactsExpired":true,"leaseOwner":"owner"}`},
	} {
		retentionExec(t, r, `INSERT INTO browser_sessions(id,profile_id,conversation_id,state,body) VALUES(?,'profile','conversation','closed',?)`, item.id, item.body)
	}
	for _, item := range []struct{ id, state, body string }{
		{"expired", "expired", `{"updatedAt":"` + retentionOld + `","audioAvailable":false}`},
		{"audio", "expired", `{"updatedAt":"` + retentionOld + `","audioFile":"private.wav","audioAvailable":true}`},
		{"completed", "completed", `{"updatedAt":"` + retentionOld + `"}`},
		{"recent", "expired", `{"updatedAt":"` + retentionRecent + `"}`},
	} {
		retentionExec(t, r, `INSERT INTO voice_jobs(id,session_owner,state,expires_at,body) VALUES(?,'owner',?,?,?)`, item.id, item.state, retentionOld, item.body)
	}
	if err := r.pruneAutomationHistory(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	retentionCount(t, r, `SELECT count(*) FROM browser_sessions`, 3)
	retentionCount(t, r, `SELECT count(*) FROM voice_jobs`, 3)
	retentionCount(t, r, `SELECT count(*) FROM browser_sessions WHERE id='expired'`, 0)
	retentionCount(t, r, `SELECT count(*) FROM voice_jobs WHERE id='expired'`, 0)
}

func TestAutomationRetentionConnectorCapacityNeverEvicts(t *testing.T) {
	r := retentionFixture(t)
	retentionTask(t, r, "task", "completed")
	tx, err := r.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range connectorPendingLimit {
		if _, err = tx.Exec(`INSERT INTO connector_inbox(connector_id,event_id,received_at,state,body) VALUES('connector',?,?,'pending','{}')`, fmt.Sprint(i), retentionOld); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO connector_outbox(id,task_id,chunk,state,body) VALUES(?,'task',?,'uncertain_delivery','{}')`, fmt.Sprint(i), i); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, capacity := range [][2]int{{1, 0}, {0, 1}, {0, connectorPendingLimit + 1}} {
		tx, err = r.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		err = reserveConnectorCapacityTx(tx, capacity[0], capacity[1])
		tx.Rollback()
		requireStatus(t, err, 429)
	}
	if err = r.pruneAutomationHistory(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	retentionCount(t, r, `SELECT count(*) FROM connector_inbox`, connectorPendingLimit)
	retentionCount(t, r, `SELECT count(*) FROM connector_outbox`, connectorPendingLimit)
	// An explicit uncertain retry reuses an occupied slot; no new capacity.
	tx, err = r.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = reserveConnectorCapacityTx(tx, 0, 0); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	retentionExec(t, r, `UPDATE connector_outbox SET state='sent' WHERE id='0'`)
	tx, err = r.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = reserveConnectorCapacityTx(tx, 0, 1); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
}

func TestAutomationRetentionPrunesTasksFKSafeAndFinalAttachmentOwner(t *testing.T) {
	r, root := openTest(t, nil)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range []string{"parent", "child", "uncertain", "reserved", "recent-delivery", "first-owner", "last-owner"} {
		retentionTask(t, r, id, "completed")
	}
	retentionExec(t, r, `UPDATE tasks SET body=json_set(body,'$.parentTaskId','parent','$.dependsOn',json('["parent"]')) WHERE id='child'`)
	retentionExec(t, r, `INSERT INTO profile_history(task_id,profile_id,conversation_id,text,reply) VALUES('child','profile','','old searchable data','reply')`)
	retentionExec(t, r, `INSERT INTO events(task_id,kind,data,time) VALUES('child','completed','old',?)`, retentionOld)
	retentionExec(t, r, `INSERT INTO tool_invocations(id,task_id,attempt,call_id,action_hash,state,body) VALUES('effect','uncertain',1,'call','hash','uncertain',?)`, `{"createdAt":"`+retentionOld+`"}`)
	retentionExec(t, r, `INSERT INTO workspace_changesets(id,invocation_id,manifest_hash,state,body) VALUES('changeset','effect','hash','uncertain','{}')`)
	retentionExec(t, r, `INSERT INTO workspace_apply_journal(changeset_id,path,state,original_hash,result_hash,error,updated_at) VALUES('changeset','file','uncertain','old','new','',?)`, retentionOld)
	retentionExec(t, r, `INSERT INTO routines(id,revision,body,enabled) VALUES('routine',1,'{}',0)`)
	retentionExec(t, r, `INSERT INTO routine_runs(id,routine_id,revision,scheduled_at,source,state,body) VALUES('reservation','routine',1,?,'manual','running',?)`, retentionOld, `{"updatedAt":"`+retentionOld+`"}`)
	retentionExec(t, r, `INSERT INTO routine_steps(run_id,node_id,binding_id,task_id,state,body) VALUES('reservation','node','binding','reserved','reserved','{}')`)
	retentionExec(t, r, `INSERT INTO connector_outbox(id,task_id,chunk,state,body) VALUES('recent','recent-delivery',0,'sent',?),('old','child',0,'sent',?)`, `{"updatedAt":"`+retentionRecent+`"}`, `{"updatedAt":"`+retentionOld+`"}`)
	retentionExec(t, r, `INSERT INTO messaging_connectors(id,revision,kind,enabled,body) VALUES('connector',1,'telegram',0,'{}')`)
	retentionExec(t, r, `INSERT INTO connector_inbox(connector_id,event_id,received_at,state,task_id,body) VALUES('connector','old-source',?,'admitted','child','{}')`, retentionOld)
	retentionExec(t, r, `INSERT INTO local_skills(id,revision,name,description) VALUES('skill',2,'Skill','')`)
	retentionExec(t, r, `INSERT INTO agent_profiles(id,revision,body) VALUES('profile',1,'{}')`)
	ids := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccc"}
	dir, err := r.attachmentDirectory()
	if err != nil {
		t.Fatal(err)
	}
	dir.Close()
	for _, id := range ids {
		retentionExec(t, r, `INSERT INTO attachments(id,sha256,size,mime,name,owner,created_at,body) VALUES(?,'hash',4,'text/plain','file','console',?,'{}')`, id, retentionOld)
		if err = os.WriteFile(filepath.Join(root, "attachments", id), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	retentionExec(t, r, `INSERT INTO attachment_tasks(attachment_id,task_id,role) VALUES(?,'child','result'),(?,'first-owner','input'),(?,'last-owner','input')`, ids[0], ids[1], ids[1])
	// A recent second task is the final owner even when the first is expired.
	retentionExec(t, r, `UPDATE tasks SET body=json_set(body,'$.updatedAt',?) WHERE id='last-owner'`, retentionRecent)
	retentionExec(t, r, `INSERT INTO local_skill_versions(skill_id,revision,sha256,body) VALUES('skill',1,'hash',?),('skill',2,'hash','{}')`, `{"assetAttachmentIds":["`+ids[2]+`"]}`)
	retentionExec(t, r, `INSERT INTO profile_skill_bindings(profile_id,skill_id,version,enabled,revision) VALUES('profile','skill',1,1,1)`)
	retentionExec(t, r, `INSERT INTO agents(id,body) VALUES('binding-agent','{}')`)
	retentionExec(t, r, `INSERT INTO credentials(hash,agent_id,session_id,task_id) VALUES('old-credential','binding-agent','session','child')`)
	retentionExec(t, r, `INSERT INTO conversations(id,title,owner,created_at) VALUES('conversation','','console',?)`, retentionOld)
	retentionExec(t, r, `INSERT INTO profile_bindings(id,profile_id,conversation_id,generation,agent_id,body) VALUES('binding','profile','conversation',1,'binding-agent','{}')`)
	retentionExec(t, r, `INSERT INTO profile_task_snapshots(task_id,profile_id,profile_revision,binding_id,body) VALUES('last-owner','profile',1,'binding',?)`, `{"skills":[{"id":"skill","version":1}]}`)
	if err = r.prune(); err != nil {
		t.Fatal(err)
	}
	retentionCount(t, r, `SELECT count(*) FROM tasks WHERE id IN ('parent','child','first-owner')`, 0)
	retentionCount(t, r, `SELECT count(*) FROM tasks WHERE id IN ('uncertain','reserved','recent-delivery','last-owner')`, 4)
	retentionCount(t, r, `SELECT count(*) FROM tool_invocations WHERE id='effect'`, 1)
	retentionCount(t, r, `SELECT count(*) FROM workspace_changesets WHERE id='changeset'`, 1)
	retentionCount(t, r, `SELECT count(*) FROM workspace_apply_journal WHERE changeset_id='changeset'`, 1)
	retentionCount(t, r, `SELECT count(*) FROM profile_history WHERE task_id='child'`, 0)
	retentionCount(t, r, `SELECT count(*) FROM events WHERE task_id='child'`, 0)
	retentionCount(t, r, `SELECT count(*) FROM connector_outbox WHERE id='old'`, 0)
	retentionCount(t, r, `SELECT count(*) FROM connector_inbox WHERE event_id='old-source'`, 0)
	retentionCount(t, r, `SELECT count(*) FROM credentials WHERE task_id='child'`, 0)
	retentionCount(t, r, `SELECT count(*) FROM attachments`, 2)
	if _, err = os.Stat(filepath.Join(root, "attachments", ids[0])); !os.IsNotExist(err) {
		t.Fatalf("final-owner bytes remain: %v", err)
	}
	for _, id := range ids[1:] {
		if _, err = os.Stat(filepath.Join(root, "attachments", id)); err != nil {
			t.Fatalf("referenced bytes removed: %v", err)
		}
	}
	retentionCount(t, r, `SELECT count(*) FROM local_skill_versions`, 2)
	retentionCount(t, r, `SELECT count(*) FROM profile_skill_bindings WHERE version=1`, 1)
	retentionCount(t, r, `SELECT count(*) FROM profile_task_snapshots WHERE task_id='last-owner'`, 1)
	retentionCount(t, r, `SELECT count(*) FROM profile_bindings WHERE id='binding'`, 1)
	retentionCount(t, r, `SELECT count(*) FROM pragma_foreign_key_check`, 0)
}

func TestAutomationRetentionNegativeLegacyAndBoundedBatches(t *testing.T) {
	r, _ := openTest(t, nil)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.options.Retention = -time.Hour
	retentionTask(t, r, "legacy", "completed")
	for i := range automationRetentionBatch + 7 {
		retentionExec(t, r, `INSERT INTO automation_events(kind,resource_id,data,time) VALUES('terminal',?,'{}',?)`, fmt.Sprint(i), retentionOld)
	}
	if err := r.prune(); err != nil {
		t.Fatal(err)
	}
	retentionCount(t, r, `SELECT count(*) FROM tasks WHERE id='legacy'`, 1)
	retentionCount(t, r, `SELECT count(*) FROM automation_events WHERE kind='terminal'`, 0)
}
