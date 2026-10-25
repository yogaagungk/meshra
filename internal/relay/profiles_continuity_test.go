package relay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

// These deterministic relay fixtures opt into the test executor, not a claim
// about any installed native adapter's broker capability.
func continuityOpenTest(t *testing.T, run func(context.Context, model.Turn, model.Emit) (string, error)) (*Relay, string) {
	t.Helper()
	r, dir := openTest(t, run)
	r.mu.Lock()
	for harness, adapter := range r.adapters {
		adapter.Capabilities["profileAutomation"] = true
		r.adapters[harness] = adapter
	}
	r.mu.Unlock()
	return r, dir
}

func continuityCall(t *testing.T, r *Relay, operation string, payload any) (any, error) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return r.profileContinuityOperation(context.Background(), admin, operation, body)
}

func continuityConversation(t *testing.T, r *Relay, kind string, profiles ...model.Profile) model.Conversation {
	t.Helper()
	ids := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		ids = append(ids, profile.ID)
	}
	v, err := continuityCall(t, r, "profiles.createConversation", map[string]any{"title": "Scoped conversation", "kind": kind, "profileIds": ids})
	if err != nil {
		t.Fatal(err)
	}
	return v.(model.Conversation)
}

func continuitySend(t *testing.T, r *Relay, profile model.Profile, conversation model.Conversation, text string) model.Task {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"id": profile.ID, "conversationId": conversation.ID, "text": text, "idempotencyKey": randomID()})
	v, err := r.profileSend(context.Background(), admin, body)
	if err != nil {
		t.Fatal(err)
	}
	return v.(model.Task)
}

func continuityBinding(t *testing.T, r *Relay, profile model.Profile, conversation model.Conversation) model.ProfileBinding {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, err := r.profileBinding(profile.ID, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestProfileConversationKindsAndIndependentBindings(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "complete", nil })
	workspace := profileStoreWorkspace(t, r)
	p := profileStoreCreate(t, r, profileStoreRequest(workspace))
	q := profileStoreCreate(t, r, profileStoreRequest(workspace))
	one := continuityConversation(t, r, "chat", p)
	two := continuityConversation(t, r, "task", p)
	channel := continuityConversation(t, r, "channel", p, q)
	if one.Owner != "admin" || one.Mode == "chat" || one.ProfileKind != "chat" || two.ProfileKind != "task" || channel.ProfileKind != "channel" || len(channel.ProfileIDs) != 2 {
		t.Fatalf("profile conversation changed collaboration semantics: %+v, %+v, %+v", one, two, channel)
	}
	first := continuityBinding(t, r, p, one)
	second := continuityBinding(t, r, p, two)
	if first.AgentID == second.AgentID || first.ID == second.ID || first.ConversationID != one.ID || second.ConversationID != two.ID {
		t.Fatalf("conversation bindings overlap: %+v %+v", first, second)
	}
	for _, conversation := range []model.Conversation{one, two} {
		if _, err := continuityCall(t, r, "profiles.bindingAction", map[string]any{"profileId": p.ID, "conversationId": conversation.ID, "action": "start"}); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := r.Agent(admin, first.AgentID)
	b, _ := r.Agent(admin, second.AgentID)
	if a.SessionID == "" || b.SessionID == "" || a.SessionID == b.SessionID || !a.ProfileManaged || a.ManagedChat {
		t.Fatalf("native sessions not isolated: %+v %+v", a, b)
	}
	_, err := r.Action(admin, first.AgentID, "restart")
	requireStatus(t, err, 403)
	_, err = r.Send(admin, model.SendRequest{From: "human:operator", To: first.AgentID, Text: "raw bypass", ConversationID: one.ID})
	requireStatus(t, err, 403)
	selected := continuitySend(t, r, p, channel, "Selected recipient only")
	if done := awaitTask(t, r, selected.ID); done.State != "completed" {
		t.Fatalf("selected recipient did not complete: %+v", done)
	}
	var taskCount, legacyCount int
	if err = r.db.QueryRow("SELECT count(*) FROM tasks WHERE json_extract(body,'$.conversationId')=?", channel.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err = r.db.QueryRow("SELECT count(*) FROM chat_conversations WHERE conversation_id IN (?,?,?)", one.ID, two.ID, channel.ID).Scan(&legacyCount); err != nil || legacyCount != 0 || taskCount != 1 {
		t.Fatalf("profile channel broadcast or legacy chat adoption: tasks=%d legacy=%d err=%v", taskCount, legacyCount, err)
	}
	for _, payload := range []map[string]any{
		{"title": "invalid", "kind": "chat", "profileIds": []string{p.ID, q.ID}},
		{"title": "invalid", "kind": "task", "profileIds": []string{}},
		{"title": "invalid", "kind": "channel", "profileIds": []string{p.ID}},
		{"title": "invalid", "kind": "channel", "profileIds": []string{p.ID, p.ID}},
		{"title": "invalid", "kind": "other", "profileIds": []string{p.ID}},
		{"title": "invalid", "kind": "chat", "profileIds": []string{p.ID}, "mode": "chat"},
	} {
		_, err = continuityCall(t, r, "profiles.createConversation", payload)
		requireStatus(t, err, 400)
	}
	var conversationCount int
	if err = r.db.QueryRow("SELECT count(*) FROM profile_conversation_settings").Scan(&conversationCount); err != nil || conversationCount != 3 {
		t.Fatalf("invalid creation left partial state: count=%d err=%v", conversationCount, err)
	}
	seen := map[string]bool{}
	after := ""
	for {
		v, err := continuityCall(t, r, "profiles.conversations", map[string]any{"limit": 1, "after": after})
		if err != nil {
			t.Fatal(err)
		}
		page := v.(model.AutomationPage[model.Conversation])
		if len(page.Items) != 1 || seen[page.Items[0].ID] {
			t.Fatalf("pagination skipped or repeated conversations: %+v", page)
		}
		seen[page.Items[0].ID] = true
		if !page.HasMore {
			break
		}
		after = page.NextAfter
	}
	if len(seen) != 3 {
		t.Fatalf("missing profile conversations: %v", seen)
	}
	_, err = r.profileContinuityOperation(context.Background(), model.Principal{AgentID: first.AgentID}, "profiles.conversations", json.RawMessage(`{}`))
	requireStatus(t, err, 403)
}

func TestProfileSwitchDisclosesExactHistoryAndPinnedVersions(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "answer", nil })
	p := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	one := continuityConversation(t, r, "chat", p)
	two := continuityConversation(t, r, "chat", p)
	first := awaitTask(t, r, continuitySend(t, r, p, one, "conversation-one-canary").ID)
	second := awaitTask(t, r, continuitySend(t, r, p, two, "conversation-two-canary").ID)
	old := continuityBinding(t, r, p, one)
	credential, err := r.Token(admin, old.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(request model.CollaborationRequest) model.SharedContext {
		t.Helper()
		v, err := r.Collaborate(admin, "context.publish", request)
		if err != nil {
			t.Fatal(err)
		}
		return v.(model.SharedContext)
	}
	pinned := publish(model.CollaborationRequest{Title: "Pinned v1", Content: "Pinned version one"})
	publish(model.CollaborationRequest{ContextID: pinned.ID, Version: 2, Content: "Unpinned version two"})
	unrelated := publish(model.CollaborationRequest{Title: "Unrelated", Content: "Never regrant unrelated context"})
	if _, err = r.Collaborate(admin, "context.attach", model.CollaborationRequest{ConversationID: one.ID, ContextID: pinned.ID, Version: 1}); err != nil {
		t.Fatal(err)
	}
	// A stale broad ACL must not be transferred into a new binding's exact grants.
	if _, err = r.db.Exec("INSERT INTO context_acl(context_id,agent_id) VALUES(?,?)", unrelated.ID, old.AgentID); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"id": p.ID, "expectedRevision": p.Revision, "binding": model.ProfileProvider{Harness: "omp"}}
	v, err := continuityCall(t, r, "profiles.switchPreview", request)
	if err != nil {
		t.Fatal(err)
	}
	preview := v.(profileSwitchDisclosure)
	if len(preview.Conversations) != 2 || preview.To.Harness != "omp" || preview.Instructions != p.Instructions {
		t.Fatalf("incomplete provider disclosure: %+v", preview)
	}
	for _, disclosure := range preview.Conversations {
		history := string(disclosure.Handoff)
		if disclosure.ConversationID == one.ID {
			if !strings.Contains(history, "conversation-one-canary") || strings.Contains(history, "conversation-two-canary") || len(disclosure.Contexts) != 1 || disclosure.Contexts[0].ContextID != pinned.ID || disclosure.Contexts[0].Version != 1 {
				t.Fatalf("incorrect first conversation disclosure: %+v", disclosure)
			}
		} else if disclosure.ConversationID == two.ID {
			if !strings.Contains(history, "conversation-two-canary") || strings.Contains(history, "conversation-one-canary") || len(disclosure.Contexts) != 0 {
				t.Fatalf("incorrect second conversation disclosure: %+v", disclosure)
			}
		} else {
			t.Fatalf("unrelated conversation disclosed: %+v", disclosure)
		}
	}
	_, err = continuityCall(t, r, "profiles.switch", request)
	requireStatus(t, err, 400)
	request["shareHistory"] = true
	v, err = continuityCall(t, r, "profiles.switch", request)
	if err != nil {
		t.Fatal(err)
	}
	updated := v.(model.Profile)
	fresh := continuityBinding(t, r, updated, one)
	if updated.ID != p.ID || updated.WorkspaceID != p.WorkspaceID || updated.Revision != p.Revision+1 || fresh.Generation != old.Generation+1 || fresh.AgentID == old.AgentID || fresh.Harness != "omp" {
		t.Fatalf("provider switch changed logical continuity: profile=%+v old=%+v new=%+v", updated, old, fresh)
	}
	_, err = r.Authenticate(credential)
	requireStatus(t, err, 401)
	_, err = r.action(admin, old.AgentID, "restart", true)
	requireStatus(t, err, 403)
	oldAgent, err := r.Agent(admin, old.AgentID)
	if err != nil || oldAgent.State != "stopped" {
		t.Fatalf("old runtime still active: %+v %v", oldAgent, err)
	}
	for _, before := range []model.Task{first, second} {
		after, err := r.Task(admin, before.ID)
		if err != nil || after.To != before.To || after.DestinationSessionID != before.DestinationSessionID || after.Result != before.Result {
			t.Fatalf("switch rewrote old task ownership: before=%+v after=%+v err=%v", before, after, err)
		}
	}
	conversation, err := r.Collaborate(admin, "conversation.get", model.CollaborationRequest{ID: one.ID})
	if err != nil {
		t.Fatal(err)
	}
	participants := conversation.(model.Conversation).Participants
	if !contains(participants, old.AgentID) || !contains(participants, fresh.AgentID) {
		t.Fatalf("historical participant was removed: %v", participants)
	}
	for _, check := range []struct {
		id              string
		version, status int
	}{{pinned.ID, 1, 0}, {pinned.ID, 2, 403}, {unrelated.ID, 1, 403}} {
		_, err = r.Collaborate(model.Principal{AgentID: fresh.AgentID}, "context.get", model.CollaborationRequest{ContextID: check.id, Version: check.version})
		if check.status == 0 {
			if err != nil {
				t.Fatalf("exact pinned version not regranted: %v", err)
			}
		} else {
			requireStatus(t, err, check.status)
		}
	}
	var broadGrants int
	if err = r.db.QueryRow("SELECT count(*) FROM context_acl WHERE agent_id=?", fresh.AgentID).Scan(&broadGrants); err != nil || broadGrants != 0 {
		t.Fatalf("switch granted broad context ACLs: %d %v", broadGrants, err)
	}
	followup := awaitTask(t, r, continuitySend(t, r, updated, one, "follow-up").ID)
	if followup.To != fresh.AgentID || followup.ConversationID != first.ConversationID || followup.DestinationSessionID == first.DestinationSessionID {
		t.Fatalf("follow-up did not use fresh exact binding: %+v", followup)
	}
	_, err = continuityCall(t, r, "profiles.switch", request)
	requireStatus(t, err, 409)
}

type continuityClosingRuntime struct {
	fakeRuntime
	closing chan struct{}
	release chan struct{}
}

func (r *continuityClosingRuntime) Close() error {
	select {
	case r.closing <- struct{}{}:
	default:
	}
	<-r.release
	return nil
}

func TestProfileSwitchSerializesAdmissionDuringTeardown(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return "answer", nil })
	closing, release := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	r.mu.Lock()
	adapter := r.adapters["codex"]
	adapter.Factory = func(context.Context, model.Agent, model.Emit) (model.Runtime, error) {
		return &continuityClosingRuntime{fakeRuntime: fakeRuntime{session: randomID(), run: func(context.Context, model.Turn, model.Emit) (string, error) { return "answer", nil }}, closing: closing, release: release}, nil
	}
	r.adapters["codex"] = adapter
	r.mu.Unlock()
	p := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	conversation := continuityConversation(t, r, "chat", p)
	awaitTask(t, r, continuitySend(t, r, p, conversation, "before-switch").ID)
	switchDone := make(chan error, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"id": p.ID, "expectedRevision": p.Revision, "binding": model.ProfileProvider{Harness: "omp"}, "shareHistory": true})
		_, err := r.profileContinuityOperation(context.Background(), admin, "profiles.switch", body)
		switchDone <- err
	}()
	select {
	case <-closing:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("switch did not tear down old runtime")
	}
	type sendResult struct {
		task model.Task
		err  error
	}
	sendDone := make(chan sendResult, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"id": p.ID, "conversationId": conversation.ID, "text": "after-switch", "idempotencyKey": randomID()})
		v, err := r.profileSend(context.Background(), admin, body)
		result := sendResult{err: err}
		if err == nil {
			result.task = v.(model.Task)
		}
		sendDone <- result
	}()
	select {
	case result := <-sendDone:
		close(release)
		t.Fatalf("admission raced old-runtime teardown: %+v", result)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-switchDone; err != nil {
		t.Fatal(err)
	}
	result := <-sendDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	fresh := continuityBinding(t, r, p, conversation)
	if result.task.To != fresh.AgentID || fresh.Harness != "omp" {
		t.Fatalf("racing admission used retired binding: task=%+v binding=%+v", result.task, fresh)
	}
	awaitTask(t, r, result.task.ID)
}

func TestProfileSwitchRejectsOutstandingWork(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	r, _ := continuityOpenTest(t, func(ctx context.Context, _ model.Turn, _ model.Emit) (string, error) {
		started <- struct{}{}
		select {
		case <-release:
			return "answer", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	p := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	conversation := continuityConversation(t, r, "task", p)
	task := continuitySend(t, r, p, conversation, "outstanding")
	<-started
	_, err := continuityCall(t, r, "profiles.switch", map[string]any{"id": p.ID, "expectedRevision": p.Revision, "binding": model.ProfileProvider{Harness: "omp"}, "shareHistory": true})
	requireStatus(t, err, 409)
	close(release)
	awaitTask(t, r, task.ID)
}

func TestProfileHandoffBoundsSequenceAndResetSurviveSwitch(t *testing.T) {
	r, _ := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) {
		return strings.Repeat("reply", 2000), nil
	})
	p := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	conversation := continuityConversation(t, r, "chat", p)
	var tasks []model.Task
	for _, label := range []string{"first-canary", "second-canary", "third-canary", "fourth-canary", "fifth-canary"} {
		tasks = append(tasks, awaitTask(t, r, continuitySend(t, r, p, conversation, label+strings.Repeat("界", 2000)).ID))
	}
	binding := continuityBinding(t, r, p, conversation)
	r.mu.Lock()
	var secondSeq, cutoff int64
	if err := r.db.QueryRow("SELECT seq FROM tasks WHERE id=?", tasks[1].ID).Scan(&secondSeq); err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	if err := r.db.QueryRow("SELECT seq FROM tasks WHERE id=?", tasks[2].ID).Scan(&cutoff); err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	handoff, err := r.profileHandoff(binding, secondSeq)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	if !strings.Contains(handoff, "first-canary") || strings.Contains(handoff, "second-canary") || strings.Contains(handoff, "fifth-canary") || len(handoff) > profileHandoffBytes || !json.Valid([]byte(handoff)) {
		r.mu.Unlock()
		t.Fatalf("handoff sequence or bound failed: %d bytes", len(handoff))
	}
	if _, err = r.db.Exec("UPDATE profile_context_epochs SET reset_after_seq=? WHERE binding_id=?", cutoff, binding.ID); err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	r.mu.Unlock()
	v, err := continuityCall(t, r, "profiles.switch", map[string]any{"id": p.ID, "expectedRevision": p.Revision, "binding": model.ProfileProvider{Harness: "omp"}, "shareHistory": true})
	if err != nil {
		t.Fatal(err)
	}
	fresh := continuityBinding(t, r, v.(model.Profile), conversation)
	r.mu.Lock()
	handoff, err = r.profileHandoff(fresh, 0)
	r.mu.Unlock()
	if err != nil || strings.Contains(handoff, "first-canary") || strings.Contains(handoff, "third-canary") || !strings.Contains(handoff, "fifth-canary") || len(handoff) > profileHandoffBytes || !json.Valid([]byte(handoff)) {
		t.Fatalf("switch lost reset cutoff or handoff bound: bytes=%d err=%v", len(handoff), err)
	}
}

func TestProfileGenerationIsMeteredDisposableAndNeverSaves(t *testing.T) {
	output := `{"name":"Draft coworker","instructions":"Explain findings and uncertainty."}`
	r, dataDir := continuityOpenTest(t, func(context.Context, model.Turn, model.Emit) (string, error) { return output, nil })
	workspace := profileStoreWorkspace(t, r)
	if err := os.WriteFile(filepath.Join(workspace.Path, "operator-private"), []byte("not a generation input"), 0600); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"description": "An assistant for research", "harness": "omp", "workspaceId": workspace.ID}
	_, err := continuityCall(t, r, "profiles.generate", request)
	requireStatus(t, err, 503)
	var draftWorkspace string
	r.mu.Lock()
	adapter := r.adapters["omp"]
	adapter.Capabilities["toolFreeDraft"] = true
	adapter.Factory = func(_ context.Context, agent model.Agent, _ model.Emit) (model.Runtime, error) {
		if !agent.GenerationDraft || !agent.Ephemeral || agent.SessionID != "" || agent.Cwd == workspace.Path || len(agent.Policy.Destinations) != 0 || agent.Policy.Delegate || agent.Policy.WorktreeWrites {
			t.Errorf("generation inherited execution authority: %+v", agent)
		}
		entries, err := os.ReadDir(agent.Cwd)
		if err != nil || len(entries) != 0 {
			t.Errorf("generation workspace was not empty: %v %v", entries, err)
		}
		draftWorkspace = agent.Cwd
		return &fakeRuntime{session: randomID(), run: func(context.Context, model.Turn, model.Emit) (string, error) { return output, nil }}, nil
	}
	r.adapters["omp"] = adapter
	r.mu.Unlock()
	v, err := continuityCall(t, r, "profiles.generate", request)
	if err != nil {
		t.Fatal(err)
	}
	taskID := v.(map[string]any)["taskId"].(string)
	task := awaitTask(t, r, taskID)
	v, err = continuityCall(t, r, "profiles.generationDraft", map[string]any{"taskId": taskID})
	if err != nil {
		t.Fatal(err)
	}
	result := v.(profileGenerationResult)
	if result.Draft == nil || result.Draft.Name != "Draft coworker" || result.Error != "" || task.State != "completed" {
		t.Fatalf("draft not surfaced for review: %+v task=%+v", result, task)
	}
	var profiles, usage int
	if err = r.db.QueryRow("SELECT count(*) FROM agent_profiles").Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if err = r.db.QueryRow("SELECT count(*) FROM run_usage WHERE task_id=?", taskID).Scan(&usage); err != nil || profiles != 0 || usage != 1 {
		t.Fatalf("generation saved profile or skipped normal metering: profiles=%d usage=%d err=%v", profiles, usage, err)
	}
	_, err = r.Action(admin, task.To, "start")
	requireStatus(t, err, 403)
	_, err = r.Send(admin, model.SendRequest{From: "human:operator", To: task.To, Text: "reused draft identity"})
	requireStatus(t, err, 403)
	if filepath.Dir(draftWorkspace) != dataDir {
		t.Fatalf("disposable workspace outside owned data directory: %s", draftWorkspace)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.cleanupProfileGenerations()
		if _, err = os.Stat(draftWorkspace); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed draft workspace retained: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = r.Task(admin, taskID); err != nil {
		t.Fatalf("disposal erased task provenance: %v", err)
	}
	output = `{"name":"Unsafe draft","instructions":"Do work","capabilities":{"command":true}}`
	v, err = continuityCall(t, r, "profiles.generate", request)
	if err != nil {
		t.Fatal(err)
	}
	taskID = v.(map[string]any)["taskId"].(string)
	awaitTask(t, r, taskID)
	v, err = continuityCall(t, r, "profiles.generationDraft", map[string]any{"taskId": taskID})
	if err != nil {
		t.Fatal(err)
	}
	result = v.(profileGenerationResult)
	if result.Draft != nil || result.Error == "" || result.Raw == "" || result.State != "completed" {
		t.Fatalf("invalid structured output was repaired or hidden: %+v", result)
	}
}

func TestProfileDraftParsingRejectsAuthorityAndUnboundedOutput(t *testing.T) {
	for _, input := range []string{
		`null`, `[]`, `{}`, `{"name":"n","instructions":"i","permissions":{}}`,
		`{"name":"n","instructions":"i"} {"name":"n","instructions":"i"}`,
		`{"name":"n","instructions":"i"} trailing`,
		`{"name":"` + strings.Repeat("界", 43) + `","instructions":"i"}`,
		`{"name":"n","instructions":"` + strings.Repeat("a", 16385) + `"}`,
		`{"name":"n","instructions":"bad\u0000control"}`,
		`{"name":"-----BEGIN PRIVATE KEY-----","instructions":"i"}`,
		"{\"name\":\"n\",\"instructions\":\"" + string([]byte{0xff}) + "\"}",
		strings.Repeat("x", 24577),
	} {
		if _, err := parseProfileDraft(input); err == nil {
			t.Fatalf("accepted invalid generated authority/output: %.120s", input)
		}
	}
}

func TestProfileBindingControlsRecordEventsAndAllowArchivedStop(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	p := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	conversation := continuityConversation(t, r, "chat", p)
	request := map[string]any{"profileId": p.ID, "conversationId": conversation.ID, "action": "start"}
	if _, err := continuityCall(t, r, "profiles.bindingAction", request); err != nil {
		t.Fatal(err)
	}
	if _, err := profileStoreCall(t, r, "profiles.archive", map[string]any{"id": p.ID, "expectedRevision": p.Revision, "archived": true}); err != nil {
		t.Fatal(err)
	}
	request["action"] = "restart"
	_, err := continuityCall(t, r, "profiles.bindingAction", request)
	requireStatus(t, err, 409)
	request["action"] = "stop"
	v, err := continuityCall(t, r, "profiles.bindingAction", request)
	if err != nil || v.(model.Agent).State != "stopped" {
		t.Fatalf("archiving prevented reviewed stop: %v %v", v, err)
	}
	var events int
	if err = r.db.QueryRow("SELECT count(*) FROM automation_events WHERE kind='profiles.bindingAction' AND profile_id=?", p.ID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("reviewed lifecycle controls missing atomic events: count=%d err=%v", events, err)
	}
	_, err = continuityCall(t, r, "profiles.createConversation", map[string]any{"title": "Archived", "kind": "chat", "profileIds": []string{p.ID}})
	requireStatus(t, err, 409)
}
