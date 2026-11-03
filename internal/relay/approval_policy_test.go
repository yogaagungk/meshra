package relay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"meshra/internal/model"
)

func TestApprovalRuleMatching(t *testing.T) {
	cmd := func(argv []string, shell string) *toolAction {
		return &toolAction{args: &preparedCommand{input: toolCommandArgs{Argv: argv, Shell: shell}}}
	}
	cases := []struct {
		name   string
		rule   approvalRule
		action *toolAction
		want   bool
	}{
		{"exact quoted argument", approvalRule{MatcherKind: "argv_exact", Matcher: `["echo","a b"]`}, cmd([]string{"echo", "a b"}, ""), true},
		{"quoting is not shell parsing", approvalRule{MatcherKind: "argv_exact", Matcher: `["echo","a b"]`}, cmd([]string{"echo", "a", "b"}, ""), false},
		{"exact rejects extra", approvalRule{MatcherKind: "argv_exact", Matcher: `["git","status"]`}, cmd([]string{"git", "status", "--short"}, ""), false},
		{"prefix", approvalRule{MatcherKind: "argv_prefix", Matcher: `["git","status"]`}, cmd([]string{"git", "status", "--short"}, ""), true},
		{"shell never matches", approvalRule{MatcherKind: "argv_prefix", Matcher: `["echo"]`}, cmd([]string{"echo", "x"}, "echo x"), false},
		{"glob", approvalRule{MatcherKind: "path_glob", Matcher: "docs/*.txt"}, &toolAction{args: &toolWriteArgs{Path: "docs/a.txt"}}, true},
		{"glob no crossing directories", approvalRule{MatcherKind: "path_glob", Matcher: "docs/*.txt"}, &toolAction{args: &toolWriteArgs{Path: "docs/sub/a.txt"}}, false},
		{"traversal", approvalRule{MatcherKind: "path_glob", Matcher: "docs/*"}, &toolAction{args: &toolWriteArgs{Path: "docs/../secret"}}, false},
		{"absolute", approvalRule{MatcherKind: "path_glob", Matcher: "*"}, &toolAction{args: &toolWriteArgs{Path: "/secret"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := approvalRuleMatches(tc.rule, tc.action); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApprovalPolicyRuleLifecycle(t *testing.T) {
	r, _ := openTest(t, nil)
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	p := brokerConsole(r, "policy-test")
	invoke := func(op string, q map[string]any) (any, error) {
		t.Helper()
		q["profileId"] = profile.ID
		raw, _ := json.Marshal(q)
		return r.approvalPolicyOperation(context.Background(), p, op, raw)
	}
	var before int
	if err := r.db.QueryRow("SELECT revision FROM profile_tool_policies WHERE profile_id=?", profile.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	v, err := invoke("approval_policy.get", map[string]any{})
	if err != nil || v.(map[string]any)["policy"] != "manual" {
		t.Fatalf("default: %v %v", v, err)
	}
	if _, err = invoke("approval_policy.set", map[string]any{"policy": "allowlist"}); err != nil {
		t.Fatal(err)
	}
	v, err = invoke("approval_rules.create", map[string]any{"tool": "meshra_workspace_command", "matcherKind": "argv_exact", "matcher": `["echo","a b"]`, "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	rule := v.(approvalRule)
	b := &toolBroker{relay: r, snapshot: model.ProfileTaskSnapshot{Profile: profile, Binding: model.ProfileBinding{Harness: "openai"}, ConsoleSessionOwner: "policy-test"}}
	action := &toolAction{args: &preparedCommand{input: toolCommandArgs{Argv: []string{"echo", "a b"}}}}
	matched, err := b.matchingApprovalRuleLocked(action, "meshra_workspace_command")
	if err != nil || matched != rule.ID {
		t.Fatalf("match %q: %v", matched, err)
	}
	if _, err = r.db.Exec("UPDATE tool_allow_rules SET expires_at=? WHERE id=?", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), rule.ID); err != nil {
		t.Fatal(err)
	}
	matched, err = b.matchingApprovalRuleLocked(action, "meshra_workspace_command")
	if err != nil || matched != "" {
		t.Fatalf("expired match %q: %v", matched, err)
	}
	b.snapshot.External = true
	if _, err = r.db.Exec("UPDATE tool_allow_rules SET expires_at=NULL WHERE id=?", rule.ID); err != nil {
		t.Fatal(err)
	}
	matched, err = b.matchingApprovalRuleLocked(action, "meshra_workspace_command")
	if err != nil || matched != "" {
		t.Fatalf("unattended match %q: %v", matched, err)
	}
	var after int
	if err = r.db.QueryRow("SELECT revision FROM profile_tool_policies WHERE profile_id=?", profile.ID).Scan(&after); err != nil || after != before+2 {
		t.Fatalf("revision %d -> %d: %v", before, after, err)
	}
	v, err = invoke("approval_rules.list", map[string]any{})
	if err != nil || len(v.(map[string]any)["items"].([]approvalRule)) != 1 {
		t.Fatalf("list: %v %v", v, err)
	}
	if _, err = invoke("approval_rules.revoke", map[string]any{"id": rule.ID}); err != nil {
		t.Fatal(err)
	}
	var revoked string
	var revision int
	if err = r.db.QueryRow("SELECT revoked_at,revision FROM tool_allow_rules WHERE id=?", rule.ID).Scan(&revoked, &revision); err != nil || revoked == "" || revision != 2 {
		t.Fatalf("revocation audit: %q %d %v", revoked, revision, err)
	}
	b.snapshot.External = false
	matched, err = b.matchingApprovalRuleLocked(action, "meshra_workspace_command")
	if err != nil || matched != "" {
		t.Fatalf("revoked match %q: %v", matched, err)
	}
	if _, err = invoke("approval_rules.create", map[string]any{"tool": "meshra_external_mcp", "matcherKind": "argv_exact", "matcher": `["echo"]`}); err == nil {
		t.Fatal("external MCP rule accepted")
	}
	if _, err = invoke("approval_rules.create", map[string]any{"tool": "meshra_workspace_write", "matcherKind": "path_glob", "matcher": "../*"}); err == nil {
		t.Fatal("traversal rule accepted")
	}
	if _, err = invoke("approval_rules.create", map[string]any{"tool": "meshra_workspace_command", "matcherKind": "argv_exact", "matcher": `["echo"]`, "expiresAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}); err == nil {
		t.Fatal("expired rule accepted")
	}
}

func TestApprovalPolicyChangeInvalidatesPending(t *testing.T) {
	script := newBrokerScript(brokerStep{call: model.ToolCall{ID: "question", Name: "meshra_ask_user", Arguments: json.RawMessage(`{"question":"Continue?"}`)}})
	r, _ := continuityOpenTest(t, script.run)
	console := brokerConsole(r, "policy-invalidation")
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, console, profile, conversation, "ask")
	id := awaitBrokerPendingApproval(t, r, task.ID)
	raw, _ := json.Marshal(map[string]any{"profileId": profile.ID, "policy": "allowlist"})
	if _, err := r.approvalPolicyOperation(context.Background(), console, "approval_policy.set", raw); err != nil {
		t.Fatal(err)
	}
	if status := awaitBrokerApprovalStatus(t, r, id, "invalidated"); status != "invalidated" {
		t.Fatalf("approval status %s", status)
	}
	outcome := script.await(t, task.ID, 0, 10*time.Second)
	if outcome.err == nil {
		t.Fatal("invalidated action succeeded")
	}
}
