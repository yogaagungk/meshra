package relay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/model"
)

var admin = model.Principal{Admin: true}

type fakeRuntime struct {
	session string
	run     func(context.Context, model.Turn, model.Emit) (string, error)
}

func (f *fakeRuntime) SessionID() string { return f.session }
func (f *fakeRuntime) Close() error      { return nil }
func (f *fakeRuntime) Run(c context.Context, t model.Turn, e model.Emit) (string, error) {
	if f.run != nil {
		return f.run(c, t, e)
	}
	e("delta", t.Task.Text)
	return "reply:" + t.Task.To + ":" + t.Task.Text, nil
}
func fakeAdapters(run func(context.Context, model.Turn, model.Emit) (string, error)) map[string]model.Adapter {
	out := map[string]model.Adapter{}
	for _, h := range []string{"omp", "claude", "codex", "opencode"} {
		out[h] = model.Adapter{Factory: func(_ context.Context, a model.Agent, e model.Emit) (model.Runtime, error) {
			sid := a.SessionID
			if sid == "" {
				sid = "native-" + randomID()
			}
			return &fakeRuntime{session: sid, run: run}, nil
		}, Capabilities: map[string]bool{"prompt": true, "resume": true, "cancel": true, "stream": true}}
	}
	return out
}
func openTest(t *testing.T, run func(context.Context, model.Turn, model.Emit) (string, error)) (*Relay, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := Open(dir, fakeAdapters(run))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r, dir
}
func createTest(t *testing.T, r *Relay, h, n string) model.Agent {
	t.Helper()
	a, err := r.Create(admin, model.CreateAgent{Harness: h, Name: n, Cwd: t.TempDir(), Policy: model.Policy{Destinations: []string{"*"}, Operations: []string{"list", "send", "request", "status", "events", "cancel"}, Delegate: true}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func startTest(t *testing.T, r *Relay, id string) model.Agent {
	t.Helper()
	a, err := r.Action(admin, id, "start")
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func sendTest(t *testing.T, r *Relay, from, to, text string) model.Task {
	t.Helper()
	v, err := r.Send(admin, model.SendRequest{From: from, To: to, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func awaitTask(t *testing.T, r *Relay, id string) model.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err := r.Task(admin, id)
		if err != nil {
			t.Fatal(err)
		}
		if model.Terminal(v.State) {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", id)
	return model.Task{}
}
func requireStatus(t *testing.T, err error, status int) {
	t.Helper()
	var e *model.Error
	if !errors.As(err, &e) || e.Status != status {
		t.Fatalf("expected status %d, got %v", status, err)
	}
}

func TestRegistryIdentityIsolationAndReplyOwnership(t *testing.T) {
	var mu sync.Mutex
	received := map[string][]string{}
	r, dir := openTest(t, func(_ context.Context, turn model.Turn, e model.Emit) (string, error) {
		mu.Lock()
		received[turn.Task.To] = append(received[turn.Task.To], turn.Task.Text)
		mu.Unlock()
		e("delta", "answer")
		return "answer:" + turn.Task.Text, nil
	})
	prod := createTest(t, r, "claude", "exampleone")
	wallet := createTest(t, r, "claude", "exampletwo")
	origin := createTest(t, r, "omp", "exampleworker")
	p := startTest(t, r, prod.ID)
	w := startTest(t, r, wallet.ID)
	o := startTest(t, r, origin.ID)
	if p.SessionID == w.SessionID {
		t.Fatal("independent Claude identities share a session")
	}
	key, err := r.Token(admin, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := r.Authenticate(key)
	if err != nil {
		t.Fatal(err)
	}
	req, err := r.Send(principal, model.SendRequest{To: p.ID, Text: "Review API", Type: "request"})
	if err != nil {
		t.Fatal(err)
	}
	result := awaitTask(t, r, req.ID)
	if result.State != "completed" || result.Result != "answer:Review API" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.OriginAgentID != o.ID || result.OriginSessionID != o.SessionID || result.FromSessionID != o.SessionID || result.DestinationSessionID != p.SessionID {
		t.Fatalf("reply ownership changed: %+v", result)
	}
	mu.Lock()
	if len(received[w.ID]) != 0 || len(received[p.ID]) != 1 {
		t.Fatalf("cross-talk: %v", received)
	}
	mu.Unlock()
	// A different origin session cannot inspect the original reply.
	_, err = r.Task(model.Principal{AgentID: o.ID, SessionID: "unrelated-session"}, req.ID)
	requireStatus(t, err, 403)
	if _, err = r.Task(principal, req.ID); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	after, err := r2.Agent(admin, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != p.ID || after.SessionID != p.SessionID || after.State != "stopped" {
		t.Fatalf("identity lost on restart: %+v", after)
	}
	authenticated, err := r2.Authenticate(key)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := r2.Task(authenticated, req.ID)
	if err != nil || persisted.Result != "answer:Review API" {
		t.Fatalf("reply lost: %+v %v", persisted, err)
	}
}
func TestRegistryValidationAndLifecycle(t *testing.T) {
	r, dir := openTest(t, nil)
	a := createTest(t, r, "claude", "prod")
	_, err := r.Create(admin, model.CreateAgent{Harness: "claude", Name: "prod", Cwd: a.Cwd})
	requireStatus(t, err, 409)
	for _, c := range []model.CreateAgent{{Harness: "claude", Name: "Upper", Cwd: a.Cwd}, {Harness: "bad", Name: "prod", Cwd: a.Cwd}, {Harness: "omp", Name: "missing", Cwd: filepath.Join(a.Cwd, "missing")}, {Harness: "omp", Name: "many", Cwd: a.Cwd, Concurrency: 2}} {
		_, err = r.Create(admin, c)
		requireStatus(t, err, 400)
	}
	_, err = Open(dir, fakeAdapters(nil))
	if err == nil {
		t.Fatal("second daemon acquired same data directory")
	}
	startTest(t, r, a.ID)
	again := startTest(t, r, a.ID)
	restarted, err := r.Action(admin, a.ID, "restart")
	if err != nil || restarted.SessionID != again.SessionID {
		t.Fatalf("resume lost mapping: %+v %v", restarted, err)
	}
	if err = r.Remove(admin, a.ID); err == nil {
		t.Fatal("removed running agent")
	}
	if _, err = r.Action(admin, a.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if err = r.Remove(admin, a.ID); err != nil {
		t.Fatal(err)
	}
	_, err = r.Create(admin, model.CreateAgent{Harness: "claude", Name: "prod", Cwd: a.Cwd})
	requireStatus(t, err, 409)
	info, err := os.Stat(filepath.Join(dir, "admin.token"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe token mode: %v %v", info, err)
	}
	_, err = r.Authenticate("invented")
	requireStatus(t, err, 401)
}
func TestBusyQueueAndRepositorySerialization(t *testing.T) {
	entered := make(chan model.Task, 4)
	release := make(chan struct{}, 4)
	r, _ := openTest(t, func(ctx context.Context, v model.Turn, _ model.Emit) (string, error) {
		entered <- v.Task
		select {
		case <-release:
			return v.Task.Text, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	source := createTest(t, r, "omp", "source")
	a := createTest(t, r, "claude", "one")
	b, err := r.Create(admin, model.CreateAgent{Harness: "codex", Name: "two", Cwd: a.Cwd})
	if err != nil {
		t.Fatal(err)
	}
	startTest(t, r, a.ID)
	startTest(t, r, b.ID)
	first := sendTest(t, r, source.ID, a.ID, "first")
	select {
	case v := <-entered:
		if v.ID != first.ID {
			t.Fatal("wrong first task")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("not dispatched")
	}
	second := sendTest(t, r, source.ID, a.ID, "second")
	third := sendTest(t, r, source.ID, b.ID, "same repository")
	time.Sleep(100 * time.Millisecond)
	for _, v := range []model.Task{second, third} {
		state, _ := r.Task(admin, v.ID)
		if state.State != "queued" {
			t.Fatalf("conflicting turn ran: %+v", state)
		}
	}
	release <- struct{}{}
	if got := awaitTask(t, r, first.ID); got.Result != "first" {
		t.Fatal(got)
	}
	next := <-entered
	if next.ID != second.ID {
		t.Fatalf("FIFO violation: %+v", next)
	}
	release <- struct{}{}
	awaitTask(t, r, second.ID)
	next = <-entered
	if next.ID != third.ID {
		t.Fatal("repository lock not released")
	}
	release <- struct{}{}
	awaitTask(t, r, third.ID)
}
func TestConcurrentRequestsAndAdmissionDeduplication(t *testing.T) {
	r, _ := openTest(t, nil)
	origin := createTest(t, r, "omp", "source")
	a := createTest(t, r, "claude", "one")
	b := createTest(t, r, "opencode", "two")
	startTest(t, r, a.ID)
	startTest(t, r, b.ID)
	q := model.SendRequest{From: origin.ID, To: a.ID, Text: "first", IdempotencyKey: "same"}
	one, err := r.Send(admin, q)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := r.Send(admin, q)
	if err != nil || duplicate.ID != one.ID {
		t.Fatalf("duplicate admission: %v %v", duplicate, err)
	}
	q.Text = "changed"
	_, err = r.Send(admin, q)
	requireStatus(t, err, 409)
	two := sendTest(t, r, origin.ID, b.ID, "second")
	firstResult := awaitTask(t, r, one.ID)
	secondResult := awaitTask(t, r, two.ID)
	if firstResult.CorrelationID == secondResult.CorrelationID || firstResult.Result != "reply:"+a.ID+":first" || secondResult.Result != "reply:"+b.ID+":second" {
		t.Fatalf("crossed responses: %+v %+v", firstResult, secondResult)
	}
}
func TestFailureTimeoutCancelAndRetry(t *testing.T) {
	r, _ := openTest(t, func(ctx context.Context, v model.Turn, _ model.Emit) (string, error) {
		if v.Task.Text == "fail" {
			return "", errors.New("child exited")
		}
		<-ctx.Done()
		return "", ctx.Err()
	})
	source := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "codex", "dest")
	_, err := r.Send(admin, model.SendRequest{From: source.ID, To: "claude:absent", Text: "hello"})
	requireStatus(t, err, 404)
	for _, q := range []model.SendRequest{{From: source.ID, To: dest.ID, Text: ""}, {From: source.ID, To: dest.ID, Text: "x", TimeoutSeconds: -1}} {
		_, err = r.Send(admin, q)
		requireStatus(t, err, 400)
	}
	_, err = r.Send(admin, model.SendRequest{From: source.ID, To: dest.ID, Text: strings.Repeat("x", model.MaxTextBytes+1)})
	requireStatus(t, err, 413)
	offline, err := r.Send(admin, model.SendRequest{From: source.ID, To: dest.ID, Text: "offline", TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := awaitTask(t, r, offline.ID); got.State != "timed_out" || got.Attempts != 0 {
		t.Fatal(got)
	}
	cancelled := sendTest(t, r, source.ID, dest.ID, "queued")
	if _, err = r.Cancel(admin, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if got := awaitTask(t, r, cancelled.ID); got.State != "cancelled" || got.Attempts != 0 {
		t.Fatal(got)
	}
	startTest(t, r, dest.ID)
	failed := sendTest(t, r, source.ID, dest.ID, "fail")
	if got := awaitTask(t, r, failed.ID); got.State != "failed" || !strings.Contains(got.Error, "child exited") {
		t.Fatal(got)
	}
	if state, err := r.Agent(admin, dest.ID); err != nil || state.State != "error" {
		t.Fatalf("failed runtime should stop new dispatch: %+v %v", state, err)
	}
	if _, err = r.Action(admin, dest.ID, "restart"); err != nil {
		t.Fatal(err)
	}
	again, err := r.Retry(admin, failed.ID)
	if err != nil || again.State != "queued" {
		t.Fatalf("retry failed %v %v", again, err)
	}
	if got := awaitTask(t, r, failed.ID); got.Attempts != 2 {
		t.Fatal(got)
	}
	if _, err = r.Action(admin, dest.ID, "restart"); err != nil {
		t.Fatal(err)
	}
	running, err := r.Send(admin, model.SendRequest{From: source.ID, To: dest.ID, Text: "wait", TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := awaitTask(t, r, running.ID); got.State != "timed_out" || got.Attempts != 1 {
		t.Fatal(got)
	}
}
func TestQueuedRestartAndUncertainRecovery(t *testing.T) {
	r, dir := openTest(t, nil)
	source := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "claude", "dest")
	q := sendTest(t, r, source.ID, dest.ID, "preserved")
	uncertain := sendTest(t, r, source.ID, dest.ID, "uncertain")
	r.mu.Lock()
	uncertain.State = "dispatching"
	uncertain.Attempts = 1
	if err := r.transition(uncertain, "dispatching", "simulated durable pre-crash claim"); err != nil {
		t.Fatal(err)
	}
	uncertain.State = "running"
	if err := r.transition(uncertain, "running", "simulated durable pre-crash state"); err != nil {
		t.Fatal(err)
	}
	r.mu.Unlock()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	got, err := r2.Task(admin, uncertain.ID)
	if err != nil || got.State != "failed" || got.Attempts != 1 || !strings.Contains(got.Error, "uncertain") {
		t.Fatalf("unsafe recovery: %+v %v", got, err)
	}
	startTest(t, r2, dest.ID)
	if got = awaitTask(t, r2, q.ID); got.State != "completed" || got.Attempts != 1 || got.Result != "reply:"+dest.ID+":preserved" {
		t.Fatal(got)
	}
	ev, err := r2.Events(admin, uncertain.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ev[len(ev)-1].Type != "failed" {
		t.Fatalf("recovery not audited: %+v", ev)
	}
}
func TestDelegationCorrelationPermissionsAndCycleSafety(t *testing.T) {
	var r *Relay
	childIDs := make(chan string, 2)
	run := func(ctx context.Context, v model.Turn, e model.Emit) (string, error) {
		if v.Task.To == "claude:middle" {
			principal, err := r.Authenticate(v.Token)
			if err != nil {
				return "", err
			}
			child, err := r.Send(principal, model.SendRequest{To: "codex:leaf", Text: "leaf work", Type: "request"})
			if err != nil {
				return "", err
			}
			childIDs <- child.ID
			for {
				got, err := r.Task(principal, child.ID)
				if err != nil {
					return "", err
				}
				if model.Terminal(got.State) {
					return "middle:" + got.Result, nil
				}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if v.Task.To == "codex:leaf" {
			p, err := r.Authenticate(v.Token)
			if err != nil {
				return "", err
			}
			_, err = r.Send(p, model.SendRequest{To: "omp:origin", Text: "cycle"})
			var me *model.Error
			if !errors.As(err, &me) || me.Status != 409 {
				return "", errors.New("cycle was not rejected")
			}
			return "leaf reply to original OMP", nil
		}
		return "unexpected", nil
	}
	r, _ = openTest(t, run)
	origin := createTest(t, r, "omp", "origin")
	middle := createTest(t, r, "claude", "middle")
	leaf := createTest(t, r, "codex", "leaf")
	o := startTest(t, r, origin.ID)
	startTest(t, r, middle.ID)
	startTest(t, r, leaf.ID)
	root, err := r.Send(admin, model.SendRequest{From: o.ID, To: middle.ID, Text: "delegate", Type: "request", MaxHops: 8})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitTask(t, r, root.ID)
	if got.State != "completed" || got.Result != "middle:leaf reply to original OMP" {
		t.Fatal(got)
	}
	childID := <-childIDs
	child, err := r.Task(admin, childID)
	if err != nil {
		t.Fatal(err)
	}
	if child.CorrelationID != root.CorrelationID || child.ParentTaskID != root.ID || child.HopCount != 2 || child.MaxHops != 8 || child.OriginAgentID != o.ID || child.OriginSessionID != o.SessionID {
		t.Fatalf("delegation ownership lost: %+v", child)
	}
	if child.Deadline.After(root.Deadline) {
		t.Fatalf("delegation extended the parent deadline: child %s parent %s", child.Deadline, root.Deadline)
	}
	p := model.Principal{AgentID: middle.ID, SessionID: child.FromSessionID}
	_, err = r.Send(p, model.SendRequest{From: o.ID, To: leaf.ID, Text: "impersonate"})
	requireStatus(t, err, 403)
	restricted, err := r.Create(admin, model.CreateAgent{Harness: "omp", Name: "restricted", Cwd: t.TempDir(), Policy: model.Policy{Operations: []string{"send", "status"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Send(model.Principal{AgentID: restricted.ID}, model.SendRequest{To: leaf.ID, Text: "denied"})
	requireStatus(t, err, 403)
	_, err = r.Send(model.Principal{AgentID: restricted.ID}, model.SendRequest{To: leaf.ID, Text: "denied", Type: "request"})
	requireStatus(t, err, 403)
	_, err = r.Send(model.Principal{AgentID: restricted.ID}, model.SendRequest{To: leaf.ID, Text: "denied", ParentTaskID: root.ID})
	requireStatus(t, err, 403)
}
func TestCancellationDiscardsPartialTextAndKeepsReplay(t *testing.T) {
	entered := make(chan struct{})
	r, _ := openTest(t, func(ctx context.Context, v model.Turn, e model.Emit) (string, error) {
		e("delta", "sk-proj-")
		e("delta", "abcdefghijklmnopqrstuvwxyz0123456789")
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	source := createTest(t, r, "omp", "source")
	dest := createTest(t, r, "opencode", "dest")
	startTest(t, r, dest.ID)
	task := sendTest(t, r, source.ID, dest.ID, "stream")
	select {
	case <-entered:
	case <-time.After(4 * time.Second):
		t.Fatal("turn not entered")
	}
	if _, err := r.Cancel(admin, task.ID); err != nil {
		t.Fatal(err)
	}
	got := awaitTask(t, r, task.ID)
	if got.State != "cancelled" {
		t.Fatal(got)
	}
	var last int64
	count := 0
	for {
		events, err := r.Events(admin, task.ID, last)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			if event.ID <= last {
				t.Fatal("event order or replay duplicate")
			}
			last = event.ID
			if event.Type == "delta" {
				count++
			}
		}
	}
	if count != 0 {
		t.Fatalf("cancelled turn exposed partial unredacted text: %d events", count)
	}
}

func TestBoundedInboxAndIdempotentAcceptanceAtCapacity(t *testing.T) {
	r, _ := openTest(t, nil)
	source := createTest(t, r, "omp", "source")
	dest := createTest(t, r, "claude", "dest")
	q := model.SendRequest{From: source.ID, To: dest.ID, Text: "queued", IdempotencyKey: "first"}
	first, err := r.Send(admin, q)
	if err != nil {
		t.Fatal(err)
	}
	for range queueLimit - 1 {
		sendTest(t, r, source.ID, dest.ID, "queued")
	}
	_, err = r.Send(admin, model.SendRequest{From: source.ID, To: dest.ID, Text: "overflow"})
	requireStatus(t, err, 429)
	same, err := r.Send(admin, q)
	if err != nil || same.ID != first.ID {
		t.Fatalf("idempotent admission rejected at capacity: %+v %v", same, err)
	}
	if _, err = r.Cancel(admin, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Send(admin, model.SendRequest{From: source.ID, To: dest.ID, Text: "freed slot"}); err != nil {
		t.Fatal(err)
	}
}

func TestTurnCredentialAncestryAndExpiry(t *testing.T) {
	var r *Relay
	token := make(chan string, 1)
	release := make(chan struct{})
	run := func(ctx context.Context, v model.Turn, _ model.Emit) (string, error) {
		token <- v.Token
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	r, _ = openTest(t, run)
	origin := createTest(t, r, "omp", "origin")
	middle := createTest(t, r, "claude", "middle")
	leaf := createTest(t, r, "codex", "leaf")
	startTest(t, r, origin.ID)
	startTest(t, r, middle.ID)
	startTest(t, r, leaf.ID)
	root, err := r.Send(admin, model.SendRequest{From: origin.ID, To: middle.ID, Text: "parent", MaxHops: 1})
	if err != nil {
		t.Fatal(err)
	}
	var credential string
	select {
	case credential = <-token:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not start")
	}
	p, err := r.Authenticate(credential)
	if err != nil {
		t.Fatal(err)
	}
	if p.TaskID != root.ID || p.AgentID != middle.ID {
		t.Fatalf("wrong authenticated turn: %+v", p)
	}
	_, err = r.Send(p, model.SendRequest{To: leaf.ID, Text: "too deep"})
	requireStatus(t, err, 409)
	_, err = r.Send(p, model.SendRequest{To: leaf.ID, Text: "forged ancestry", ParentTaskID: "another-task"})
	requireStatus(t, err, 403)
	_, err = r.Send(p, model.SendRequest{To: origin.ID, Text: "cycle"})
	requireStatus(t, err, 409)
	close(release)
	awaitTask(t, r, root.ID)
	_, err = r.Authenticate(credential)
	requireStatus(t, err, 401)
}

func TestInterruptedRuntimePausesInboxUntilExplicitRestart(t *testing.T) {
	entered := make(chan struct{})
	r, _ := openTest(t, func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
		if turn.Task.Text == "second" {
			return "second completed", nil
		}
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	origin := createTest(t, r, "omp", "origin")
	destination := createTest(t, r, "claude", "destination")
	startTest(t, r, destination.ID)
	first := sendTest(t, r, origin.ID, destination.ID, "first")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first turn did not start")
	}
	second := sendTest(t, r, origin.ID, destination.ID, "second")
	if _, err := r.Cancel(admin, first.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		a, err := r.Agent(admin, destination.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a.State == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("interrupted runtime did not require recovery")
		}
		time.Sleep(time.Millisecond)
	}
	pending, err := r.Task(admin, second.ID)
	if err != nil || pending.State != "queued" || pending.Attempts != 0 {
		t.Fatalf("queued turn reached interrupted runtime: %+v %v", pending, err)
	}
	if _, err = r.Action(admin, destination.ID, "restart"); err != nil {
		t.Fatal(err)
	}
	done := awaitTask(t, r, second.ID)
	if done.State != "completed" || done.Result != "second completed" || done.Attempts != 1 {
		t.Fatalf("restart did not release queued work: %+v", done)
	}
}
