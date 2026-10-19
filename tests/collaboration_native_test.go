package tests

import (
	"meshra/internal/adapters/claude"
	"meshra/internal/adapters/codex"
	"meshra/internal/adapters/omp"
	"meshra/internal/adapters/opencode"
	"meshra/internal/model"
	"meshra/internal/relay"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests use real adapters and providers. No fallback runtime is installed.
func TestNativeCollaboration(t *testing.T) {
	if os.Getenv("MESHRA_V03_NATIVE") != "1" {
		t.Skip("opt in with MESHRA_V03_NATIVE=1; real provider costs")
	}
	nativeCollaboration(t, "opencode")
}
func TestNativeTwoHarnessCollaboration(t *testing.T) {
	if os.Getenv("MESHRA_V03_TWO_HARNESS") != "1" {
		t.Skip("opt in with MESHRA_V03_TWO_HARNESS=1; real provider costs")
	}
	nativeCollaboration(t, "omp")
}
func nativeCollaboration(t *testing.T, publisher string) {
	t.Helper()
	p := model.Principal{Admin: true}
	r, err := relay.Open(t.TempDir(), map[string]model.Adapter{"claude": claude.New(), "codex": codex.New(), "omp": omp.New(), "opencode": opencode.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ids := []string{publisher + ":apmobileprod", "codex:implementer", "omp:reviewer", "claude:apmobilewallet"}
	for _, id := range ids {
		parts := strings.Split(id, ":")
		config := model.Config{}
		switch parts[0] {
		case "omp":
			config = model.Config{Model: os.Getenv("MESHRA_OMP_MODEL"), Provider: os.Getenv("MESHRA_OMP_PROVIDER")}
		case "codex":
			config = model.Config{Model: os.Getenv("MESHRA_CODEX_MODEL"), Provider: os.Getenv("MESHRA_CODEX_PROVIDER")}
		case "opencode":
			config = model.Config{Model: os.Getenv("MESHRA_OPENCODE_MODEL"), Provider: os.Getenv("MESHRA_OPENCODE_PROVIDER")}
		case "claude":
			config.Model = os.Getenv("MESHRA_CLAUDE_MODEL")
		}
		if _, err = r.Create(p, model.CreateAgent{Harness: parts[0], Name: parts[1], Cwd: t.TempDir(), Config: config}); err != nil {
			t.Fatal(err)
		}
	}
	v, err := r.Collaborate(p, "conversation.create", model.CollaborationRequest{Title: "native contract implementation review", Participants: ids[:3]})
	if err != nil {
		t.Fatal(err)
	}
	conv := v.(model.Conversation)
	// All named sessions are started through their real adapter; wallet stays offline.
	for _, id := range ids[:3] {
		if _, err = r.Action(p, id, "start"); err != nil {
			t.Fatal(err)
		}
	}
	await := func(id string) model.Task {
		t.Helper()
		until := time.Now().Add(150 * time.Second)
		for time.Now().Before(until) {
			task, e := r.Task(p, id)
			if e != nil {
				t.Fatal(e)
			}
			if model.Terminal(task.State) {
				if task.State != "completed" {
					t.Fatalf("real %s task failed: %s (%s)", task.To, task.ErrorCategory, task.Error)
				}
				return task
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("real provider turn exceeded 150 seconds")
		return model.Task{}
	}
	first, err := r.Send(p, model.SendRequest{From: ids[2], To: ids[0], Text: "Reply exactly MESHRA_V03_PINNED_CONTRACT. Do not use tools.", ConversationID: conv.ID, TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	first = await(first.ID)
	if !strings.Contains(first.Result, "MESHRA_V03_PINNED_CONTRACT") {
		t.Fatal("publisher did not produce contract marker")
	}
	owner := model.Principal{AgentID: ids[0]}
	v, err = r.Collaborate(owner, "context.publish", model.CollaborationRequest{Title: "native contract", Content: first.Result})
	if err != nil {
		t.Fatal(err)
	}
	ctx := v.(model.SharedContext)
	for _, id := range ids[1:3] {
		if _, err = r.Collaborate(owner, "context.grant", model.CollaborationRequest{ContextID: ctx.ID, AgentID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = r.Collaborate(model.Principal{AgentID: ids[3]}, "context.get", model.CollaborationRequest{ContextID: ctx.ID, Version: 1}); err == nil {
		t.Fatal("wallet context leak")
	}
	if _, err = r.Collaborate(owner, "context.publish", model.CollaborationRequest{ContextID: ctx.ID, Version: 2, Content: "MESHRA_V03_DIFFERENT_VERSION"}); err != nil {
		t.Fatal(err)
	}
	refs := []model.ContextRef{{ContextID: ctx.ID, Version: 1}}
	implement, err := r.Send(p, model.SendRequest{From: ids[0], To: ids[1], Text: "Read the shared contract data and reply exactly MESHRA_V03_IMPLEMENT_OK if it includes MESHRA_V03_PINNED_CONTRACT. Do not use tools.", ConversationID: conv.ID, ContextRefs: refs, DependsOn: []string{first.ID}, TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	implement = await(implement.ID)
	if implement.ResultRef == nil {
		t.Fatal("implementation lacks immutable result reference")
	}
	if _, err = r.Collaborate(model.Principal{AgentID: ids[1]}, "context.grant", model.CollaborationRequest{ContextID: implement.ResultRef.ContextID, AgentID: ids[2]}); err != nil {
		t.Fatal(err)
	}
	reviewRefs := append(append([]model.ContextRef{}, refs...), *implement.ResultRef)
	review, err := r.Send(p, model.SendRequest{From: ids[0], To: ids[2], Text: "Read the pinned contract and implementation result data. Reply exactly MESHRA_V03_REVIEW_OK if they include MESHRA_V03_PINNED_CONTRACT and MESHRA_V03_IMPLEMENT_OK. Do not use tools.", ConversationID: conv.ID, ContextRefs: reviewRefs, DependsOn: []string{implement.ID}, TimeoutSeconds: 240})
	if err != nil {
		t.Fatal(err)
	}
	review = await(review.ID)
	if !strings.Contains(implement.Result, "MESHRA_V03_IMPLEMENT_OK") || !strings.Contains(review.Result, "MESHRA_V03_REVIEW_OK") {
		t.Fatal("real providers did not confirm pinned data")
	}
	events, err := r.Collaborate(p, "conversation.events", model.CollaborationRequest{ID: conv.ID})
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, event := range events.([]map[string]any) {
		if event["operation"] == "task.completed" {
			completed++
		}
	}
	if completed != 3 {
		t.Fatalf("conversation did not receive three results: %d", completed)
	}
	wallet, err := r.Agent(p, ids[3])
	if err != nil || wallet.SessionID != "" {
		t.Fatal("wallet received a session")
	}
	t.Logf("real harness publisher=%s implementer=codex reviewer=omp; pinned context consumed; wallet denied; conversation=%s tasks=%s,%s,%s", publisher, conv.ID, first.ID, implement.ID, review.ID)
}

func TestNativeLifecycle(t *testing.T) {
	if os.Getenv("MESHRA_V03_NATIVE") != "1" {
		t.Skip("opt-in real OMP streaming cancellation/restart; provider costs")
	}
	dir := t.TempDir()
	p := model.Principal{Admin: true}
	adapters := map[string]model.Adapter{"omp": omp.New()}
	r, err := relay.Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close() }()
	for _, id := range []string{"omp:v03lifecycle", "codex:v03origin"} {
		parts := strings.Split(id, ":")
		config := model.Config{}
		if parts[0] == "omp" {
			config = model.Config{Model: os.Getenv("MESHRA_OMP_MODEL"), Provider: os.Getenv("MESHRA_OMP_PROVIDER")}
		}
		if _, err = r.Create(p, model.CreateAgent{Harness: parts[0], Name: parts[1], Cwd: t.TempDir(), Config: config}); err != nil {
			t.Fatal(err)
		}
	}
	v, err := r.Collaborate(p, "conversation.create", model.CollaborationRequest{Title: "native lifecycle", Participants: []string{"omp:v03lifecycle", "codex:v03origin"}})
	if err != nil {
		t.Fatal(err)
	}
	conv := v.(model.Conversation)
	if _, err = r.Action(p, "omp:v03lifecycle", "start"); err != nil {
		t.Fatal(err)
	}
	send := func(text string) model.Task {
		t.Helper()
		task, e := r.Send(p, model.SendRequest{From: "codex:v03origin", To: "omp:v03lifecycle", ConversationID: conv.ID, Text: text, TimeoutSeconds: 120})
		if e != nil {
			t.Fatal(e)
		}
		return task
	}
	streaming := func(id string) {
		t.Helper()
		until := time.Now().Add(90 * time.Second)
		for time.Now().Before(until) {
			events, e := r.Events(p, id, 0)
			if e != nil {
				t.Fatal(e)
			}
			for _, event := range events {
				if event.Type == "progress" && strings.Contains(event.Data, "text buffered") {
					return
				}
			}
			task, e := r.Task(p, id)
			if e != nil {
				t.Fatal(e)
			}
			if model.Terminal(task.State) {
				t.Fatalf("turn ended before cancellation: %s %s", task.State, task.Error)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("no real streaming delta within 90 seconds")
	}
	long := "Write at least 3000 words explaining sorting algorithms, starting immediately with the word Sorting. Do not use tools."
	first := send(long)
	second := send("Reply exactly MESHRA_V03_SERIALIZED_OK. Do not use tools.")
	streaming(first.ID)
	queued, err := r.Task(p, second.ID)
	if err != nil || queued.Attempts != 0 {
		t.Fatal("busy session dispatched concurrent turn", queued, err)
	}
	cancelled, err := r.Cancel(p, first.ID)
	if err != nil || cancelled.State != "cancelled" || !cancelled.Ambiguous {
		t.Fatal("native cancellation not recorded", cancelled, err)
	}
	until := time.Now().Add(15 * time.Second)
	stopped := false
	for time.Now().Before(until) {
		a, e := r.Agent(p, "omp:v03lifecycle")
		if e != nil {
			t.Fatal(e)
		}
		if a.State == "error" {
			stopped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("cancelled adapter did not release busy turn")
	}
	if _, err = r.Action(p, "omp:v03lifecycle", "restart"); err != nil {
		t.Fatal(err)
	}
	until = time.Now().Add(90 * time.Second)
	completed := false
	for time.Now().Before(until) {
		task, e := r.Task(p, second.ID)
		if e != nil {
			t.Fatal(e)
		}
		if model.Terminal(task.State) {
			if task.State != "completed" || task.Attempts != 1 || !strings.Contains(task.Result, "MESHRA_V03_SERIALIZED_OK") {
				t.Fatal("queued native turn failed", task.State, task.Error)
			}
			completed = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !completed {
		t.Fatal("serialized native turn did not finish")
	}
	third := send(long)
	streaming(third.ID)
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, openErr := relay.Open(dir, adapters)
	if openErr != nil {
		t.Fatal(openErr)
	}
	r = reopened
	interrupted, err := r.Task(p, third.ID)
	if err != nil || interrupted.State != "failed" || !interrupted.Ambiguous || interrupted.Attempts != 1 || interrupted.ErrorCategory != "uncertain_execution" {
		t.Fatal("interrupted native turn was not preserved uncertain", interrupted, err)
	}
	if _, err = r.Action(p, "omp:v03lifecycle", "start"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	interrupted, err = r.Task(p, third.ID)
	if err != nil || interrupted.State != "failed" || interrupted.Attempts != 1 {
		t.Fatal("uncertain native turn replayed", interrupted, err)
	}
	t.Log("real OMP streaming observed before cancellation and shutdown; busy-session serialization, cancellation cleanup, exact-session restart and no uncertain replay passed")
}
