package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/model"
)

func TestPopulatedV1MigrationAndFutureVersionRejection(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "meshra.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(migration); err != nil {
		t.Fatal(err)
	}
	source := model.Agent{ID: "omp:origin", Harness: "omp", Name: "origin", Cwd: t.TempDir(), State: "registered"}
	dest := model.Agent{ID: "claude:dest", Harness: "claude", Name: "dest", Cwd: t.TempDir(), State: "registered"}
	for _, a := range []model.Agent{source, dest} {
		b, _ := json.Marshal(a)
		if _, err = db.Exec("INSERT INTO agents VALUES(?,?)", a.ID, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	old := model.Task{ID: "legacy", MessageID: "legacy-message", CorrelationID: "legacy-correlation", From: source.ID, To: dest.ID, Type: "request", Text: "retain identity", State: "queued", IdempotencyKey: "key", CreatedAt: now(), UpdatedAt: now(), Deadline: now().Add(time.Hour)}
	b, _ := json.Marshal(old)
	if _, err = db.Exec("INSERT INTO tasks(seq,id,sender,sender_session,idempotency_key,state,body) VALUES(17,?,?,?,?,?,?)", old.ID, old.From, "", old.IdempotencyKey, old.State, string(b)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO events(id,task_id,kind,data,time) VALUES(23,?,'queued','legacy accepted',?)", old.ID, old.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Send(admin, model.SendRequest{From: source.ID, To: dest.ID, Type: "request", Text: old.Text, IdempotencyKey: "key"})
	if err != nil || got.ID != old.ID || got.MessageID != old.MessageID || got.CorrelationID != old.CorrelationID {
		t.Fatalf("lost v1 identity/idempotency: %+v %v", got, err)
	}
	trace, err := r.Trace(admin, old.ID)
	if err != nil || trace.Events[0].ID != 23 || trace.Events[0].Data != "legacy accepted" {
		t.Fatalf("lost event history: %+v %v", trace, err)
	}
	distinct, err := r.Send(admin, model.SendRequest{From: source.ID, To: dest.ID, Type: "send", Text: old.Text, IdempotencyKey: "key"})
	if err != nil || distinct.ID == old.ID {
		t.Fatalf("operation scope not migrated: %+v %v", distinct, err)
	}
	startTest(t, r, dest.ID)
	if done := awaitTask(t, r, old.ID); done.State != "completed" || done.Result != "reply:"+dest.ID+":"+old.Text {
		t.Fatal(done)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", filepath.Join(dir, "meshra.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE schema_version SET version=99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if newer, e := Open(dir, fakeAdapters(nil)); e == nil {
		newer.Close()
		t.Fatal("future database opened")
	}
	db, err = sql.Open("sqlite", filepath.Join(dir, "meshra.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err = db.QueryRow("SELECT max(version) FROM schema_version").Scan(&version); err != nil || version != 99 {
		t.Fatalf("future schema modified: %d %v", version, err)
	}
}

func TestFailedMigrationRollsBackWithoutLosingV1Data(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "meshra.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(migration); err != nil {
		t.Fatal(err)
	}
	// Invalid persisted JSON is diagnosed, not silently discarded in a rebuild.
	if _, err = db.Exec("INSERT INTO tasks(id,sender,sender_session,state,body) VALUES('bad','omp:source','','queued','not json')"); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err == nil {
		t.Fatal("corrupt task migrated")
	}
	var version, count int
	if err = db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM tasks WHERE id='bad' AND body='not json'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if version != 1 || count != 1 {
		t.Fatalf("rollback lost original data: version=%d rows=%d", version, count)
	}
}

func TestOfflineDeliveryBackoffExhaustionAndExplicitRedrive(t *testing.T) {
	r, err := OpenWithOptions(t.TempDir(), fakeAdapters(nil), Options{MaxDeliveryAttempts: 3, RetryBase: 5 * time.Millisecond, RetryCap: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	origin := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "codex", "dest")
	task := sendTest(t, r, origin.ID, dest.ID, "offline")
	done := awaitTask(t, r, task.ID)
	if done.State != "failed" || done.ErrorCategory != "delivery_exhausted" || done.Ambiguous || done.Attempts != 0 || done.DeliveryAttempts != 3 {
		t.Fatalf("unsafe offline retry: %+v", done)
	}
	trace, err := r.Trace(admin, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	scheduled := 0
	for _, event := range trace.Events {
		if event.Type == "retry_scheduled" {
			scheduled++
		}
	}
	if scheduled != 2 {
		t.Fatalf("expected two bounded retry schedules, got %d", scheduled)
	}
	startTest(t, r, dest.ID)
	if _, err = r.Retry(admin, task.ID); err != nil {
		t.Fatal(err)
	}
	if got := awaitTask(t, r, task.ID); got.State != "completed" || got.Attempts != 1 || got.DeliveryAttempts != 0 {
		t.Fatalf("manual redrive failed: %+v", got)
	}
}

func TestPersistedRetryScheduleAndStartWakeup(t *testing.T) {
	r, dir := openTest(t, nil)
	origin := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "claude", "dest")
	task := sendTest(t, r, origin.ID, dest.ID, "scheduled")
	r.mu.Lock()
	current, err := r.task(task.ID)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	current.DeliveryAttempts = 2
	current.NextAttemptAt = now().Add(time.Hour)
	err = r.transition(current, "retry_scheduled", "offline backoff")
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	got, err := resumed.Task(admin, task.ID)
	if err != nil || got.DeliveryAttempts != 2 || !got.NextAttemptAt.Equal(current.NextAttemptAt) || got.Attempts != 0 {
		t.Fatalf("schedule lost: %+v %v", got, err)
	}
	startTest(t, resumed, dest.ID)
	if got = awaitTask(t, resumed, task.ID); got.State != "completed" || got.Attempts != 1 {
		t.Fatalf("start didn't wake inbox: %+v", got)
	}
}

func TestClaimTraceAndLateCancellationCannotRepeatEffects(t *testing.T) {
	entered := make(chan model.Task, 1)
	release := make(chan struct{})
	r, _ := openTest(t, func(_ context.Context, v model.Turn, e model.Emit) (string, error) {
		entered <- v.Task
		<-release
		e("delta", "late output")
		return "late success", nil
	})
	origin := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "claude", "dest")
	started := startTest(t, r, dest.ID)
	task := sendTest(t, r, origin.ID, dest.ID, "non-idempotent work")
	var claim model.Task
	select {
	case claim = <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("turn not entered")
	}
	if claim.State != "running" || claim.LeaseOwner == "" || !claim.LeaseUntil.Equal(task.Deadline) || claim.DestinationSessionID != started.SessionID {
		close(release)
		t.Fatalf("claim mapping lost: %+v", claim)
	}
	trace, err := r.Trace(admin, task.ID)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	states := []string{}
	for _, event := range trace.Events {
		if event.Type == "dispatching" || event.Type == "delivered" || event.Type == "running" {
			states = append(states, event.Type)
			if event.Attempt != 1 || event.CorrelationID != task.CorrelationID || event.To != dest.ID || event.SessionID != started.SessionID {
				close(release)
				t.Fatalf("event ownership lost: %+v", event)
			}
		}
	}
	if strings.Join(states, ",") != "dispatching,delivered,running" {
		close(release)
		t.Fatalf("delivery/completion conflated: %v", states)
	}
	if _, err = r.Cancel(admin, task.ID); err != nil {
		close(release)
		t.Fatal(err)
	}
	_, err = r.Retry(admin, task.ID)
	requireStatus(t, err, 409)
	close(release)
	// Close waits for the uncooperative invocation, making the late return observable.
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	// Inspect retained state directly because the relay is now closed.
	// Reopen uses the same directory discovered through the protected lock path.
	dir := filepath.Dir(r.lock.Name())
	again, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	got, err := again.Task(admin, task.ID)
	if err != nil || got.State != "cancelled" || !got.Ambiguous || got.Result != "" || got.Attempts != 1 || got.LeaseOwner != "" {
		t.Fatalf("late result overwrote cancellation: %+v %v", got, err)
	}
}

func TestSecretSafeIdempotencyAndOperationScope(t *testing.T) {
	r, _ := openTest(t, nil)
	source := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "claude", "dest")
	q := model.SendRequest{From: source.ID, To: dest.ID, Text: "token=sk-1234567890abcdefghijk", IdempotencyKey: "secret"}
	first, err := r.Send(admin, q)
	if err != nil {
		t.Fatal(err)
	}
	same, err := r.Send(admin, q)
	if err != nil || same.ID != first.ID {
		t.Fatalf("same raw request not deduplicated: %+v %v", same, err)
	}
	q.Text = "token=sk-abcdefghijk1234567890"
	_, err = r.Send(admin, q)
	requireStatus(t, err, 409)
	q.Text = "token=sk-1234567890abcdefghijk"
	q.Type = "request"
	other, err := r.Send(admin, q)
	if err != nil || other.ID == first.ID {
		t.Fatalf("operations incorrectly collide: %+v %v", other, err)
	}
	var body string
	if err = r.db.QueryRow("SELECT body FROM tasks WHERE id=?", first.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "sk-1234567890abcdefghijk") || strings.Contains(first.Text, "sk-1234567890abcdefghijk") {
		t.Fatal("raw credential persisted or returned")
	}
}

func TestRetentionPreservesActiveAncestryAndDropsExpiredHistory(t *testing.T) {
	r, _ := openTest(t, nil)
	source := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "claude", "dest")
	parent := sendTest(t, r, source.ID, dest.ID, "old parent")
	leaf := sendTest(t, r, source.ID, dest.ID, "active child")
	expired := sendTest(t, r, source.ID, dest.ID, "expired")
	for _, id := range []string{parent.ID, expired.ID} {
		if _, err := r.Cancel(admin, id); err != nil {
			t.Fatal(err)
		}
	}
	r.mu.Lock()
	old := now().Add(-31 * 24 * time.Hour)
	for _, id := range []string{parent.ID, expired.ID} {
		task, err := r.task(id)
		if err != nil {
			r.mu.Unlock()
			t.Fatal(err)
		}
		task.UpdatedAt = old
		b, _ := json.Marshal(task)
		if _, err = r.db.Exec("UPDATE tasks SET body=? WHERE id=?", string(b), id); err != nil {
			r.mu.Unlock()
			t.Fatal(err)
		}
	}
	child, err := r.task(leaf.ID)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	child.ParentTaskID = parent.ID
	b, _ := json.Marshal(child)
	_, err = r.db.Exec("UPDATE tasks SET body=? WHERE id=?", string(b), child.ID)
	if err == nil {
		err = r.prune()
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Task(admin, parent.ID); err != nil {
		t.Fatalf("retained child lost ancestry: %v", err)
	}
	if _, err = r.Task(admin, leaf.ID); err != nil {
		t.Fatalf("active task expired: %v", err)
	}
	_, err = r.Task(admin, expired.ID)
	requireStatus(t, err, 404)
	var count int
	if err = r.db.QueryRow("SELECT count(*) FROM events WHERE task_id=?", expired.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphaned expired events: %d %v", count, err)
	}
}

func TestRemovedWorkspaceCannotStart(t *testing.T) {
	r, _ := openTest(t, nil)
	dest := createTest(t, r, "claude", "dest")
	if err := os.Remove(dest.Cwd); err != nil {
		t.Fatal(err)
	}
	_, err := r.Action(admin, dest.ID, "start")
	requireStatus(t, err, 400)
}

type slowClaimHandler struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*slowClaimHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *slowClaimHandler) WithAttrs([]slog.Attr) slog.Handler     { return h }
func (h *slowClaimHandler) WithGroup(string) slog.Handler          { return h }
func (h *slowClaimHandler) Handle(_ context.Context, record slog.Record) error {
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "transition" && attr.Value.String() == "dispatching" {
			h.once.Do(func() {
				close(h.entered)
				<-h.release
			})
		}
		return true
	})
	return nil
}

func TestDeadlineSurvivesSlowClaimAndUncancellableRuntime(t *testing.T) {
	releaseRun := make(chan struct{})
	r, _ := openTest(t, func(context.Context, model.Turn, model.Emit) (string, error) {
		<-releaseRun // Models a downstream operation that ignores cancellation.
		return "late result", nil
	})
	origin := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "codex", "dest")
	startTest(t, r, dest.ID)
	gate := &slowClaimHandler{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(func() { release(); close(releaseRun) })
	r.SetLogger(slog.New(gate))
	task, err := r.Send(admin, model.SendRequest{From: origin.ID, To: dest.ID, Text: "bounded deadline", TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not reach the slow lifecycle sink")
	}
	// A slow durable transition/log sink consumes the original deadline.
	<-time.After(time.Until(task.Deadline.Add(20 * time.Millisecond)))
	release()
	until := time.Now().Add(600 * time.Millisecond)
	for {
		got, err := r.Task(admin, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if model.Terminal(got.State) {
			if got.State != "timed_out" || !got.Ambiguous || got.Result != "" {
				t.Fatalf("deadline misclassified uncertain execution: %+v", got)
			}
			break
		}
		if time.Now().After(until) {
			t.Fatal("scheduler extended the persisted deadline after slow claim persistence")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNamedClaudeAndOMPIsolationUnderConcurrencyAndRestart(t *testing.T) {
	type invocation struct {
		runtimeID string
		task      model.Task
	}
	entered := make(chan invocation, 3)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	adapters := map[string]model.Adapter{}
	for _, harness := range []string{"claude", "omp"} {
		adapters[harness] = model.Adapter{Factory: func(_ context.Context, agent model.Agent, _ model.Emit) (model.Runtime, error) {
			session := agent.SessionID
			if session == "" {
				session = "native-" + randomID()
			}
			return &fakeRuntime{session: session, run: func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
				entered <- invocation{runtimeID: agent.ID, task: turn.Task}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-release:
					return "native:" + agent.ID + ":" + turn.Task.Text, nil
				}
			}}, nil
		}}
	}
	dir := t.TempDir()
	core, err := Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	agents := []model.Agent{
		createTest(t, core, "claude", "exampleone"),
		createTest(t, core, "claude", "exampletwo"),
		createTest(t, core, "omp", "exampleworker"),
	}
	sessions := map[string]string{}
	for i := range agents {
		agents[i] = startTest(t, core, agents[i].ID)
		sessions[agents[i].ID] = agents[i].SessionID
	}
	token, err := core.Token(admin, agents[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	tasks := []model.Task{
		sendTest(t, core, agents[2].ID, agents[0].ID, "prod work"),
		sendTest(t, core, agents[2].ID, agents[1].ID, "wallet work"),
		sendTest(t, core, agents[0].ID, agents[2].ID, "ads work"),
	}
	// All three runtimes must enter before any is released, proving independent
	// progress rather than merely accepting requests into three queues.
	seen := map[string]bool{}
	until := time.NewTimer(10 * time.Second)
	defer until.Stop()
	for range tasks {
		select {
		case call := <-entered:
			if seen[call.runtimeID] || call.runtimeID != call.task.To || call.task.DestinationSessionID != sessions[call.runtimeID] {
				t.Fatalf("named instance/session crossed under concurrency: %+v", call)
			}
			seen[call.runtimeID] = true
		case <-until.C:
			t.Fatal("independent named runtimes did not progress concurrently")
		}
	}
	releaseAll()
	for _, task := range tasks {
		done := awaitTask(t, core, task.ID)
		if done.State != "completed" || done.Result != "native:"+task.To+":"+task.Text || done.CorrelationID != task.CorrelationID {
			t.Fatalf("reply crossed native instance/correlation: %+v", done)
		}
	}
	for _, agent := range agents {
		if _, err = core.Action(admin, agent.ID, "stop"); err != nil {
			t.Fatal(err)
		}
	}
	pending := sendTest(t, core, agents[2].ID, agents[1].ID, "wallet after restart")
	if err = core.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	for _, agent := range agents {
		current := startTest(t, resumed, agent.ID)
		if current.SessionID != sessions[agent.ID] {
			t.Fatalf("restart changed named native mapping: %+v", current)
		}
	}
	principal, err := resumed.Authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	done := awaitTask(t, resumed, pending.ID)
	if done.State != "completed" || done.Attempts != 1 || done.Result != "native:"+agents[1].ID+":"+pending.Text ||
		done.DestinationSessionID != sessions[agents[1].ID] || done.OriginSessionID != sessions[agents[2].ID] {
		t.Fatalf("restart crossed wallet/prod/origin session: %+v", done)
	}
	if reply, err := resumed.Task(principal, pending.ID); err != nil || reply.CorrelationID != pending.CorrelationID {
		t.Fatalf("original OMP requester lost reply: %+v %v", reply, err)
	}
}
