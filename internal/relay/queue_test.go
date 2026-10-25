package relay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"meshra/internal/model"
)

func queueCall(t *testing.T, r *Relay, op string, q any) queueView {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	value, err := r.queueOperation(admin, op, raw)
	if err != nil {
		t.Fatal(err)
	}
	return value.(queueView)
}
func queueSnapshot(t *testing.T, r *Relay, id string) queueView {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.queueState(id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestQueuePauseExpiresWithoutOfflineDelivery(t *testing.T) {
	r, _ := openTest(t, nil)
	source := createTest(t, r, "omp", "source")
	dest := createTest(t, r, "codex", "paused")
	queueCall(t, r, "queue.pause", map[string]any{"agentId": dest.ID, "expectedRevision": 0})
	task := sendTest(t, r, source.ID, dest.ID, "paused task")
	r.tick()
	current, err := r.Task(admin, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "queued" || current.DeliveryAttempts != 0 {
		t.Fatalf("paused queue consumed a delivery check: %+v", current)
	}
	r.mu.Lock()
	current.Deadline = now().Add(-time.Second)
	err = r.transition(current, "deadline_adjusted", "")
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r.tick()
	current, err = r.Task(admin, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "timed_out" || current.DeliveryAttempts != 0 {
		t.Fatalf("paused deadline not enforced: %+v", current)
	}
	state := queueSnapshot(t, r, dest.ID)
	_, err = r.queueOperation(admin, "queue.resume", json.RawMessage(`{"agentId":"`+dest.ID+`","expectedRevision":0}`))
	requireStatus(t, err, 409)
	resumed := queueCall(t, r, "queue.resume", map[string]any{"agentId": dest.ID, "expectedRevision": state.Revision})
	if resumed.Paused {
		t.Fatal("resume retained pause")
	}
}

func TestQueueReorderPreservesRecipientOrderAndTranscript(t *testing.T) {
	observed := make(chan string, 3)
	r, _ := openTest(t, func(_ context.Context, turn model.Turn, _ model.Emit) (string, error) {
		observed <- turn.Task.Text
		return "complete", nil
	})
	r.mu.Lock()
	r.settings.MaxParallelTurns = 1
	r.mu.Unlock()
	source := createTest(t, r, "omp", "source")
	a := createTest(t, r, "codex", "first")
	b := createTest(t, r, "claude", "second")
	queueCall(t, r, "queue.pause", map[string]any{"agentId": a.ID, "expectedRevision": 0})
	queueCall(t, r, "queue.pause", map[string]any{"agentId": b.ID, "expectedRevision": 0})
	first := sendTest(t, r, source.ID, a.ID, "A1")
	middle := sendTest(t, r, source.ID, b.ID, "B1")
	last := sendTest(t, r, source.ID, a.ID, "A2")
	state := queueSnapshot(t, r, a.ID)
	bad, _ := json.Marshal(map[string]any{"agentId": a.ID, "expectedRevision": state.Revision, "taskIds": []string{last.ID}})
	_, err := r.queueOperation(admin, "queue.reorder", bad)
	requireStatus(t, err, 409)
	state = queueCall(t, r, "queue.reorder", map[string]any{"agentId": a.ID, "expectedRevision": state.Revision, "taskIds": []string{last.ID, first.ID}})
	startTest(t, r, a.ID)
	startTest(t, r, b.ID)
	// Resume both while holding relay ownership so the scheduler sees one complete
	// eligibility change, not two unrelated operator actions at different times.
	r.mu.Lock()
	_, err = r.db.Exec("UPDATE agent_queue_state SET paused=0,revision=revision+1 WHERE agent_id IN (?,?)", a.ID, b.ID)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r.signal()
	seen := map[string]int{}
	for i := range 3 {
		select {
		case got := <-observed:
			if _, duplicate := seen[got]; duplicate {
				t.Fatal("queue dispatched a task twice", got)
			}
			seen[got] = i
		case <-time.After(5 * time.Second):
			t.Fatal("queue did not dispatch")
		}
	}
	aFirst, firstOK := seen["A2"]
	aLast, lastOK := seen["A1"]
	if !firstOK || !lastOK || aFirst >= aLast {
		t.Fatal("recipient dispatch rank was ignored", seen)
	}
	if _, ok := seen["B1"]; !ok {
		t.Fatal("reorder lost another recipient's queued task", seen)
	}
	for _, task := range []model.Task{first, middle, last} {
		if done := awaitTask(t, r, task.ID); done.State != "completed" {
			t.Fatal(done)
		}
	}
	var x, y, z int64
	if err = r.db.QueryRow("SELECT seq FROM tasks WHERE id=?", first.ID).Scan(&x); err != nil {
		t.Fatal(err)
	}
	if err = r.db.QueryRow("SELECT seq FROM tasks WHERE id=?", middle.ID).Scan(&y); err != nil {
		t.Fatal(err)
	}
	if err = r.db.QueryRow("SELECT seq FROM tasks WHERE id=?", last.ID).Scan(&z); err != nil {
		t.Fatal(err)
	}
	if !(x < y && y < z) {
		t.Fatal("dispatch reorder rewrote transcript order")
	}
	if state.Tasks[0].ID != last.ID {
		t.Fatal("queue inspection did not expose dispatch rank")
	}
}
