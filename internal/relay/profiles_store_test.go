package relay

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

func profileStoreCall(t *testing.T, r *Relay, op string, q any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return r.profileStoreOperation(admin, op, raw)
}

func profileStoreWorkspace(t *testing.T, r *Relay) model.Workspace {
	t.Helper()
	v, err := r.workspaceOperation("workspaces/add", model.ConsoleRequest{Path: t.TempDir(), Name: "Profile workspace"})
	if err != nil {
		t.Fatal(err)
	}
	return v.(model.Workspace)
}

func profileStoreRequest(w model.Workspace) map[string]any {
	return map[string]any{
		"name": "Portable coworker", "instructions": "Keep findings scoped.\nExplain uncertainty.",
		"workspaceId": w.ID, "binding": model.ProfileProvider{Harness: "codex"},
		"capabilities": model.ProfileCapabilities{}, "allowedProfileIds": []string{}, "notificationsEnabled": false,
	}
}

func profileStoreCreate(t *testing.T, r *Relay, q map[string]any) model.Profile {
	t.Helper()
	v, err := profileStoreCall(t, r, "profiles.save", q)
	if err != nil {
		t.Fatal(err)
	}
	return v.(model.Profile)
}

func TestProfileStoreCreationPreservesCanonicalAgents(t *testing.T) {
	r, _ := openTest(t, nil)
	canonical := createTest(t, r, "codex", "portable")
	before, err := r.Agents(admin)
	if err != nil {
		t.Fatal(err)
	}
	q := profileStoreRequest(profileStoreWorkspace(t, r))
	q["name"] = canonical.DisplayName
	q["capabilities"] = model.ProfileCapabilities{WorkspaceRead: true, MemoryWrite: true, Attachments: true}
	v := profileStoreCreate(t, r, q)
	if v.ID == canonical.ID || v.Revision != 1 || v.Archived || profileExecution(v.Capabilities) || !v.Capabilities.WorkspaceRead || !v.Capabilities.MemoryWrite || !v.Capabilities.Attachments {
		t.Fatalf("creation changed profile identity or requested opt-ins: %+v", v)
	}
	after, err := r.Agents(admin)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("profile creation changed registered agents: %v, %+v", err, after)
	}
	var bindings int
	if err = r.db.QueryRow("SELECT count(*) FROM profile_bindings WHERE profile_id=?", v.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("profile creation materialized a provider binding: count=%d err=%v", bindings, err)
	}
	got, err := profileStoreCall(t, r, "profiles.get", map[string]any{"id": v.ID})
	if err != nil || !reflect.DeepEqual(v, got) {
		t.Fatalf("saved profile differs: %+v, %v", got, err)
	}
}

func TestProfileStoreStrictValidation(t *testing.T) {
	r, _ := openTest(t, nil)
	w := profileStoreWorkspace(t, r)
	cases := []struct {
		name   string
		edit   func(map[string]any)
		status int
	}{
		{"unknown field", func(q map[string]any) { q["archived"] = true }, 400},
		{"unknown binding field", func(q map[string]any) {
			q["binding"] = map[string]any{"harness": "codex", "config": map[string]any{}, "secret": "value"}
		}, 400},
		{"unknown capability", func(q map[string]any) { q["capabilities"] = map[string]any{"shell": true} }, 400},
		{"blank name", func(q map[string]any) { q["name"] = " " }, 400},
		{"name byte bound", func(q map[string]any) { q["name"] = strings.Repeat("界", 43) }, 400},
		{"name credential", func(q map[string]any) { q["name"] = "sk-abcdefghijklmnopqrst" }, 400},
		{"instruction size", func(q map[string]any) { q["instructions"] = strings.Repeat("a", 16*1024+1) }, 413},
		{"instruction credential", func(q map[string]any) { q["instructions"] = "api_key=super-secret-value" }, 400},
		{"instruction private key", func(q map[string]any) { q["instructions"] = "-----BEGIN PRIVATE KEY-----" }, 400},
		{"instruction control", func(q map[string]any) { q["instructions"] = "read\x00only" }, 400},
		{"unknown workspace", func(q map[string]any) { q["workspaceId"] = "missing" }, 404},
		{"unknown allowed profile", func(q map[string]any) { q["allowedProfileIds"] = []string{"missing"} }, 404},
		{"unknown avatar", func(q map[string]any) { q["avatarAttachmentId"] = strings.Repeat("0", 32) }, 404},
		{"future harness", func(q map[string]any) {
			q["binding"] = model.ProfileProvider{Harness: "openai", Config: model.Config{Model: "local"}}
		}, 400},
		{"incomplete opencode config", func(q map[string]any) {
			q["binding"] = model.ProfileProvider{Harness: "opencode", Config: model.Config{Model: "model"}}
		}, 400},
		{"wrong claude provider", func(q map[string]any) {
			q["binding"] = model.ProfileProvider{Harness: "claude", Config: model.Config{Provider: "other"}}
		}, 400},
		{"config credential", func(q map[string]any) {
			q["binding"] = model.ProfileProvider{Harness: "omp", Config: model.Config{Model: "sk-abcdefghijklmnopqrst"}}
		}, 400},
		{"native connection", func(q map[string]any) {
			q["binding"] = model.ProfileProvider{Harness: "codex", ConnectionID: "endpoint"}
		}, 400},
		{"revision on creation", func(q map[string]any) { q["expectedRevision"] = 0 }, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := profileStoreRequest(w)
			tc.edit(q)
			_, err := profileStoreCall(t, r, "profiles.save", q)
			requireStatus(t, err, tc.status)
		})
	}
	for name, capability := range map[string]model.ProfileCapabilities{
		"write": {WorkspaceWrite: true}, "command": {Command: true}, "browser": {Browser: true}, "computer": {Computer: true}, "external MCP": {ExternalMCP: true},
	} {
		t.Run(name, func(t *testing.T) {
			q := profileStoreRequest(w)
			q["capabilities"] = capability
			_, err := profileStoreCall(t, r, "profiles.save", q)
			requireStatus(t, err, 400)
		})
	}
	for _, raw := range []string{"[]", "null", "{} {}", "{\"name\":\"\xff\"}"} {
		_, err := r.profileStoreOperation(admin, "profiles.save", json.RawMessage(raw))
		requireStatus(t, err, 400)
	}
	_, err := r.profileStoreOperation(model.Principal{AgentID: "codex:portable"}, "profiles.list", json.RawMessage(`{}`))
	requireStatus(t, err, 403)
	page, err := profileStoreCall(t, r, "profiles.list", map[string]any{})
	if err != nil || len(page.(model.AutomationPage[model.Profile]).Items) != 0 {
		t.Fatalf("invalid creation persisted data: %v, %+v", err, page)
	}
}

func TestProfileStoreRevisionArchiveAndUnavailableExecution(t *testing.T) {
	r, _ := openTest(t, nil)
	q := profileStoreRequest(profileStoreWorkspace(t, r))
	v := profileStoreCreate(t, r, q)
	q["id"], q["expectedRevision"], q["instructions"] = v.ID, v.Revision, "Updated instructions"
	updated := profileStoreCreate(t, r, q)
	if updated.Revision != 2 || updated.Instructions != "Updated instructions" {
		t.Fatalf("edit not persisted: %+v", updated)
	}
	_, err := profileStoreCall(t, r, "profiles.save", q)
	requireStatus(t, err, 409)
	q["expectedRevision"] = updated.Revision
	q["binding"] = model.ProfileProvider{Harness: "omp"}
	_, err = profileStoreCall(t, r, "profiles.save", q)
	requireStatus(t, err, 409)
	q["binding"] = updated.Binding
	q["capabilities"] = model.ProfileCapabilities{Command: true}
	_, err = profileStoreCall(t, r, "profiles.save", q)
	requireStatus(t, err, 503)
	got, err := profileStoreCall(t, r, "profiles.get", map[string]any{"id": v.ID})
	if err != nil || !reflect.DeepEqual(got, updated) {
		t.Fatalf("failed mutations changed profile: %+v, %v", got, err)
	}
	archived, err := profileStoreCall(t, r, "profiles.archive", map[string]any{"id": v.ID, "expectedRevision": 2, "archived": true})
	if err != nil {
		t.Fatal(err)
	}
	a := archived.(model.Profile)
	if !a.Archived || a.Revision != 3 || a.Instructions != updated.Instructions || a.WorkspaceID != updated.WorkspaceID {
		t.Fatalf("archive lost profile data: %+v", a)
	}
	_, err = profileStoreCall(t, r, "profiles.archive", map[string]any{"id": v.ID, "expectedRevision": 2, "archived": false})
	requireStatus(t, err, 409)
	q["expectedRevision"], q["capabilities"] = 3, updated.Capabilities
	if saved := profileStoreCreate(t, r, q); !saved.Archived {
		t.Fatal("ordinary save unarchived profile")
	}
}

func TestProfileStorePaginationAndAllowedProfiles(t *testing.T) {
	r, _ := openTest(t, nil)
	w := profileStoreWorkspace(t, r)
	var profiles []model.Profile
	for range 51 {
		profiles = append(profiles, profileStoreCreate(t, r, profileStoreRequest(w)))
	}
	value, err := profileStoreCall(t, r, "profiles.list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	first := value.(model.AutomationPage[model.Profile])
	if len(first.Items) != 50 || !first.HasMore || first.NextAfter != first.Items[49].ID {
		t.Fatalf("default pagination: %+v", first)
	}
	value, err = profileStoreCall(t, r, "profiles.list", map[string]any{"after": first.NextAfter, "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	last := value.(model.AutomationPage[model.Profile])
	if len(last.Items) != 1 || last.HasMore || last.NextAfter != "" || last.Items[0].ID <= first.NextAfter {
		t.Fatalf("cursor pagination: %+v", last)
	}
	for _, payload := range []map[string]any{{"limit": -1}, {"limit": 101}, {"after": "bad\n"}, {"unknown": true}} {
		_, err = profileStoreCall(t, r, "profiles.list", payload)
		requireStatus(t, err, 400)
	}
	q := profileStoreRequest(w)
	q["allowedProfileIds"] = []string{profiles[0].ID}
	withGrant := profileStoreCreate(t, r, q)
	if !reflect.DeepEqual(withGrant.AllowedProfileIDs, []string{profiles[0].ID}) {
		t.Fatal("explicit profile grant was not preserved")
	}
	q["allowedProfileIds"] = []string{profiles[0].ID, profiles[0].ID}
	_, err = profileStoreCall(t, r, "profiles.save", q)
	requireStatus(t, err, 400)
	q["id"], q["expectedRevision"], q["allowedProfileIds"] = withGrant.ID, withGrant.Revision, []string{withGrant.ID}
	_, err = profileStoreCall(t, r, "profiles.save", q)
	requireStatus(t, err, 400)
}

func TestProfileStoreEventFailureRollsBackMutation(t *testing.T) {
	r, _ := openTest(t, nil)
	q := profileStoreRequest(profileStoreWorkspace(t, r))
	before := profileStoreCreate(t, r, q)
	if _, err := r.db.Exec(`CREATE TEMP TRIGGER reject_profile_event BEFORE INSERT ON automation_events BEGIN SELECT RAISE(ABORT,'event storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	q["id"], q["expectedRevision"], q["instructions"] = before.ID, before.Revision, "Must not persist"
	if _, err := profileStoreCall(t, r, "profiles.save", q); err == nil {
		t.Fatal("save reported success without its durable event")
	}
	value, err := profileStoreCall(t, r, "profiles.get", map[string]any{"id": before.ID})
	if err != nil || !reflect.DeepEqual(value, before) {
		t.Fatalf("event failure left partially saved profile: %+v %v", value, err)
	}
	if _, err = profileStoreCall(t, r, "profiles.archive", map[string]any{"id": before.ID, "expectedRevision": before.Revision, "archived": true}); err == nil {
		t.Fatal("archive reported success without its durable event")
	}
	value, err = profileStoreCall(t, r, "profiles.get", map[string]any{"id": before.ID})
	if err != nil || !reflect.DeepEqual(value, before) {
		t.Fatalf("event failure left partially archived profile: %+v %v", value, err)
	}
}

func TestProfileStoreAdmittedSnapshotAndArchivePreserveHistory(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	r, _ := continuityOpenTest(t, func(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
		close(started)
		select {
		case <-release:
			return "Preserved profile reply", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	q := profileStoreRequest(profileStoreWorkspace(t, r))
	profile := profileStoreCreate(t, r, q)
	raw, err := json.Marshal(map[string]any{"id": profile.ID, "text": "Preserve this exchange", "idempotencyKey": "profile-snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := r.Automation(context.Background(), admin, "profiles.send", raw)
	if err != nil {
		t.Fatal(err)
	}
	task := value.(model.Task)
	select {
	case <-started:
	case <-r.ctx.Done():
		t.Fatal("relay stopped before profile execution")
	case <-time.After(5 * time.Second):
		t.Fatal("profile turn did not start")
	}
	q["id"], q["expectedRevision"], q["instructions"] = profile.ID, profile.Revision, "Edited after admission"
	edited := profileStoreCreate(t, r, q)
	r.mu.Lock()
	snapshot, err := r.profileSnapshot(task)
	r.mu.Unlock()
	if err != nil || snapshot.Profile.Instructions != profile.Instructions || snapshot.Profile.Revision != profile.Revision {
		t.Fatalf("admitted snapshot changed after edit: %+v %v", snapshot, err)
	}
	q["expectedRevision"], q["capabilities"] = edited.Revision, model.ProfileCapabilities{WorkspaceRead: true}
	_, err = profileStoreCall(t, r, "profiles.save", q)
	requireStatus(t, err, 409)
	q["capabilities"], q["workspaceId"] = profile.Capabilities, profileStoreWorkspace(t, r).ID
	_, err = profileStoreCall(t, r, "profiles.save", q)
	requireStatus(t, err, 409)
	if _, err = profileStoreCall(t, r, "profiles.archive", map[string]any{"id": profile.ID, "expectedRevision": edited.Revision, "archived": true}); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(map[string]any{"id": profile.ID, "conversationId": task.ConversationID, "text": "Forbidden future admission", "idempotencyKey": "after-archive"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Automation(context.Background(), admin, "profiles.send", raw)
	requireStatus(t, err, 409)
	close(release)
	finished := awaitTask(t, r, task.ID)
	if finished.State != "completed" || finished.Result != "Preserved profile reply" || finished.To != task.To || finished.ConversationID != task.ConversationID {
		t.Fatalf("archive changed admitted work or its history: %+v", finished)
	}
}

func TestProfileStoreWorkspaceChangeRotatesOnlyManagedSession(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	canonical := createTest(t, r, "codex", "unrelated")
	canonical = startTest(t, r, canonical.ID)
	q := profileStoreRequest(profileStoreWorkspace(t, r))
	profile := profileStoreCreate(t, r, q)
	send := func(conversationID, key string) model.Task {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"id": profile.ID, "conversationId": conversationID, "text": "Workspace continuity", "idempotencyKey": key})
		if err != nil {
			t.Fatal(err)
		}
		value, err := r.Automation(context.Background(), admin, "profiles.send", raw)
		if err != nil {
			t.Fatal(err)
		}
		return awaitTask(t, r, value.(model.Task).ID)
	}
	first := send("", "before-workspace")
	if first.State != "completed" || first.DestinationSessionID == "" {
		t.Fatalf("initial profile turn failed: %+v", first)
	}
	newWorkspace := profileStoreWorkspace(t, r)
	q["id"], q["expectedRevision"], q["workspaceId"] = profile.ID, profile.Revision, newWorkspace.ID
	updated := profileStoreCreate(t, r, q)
	managed, err := r.Agent(admin, first.To)
	if err != nil || managed.Cwd != newWorkspace.Path || managed.SessionID != "" || managed.State != "stopped" {
		t.Fatalf("saved workspace did not rotate managed runtime: %+v %v", managed, err)
	}
	unchanged, err := r.Agent(admin, canonical.ID)
	if err != nil || unchanged.ID != canonical.ID || unchanged.Cwd != canonical.Cwd || unchanged.SessionID != canonical.SessionID || unchanged.Config != canonical.Config || !reflect.DeepEqual(unchanged.Policy, canonical.Policy) {
		t.Fatalf("workspace change mutated an unrelated canonical agent: %+v %v", unchanged, err)
	}
	second := send(first.ConversationID, "after-workspace")
	if second.State != "completed" || second.To != first.To || second.ConversationID != first.ConversationID || second.DestinationSessionID == first.DestinationSessionID {
		t.Fatalf("workspace change lost identity or reused old native context: first=%+v second=%+v", first, second)
	}
	r.mu.Lock()
	oldSnapshot, oldErr := r.profileSnapshot(first)
	newSnapshot, newErr := r.profileSnapshot(second)
	r.mu.Unlock()
	if oldErr != nil || newErr != nil || oldSnapshot.Profile.WorkspaceID != profile.WorkspaceID || newSnapshot.Profile.WorkspaceID != updated.WorkspaceID {
		t.Fatalf("workspace change rewrote admitted snapshots: old=%+v new=%+v errors=%v,%v", oldSnapshot, newSnapshot, oldErr, newErr)
	}
}
