package relay

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"meshra/internal/model"
)

type rotationRuntime struct {
	session string
	run     func(context.Context, model.Turn, model.Emit) (string, error)
	close   func() error
}

func (r *rotationRuntime) SessionID() string { return r.session }
func (r *rotationRuntime) Run(c context.Context, t model.Turn, e model.Emit) (string, error) {
	return r.run(c, t, e)
}
func (r *rotationRuntime) Close() error {
	if r.close != nil {
		return r.close()
	}
	return nil
}

func TestProfileContextRotationWaitsForTeardownBeforeNextClaim(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	closeStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var factories atomic.Int32
	r.mu.Lock()
	adapter := r.adapters["codex"]
	adapter.Capabilities["durableHistory"] = true
	adapter.Factory = func(_ context.Context, a model.Agent, _ model.Emit) (model.Runtime, error) {
		generation := factories.Add(1)
		session := "rotation-" + randomID()
		return &rotationRuntime{session: session, run: func(_ context.Context, turn model.Turn, emit model.Emit) (string, error) {
			if generation == 1 {
				emit("context_usage", `{"usedTokens":80,"windowTokens":100,"estimated":false,"source":"native-fixture"}`)
			}
			return "bounded answer", nil
		}, close: func() error {
			if generation == 1 {
				close(closeStarted)
				<-release
			}
			return nil
		}}, nil
	}
	r.adapters["codex"] = adapter
	r.mu.Unlock()
	p := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	c := continuityConversation(t, r, "chat", p)
	first := continuitySend(t, r, p, c, "first")
	first = awaitTask(t, r, first.ID)
	second := continuitySend(t, r, p, c, "second")
	select {
	case <-closeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("native pressure did not rotate the idle context")
	}
	waiting, err := r.Task(admin, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.State != "queued" || waiting.Attempts != 0 {
		t.Fatalf("queued turn claimed before teardown: %+v", waiting)
	}
	r.mu.Lock()
	slot := r.slots[first.To]
	locked := slot != nil && r.repos[slot.repo]
	r.mu.Unlock()
	if !locked {
		t.Fatal("repository lock released while native teardown was pending")
	}
	releaseOnce.Do(func() { close(release) })
	second = awaitTask(t, r, second.ID)
	if second.State != "completed" || second.To != first.To || second.DestinationSessionID == first.DestinationSessionID {
		t.Fatalf("rotation changed identity or reused native context: first=%+v second=%+v", first, second)
	}
	if factories.Load() != 2 {
		t.Fatal("rotation created unexpected native sessions", factories.Load())
	}
	var epoch int
	binding := continuityBinding(t, r, p, c)
	if err = r.db.QueryRow("SELECT epoch FROM profile_context_epochs WHERE binding_id=?", binding.ID).Scan(&epoch); err != nil || epoch != 2 {
		t.Fatalf("epoch %d %v", epoch, err)
	}
	raw, _ := json.Marshal(map[string]any{"id": p.ID, "conversationId": c.ID, "expectedRevision": p.Revision})
	value, err := r.profileResetContext(admin, raw)
	if err != nil {
		t.Fatal(err)
	}
	reset := value.(profileEpoch)
	if reset.Epoch != 3 || reset.ResetAfterSeq < 1 {
		t.Fatalf("manual reset lost cutoff: %+v", reset)
	}
	r.mu.Lock()
	handoff, err := r.profileHandoff(binding, 0)
	r.mu.Unlock()
	if err != nil || handoff != "[]" {
		t.Fatalf("manual reset resurrected transcript: %s %v", handoff, err)
	}
	old, err := r.Task(admin, first.ID)
	if err != nil || old.DestinationSessionID != first.DestinationSessionID {
		t.Fatal("reset rewrote historical native ownership", err)
	}
}

func TestProfileUsagePreservesUnknownAndReportedAttribution(t *testing.T) {
	r, _ := continuityOpenTest(t, func(_ context.Context, _ model.Turn, emit model.Emit) (string, error) {
		emit("usage", `{"outputTokens":7,"model":"actual-model","provider":"actual-provider","reportedCost":0.02,"currency":"USD"}`)
		return "answer", nil
	})
	p := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	c := continuityConversation(t, r, "chat", p)
	task := awaitTask(t, r, continuitySend(t, r, p, c, "usage attribution").ID)
	if task.State != "completed" {
		t.Fatal(task)
	}
	var input *int64
	var output int64
	var modelName, provider, profileID, bindingID string
	var cost float64
	var revision int
	if err := r.db.QueryRow("SELECT input_tokens,output_tokens,reported_model,reported_provider,profile_id,profile_revision,binding_id,reported_cost FROM run_usage WHERE task_id=?", task.ID).Scan(&input, &output, &modelName, &provider, &profileID, &revision, &bindingID, &cost); err != nil {
		t.Fatal(err)
	}
	if input != nil || output != 7 || modelName != "actual-model" || provider != "actual-provider" || profileID != p.ID || revision != p.Revision || bindingID == "" || cost != 0.02 {
		t.Fatalf("invented or lost reported usage: input=%v output=%d model=%s provider=%s profile=%s revision=%d cost=%v", input, output, modelName, provider, profileID, revision, cost)
	}
}
