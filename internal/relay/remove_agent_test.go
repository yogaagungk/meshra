package relay

import (
	"testing"

	"meshra/internal/model"
)

// An agent that has received work owns queue-state rows; removing it must clear
// them instead of failing on the foreign key.
func TestRemoveAgentAfterItReceivedWork(t *testing.T) {
	r, _ := openTest(t, nil)
	from := createTest(t, r, "omp", "sender")
	dest := createTest(t, r, "omp", "dest")
	startTest(t, r, dest.ID)
	task, err := r.Send(admin, model.SendRequest{From: from.ID, To: dest.ID, Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	awaitTask(t, r, task.ID)
	if _, err = r.Action(admin, dest.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if err = r.Remove(admin, dest.ID); err != nil {
		t.Fatalf("remove failed after the agent received work: %v", err)
	}
	if _, err = r.Agent(admin, dest.ID); err == nil {
		t.Fatal("the agent still exists")
	}
}
