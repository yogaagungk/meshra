package relay

import (
	"database/sql"
	"errors"
	"testing"

	"meshra/internal/model"
)

func TestAdmissionFeatureReceiptFailureRollsBack(t *testing.T) {
	r, _ := openTest(t, nil)
	source := createTest(t, r, "omp", "source")
	dest := createTest(t, r, "codex", "dest")
	q := model.SendRequest{From: source.ID, To: dest.ID, Text: "preserve atomic admission", IdempotencyKey: "receipt-key"}
	failure := errors.New("receipt persistence failed")
	r.mu.Lock()
	_, err := r.admitLocked(admin, q, &admissionMeta{persist: func(tx *sql.Tx, task model.Task) error { return failure }})
	r.mu.Unlock()
	if !errors.Is(err, failure) {
		t.Fatalf("admission: %v", err)
	}
	var count int
	if err := r.db.QueryRow("SELECT count(*) FROM tasks WHERE idempotency_key=?", q.IdempotencyKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed receipt left a task")
	}
	task, err := r.Send(admin, q)
	if err != nil {
		t.Fatal(err)
	}
	same, err := r.Send(admin, q)
	if err != nil || same.ID != task.ID {
		t.Fatalf("ordinary idempotency lost: %v", err)
	}
	q.Text = "changed request"
	_, err = r.Send(admin, q)
	requireStatus(t, err, 409)
}

func TestConsoleAuthorityRequiresLiveSession(t *testing.T) {
	r, _ := openTest(t, nil)
	requireStatus(t, r.requireConsole(admin), 403)
	p := model.Principal{Admin: true, ConsoleSessionID: "session-digest"}
	r.SetConsoleSessionValidator(func(id string) bool {
		// A validator may call the service; relay ownership must not be held.
		_, err := r.Agents(admin)
		return id == p.ConsoleSessionID && err == nil
	})
	if err := r.requireConsole(p); err != nil {
		t.Fatal(err)
	}
	r.RevokeConsoleSession(p.ConsoleSessionID)
	requireStatus(t, r.requireConsole(p), 403)
}
