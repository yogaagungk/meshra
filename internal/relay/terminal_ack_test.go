package relay

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"meshra/internal/model"
)

// A storage failure while recording the terminal state must neither claim durable
// success, nor re-execute the turn, nor leave the identity healthy and idle.
func TestTerminalPersistenceFailureStopsDispatchWithoutReexecution(t *testing.T) {
	var runs atomic.Int32
	r, _ := openTest(t, func(context.Context, model.Turn, model.Emit) (string, error) {
		runs.Add(1)
		return "done", nil
	})
	from := createTest(t, r, "omp", "origin")
	to := createTest(t, r, "claude", "worker")
	_ = from
	startTest(t, r, to.ID)
	if _, err := r.db.Exec(`CREATE TRIGGER fail_terminal BEFORE UPDATE ON tasks WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'injected terminal storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	task := sendTest(t, r, "human:operator", to.ID, "effect then fail to record")
	deadline := time.Now().Add(5 * time.Second)
	var agent model.Agent
	for time.Now().Before(deadline) {
		agent, _ = r.Agent(admin, to.ID)
		if agent.State == "error" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agent.State != "error" || agent.Error == "" {
		t.Fatalf("identity must stop dispatch, got state=%q error=%q", agent.State, agent.Error)
	}
	got, err := r.Task(admin, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == "completed" {
		t.Fatalf("task must not claim durable success: %s", got.State)
	}
	if runs.Load() != 1 {
		t.Fatalf("turn must execute exactly once, ran %d", runs.Load())
	}
}
