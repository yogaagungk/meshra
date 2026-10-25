package relay

import (
	"context"
	"testing"
	"time"

	"meshra/internal/model"
)

// Narrowing a profile policy is always allowed; widening still requires a
// quiescent profile.
func TestProfilePolicyNarrowingIsAllowedWhileWideningIsBusy(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	r, _ := continuityOpenTest(t, func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return "answer", nil
	})
	t.Cleanup(func() { close(release) })
	workspace := profileStoreWorkspace(t, r)
	q := profileStoreRequest(workspace)
	q["capabilities"] = model.ProfileCapabilities{WorkspaceRead: true, MemoryWrite: true}
	profile := profileStoreCreate(t, r, q)
	conversation := continuityConversation(t, r, "chat", profile)
	task := continuitySend(t, r, profile, conversation, "long running request")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("profile turn did not start")
	}

	widening := profileStoreRequest(workspace)
	widening["id"] = profile.ID
	widening["expectedRevision"] = profile.Revision
	widening["capabilities"] = model.ProfileCapabilities{WorkspaceRead: true, MemoryWrite: true, Attachments: true}
	_, err := profileStoreCall(t, r, "profiles.save", widening)
	requireStatus(t, err, 409)

	narrowing := profileStoreRequest(workspace)
	narrowing["id"] = profile.ID
	narrowing["expectedRevision"] = profile.Revision
	narrowing["capabilities"] = model.ProfileCapabilities{WorkspaceRead: true}
	value, err := profileStoreCall(t, r, "profiles.save", narrowing)
	if err != nil {
		t.Fatalf("narrowing a policy was refused: %v", err)
	}
	saved := value.(model.Profile)
	if saved.Capabilities.MemoryWrite || saved.Capabilities.Attachments || !saved.Capabilities.WorkspaceRead {
		t.Fatalf("narrowing did not take effect: %+v", saved.Capabilities)
	}
	if saved.Revision != profile.Revision+1 {
		t.Fatalf("narrowing did not advance the profile revision: %d", saved.Revision)
	}
	if state := mustTask(t, r, task.ID).State; model.Terminal(state) {
		t.Fatalf("narrowing silently changed the admitted task: %s", state)
	}
}

func mustTask(t *testing.T, r *Relay, id string) model.Task {
	t.Helper()
	v, err := r.Task(admin, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The persisted tool policy revision must follow a narrowing so that an
// admitted catalog is no longer valid.
func TestProfilePolicyNarrowingAdvancesToolPolicyRevision(t *testing.T) {
	r, _ := openTest(t, nil)
	workspace := profileStoreWorkspace(t, r)
	q := profileStoreRequest(workspace)
	q["capabilities"] = model.ProfileCapabilities{WorkspaceRead: true, MemoryWrite: true}
	profile := profileStoreCreate(t, r, q)
	var before int
	if err := r.db.QueryRow("SELECT revision FROM profile_tool_policies WHERE profile_id=?", profile.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	narrowing := profileStoreRequest(workspace)
	narrowing["id"] = profile.ID
	narrowing["expectedRevision"] = profile.Revision
	narrowing["capabilities"] = model.ProfileCapabilities{}
	if _, err := profileStoreCall(t, r, "profiles.save", narrowing); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := r.db.QueryRow("SELECT revision FROM profile_tool_policies WHERE profile_id=?", profile.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("tool policy revision did not advance: %d -> %d", before, after)
	}
}
