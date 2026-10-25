package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"meshra/internal/model"
)

func TestUnmarkedAudienceRecoveryPreservesDataWithoutResumingPrivateContext(t *testing.T) {
	prompts := make(chan string, 3)
	r, dir := continuityOpenTest(t, func(_ context.Context, turn model.Turn, _ model.Emit) (string, error) {
		prompts <- turn.Task.Text
		return "done", nil
	})
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	privateMemory := "OPERATOR-ONLY-RECOVERY-CANARY"
	privateMessage := "HISTORICAL-PRIVATE-CONTEXT-CANARY"
	raw, _ := json.Marshal(map[string]any{"profileId": profile.ID, "scope": "operator", "content": privateMemory, "pinned": true})
	if _, err := r.profileMemoryOperation(context.Background(), admin, "memory.save", raw); err != nil {
		t.Fatal(err)
	}
	conversation := continuityConversation(t, r, "chat", profile)
	first := continuitySend(t, r, profile, conversation, privateMessage)
	original := awaitTask(t, r, first.ID)
	if original.State != "completed" {
		t.Fatal(original.Error)
	}
	if prompt := <-prompts; !strings.Contains(prompt, privateMemory) {
		t.Fatal("explicit new private conversation did not receive operator memory")
	}
	binding := continuityBinding(t, r, profile, conversation)
	r.mu.Lock()
	_, err := r.db.Exec("UPDATE profile_bindings SET body=json_remove(body,'$.externalConversation') WHERE id=?", binding.ID)
	adapters := r.adapters
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	retained, err := reopened.Task(admin, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.DestinationSessionID != original.DestinationSessionID || retained.To != original.To {
		t.Fatal("audience recovery rewrote historical native ownership")
	}
	followup := continuitySend(t, reopened, profile, conversation, "Continue with a restricted unknown audience")
	if completed := awaitTask(t, reopened, followup.ID); completed.State != "completed" {
		t.Fatal(completed.Error)
	}
	if prompt := <-prompts; strings.Contains(prompt, privateMemory) || strings.Contains(prompt, privateMessage) {
		t.Fatal("unverified legacy audience resumed private memory or prior context")
	}
	listed, err := reopened.profileMemoryOperation(context.Background(), admin, "memory.list", json.RawMessage(`{"profileId":"`+profile.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, memory := range listed.(profileMemoryPage).Items {
		if memory.Content == privateMemory {
			found = true
		}
	}
	if !found {
		t.Fatal("audience recovery erased the operator's saved memory")
	}
	fresh := continuityConversation(t, reopened, "chat", profile)
	freshTask := continuitySend(t, reopened, profile, fresh, "Use an explicitly private conversation")
	if completed := awaitTask(t, reopened, freshTask.ID); completed.State != "completed" {
		t.Fatal(completed.Error)
	}
	if prompt := <-prompts; !strings.Contains(prompt, privateMemory) {
		t.Fatal("restricted legacy audience contaminated a new private conversation")
	}
}
