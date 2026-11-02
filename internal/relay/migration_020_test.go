package relay

import (
	"testing"
)

// TestMigration020RemovesRetiredHarnesses seeds data that only older builds
// could create (Codex, OpenCode and ACP) and checks that schema 20 removes it
// and its dependents while an OMP agent and its chat remain.
func TestMigration020RemovesRetiredHarnesses(t *testing.T) {
	r, _ := openTest(t, nil)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := r.db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec("PRAGMA foreign_keys=OFF")
	mustExec("CREATE TABLE acp_agents(id TEXT PRIMARY KEY, revision INTEGER NOT NULL, body TEXT NOT NULL)")
	mustExec("INSERT INTO acp_agents VALUES('acp1',1,'{}')")
	mustExec("INSERT INTO automation_secrets(id,kind,nonce,ciphertext,updated_at) VALUES('s1','acp.env',x'00',x'00','t'),('s2','endpoint.apiKey',x'00',x'00','t')")
	for _, a := range []struct{ id, harness string }{{"codex:old", "codex"}, {"opencode:old", "opencode"}, {"acp:old", "acp"}, {"omp:keep", "omp"}} {
		mustExec("INSERT INTO agents(id,body) VALUES(?,?)", a.id, `{"id":"`+a.id+`","harness":"`+a.harness+`"}`)
	}
	for _, c := range []struct{ id, agent string }{{"c-old", "codex:old"}, {"c-keep", "omp:keep"}} {
		mustExec("INSERT INTO conversations(id,title,owner,created_at) VALUES(?,?,?,?)", c.id, "t", "human:operator", "now")
		mustExec("INSERT INTO conversation_participants VALUES(?,?)", c.id, c.agent)
		body := `{"taskId":"t-` + c.id + `","conversationId":"` + c.id + `","to":"` + c.agent + `","state":"completed"}`
		mustExec("INSERT INTO tasks(id,sender,sender_session,recipient,operation,state,body) VALUES(?,?,?,?,?,?,?)", "t-"+c.id, "human:operator", "console", c.agent, "request", "completed", body)
		mustExec("INSERT INTO events(task_id,kind,data,time) VALUES(?,?,?,?)", "t-"+c.id, "delta", "x", "now")
	}
	mustExec("INSERT INTO agent_profiles VALUES('p-old',1,?)", `{"id":"p-old","binding":{"harness":"codex"}}`)
	mustExec("INSERT INTO agent_profiles VALUES('p-keep',1,?)", `{"id":"p-keep","binding":{"harness":"omp"}}`)

	if _, err := r.db.Exec(migrationV20); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := r.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("SELECT count(*) FROM agents WHERE id IN ('codex:old','opencode:old','acp:old')"); n != 0 {
		t.Fatalf("retired agents remain: %d", n)
	}
	if n := count("SELECT count(*) FROM agents WHERE id='omp:keep'"); n != 1 {
		t.Fatal("the OMP agent was removed")
	}
	if n := count("SELECT count(*) FROM conversations WHERE id='c-old'") + count("SELECT count(*) FROM tasks WHERE id='t-c-old'") + count("SELECT count(*) FROM events WHERE task_id='t-c-old'"); n != 0 {
		t.Fatalf("retired chat data remains: %d", n)
	}
	if n := count("SELECT count(*) FROM conversations WHERE id='c-keep'") + count("SELECT count(*) FROM tasks WHERE id='t-c-keep'") + count("SELECT count(*) FROM events WHERE task_id='t-c-keep'"); n != 3 {
		t.Fatalf("kept chat data was damaged: %d", n)
	}
	if count("SELECT count(*) FROM agent_profiles WHERE id='p-old'") != 0 || count("SELECT count(*) FROM agent_profiles WHERE id='p-keep'") != 1 {
		t.Fatal("profile cleanup is wrong")
	}
	if count("SELECT count(*) FROM automation_secrets WHERE kind='acp.env'") != 0 || count("SELECT count(*) FROM automation_secrets WHERE id='s2'") != 1 {
		t.Fatal("secret cleanup is wrong")
	}
	if count("SELECT count(*) FROM sqlite_master WHERE name='acp_agents'") != 0 {
		t.Fatal("acp_agents was not dropped")
	}
}
