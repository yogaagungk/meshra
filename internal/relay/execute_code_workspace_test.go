package relay

import (
	"context"
	"sync"
	"testing"

	"meshra/internal/model"
	"meshra/internal/sandbox"
)

func TestConcurrentBrokerWorkspaceInitializationIsSingleSnapshot(t *testing.T) {
	r, _, workspace, _ := changesetFixture(t)
	b := &toolBroker{relay: r, snapshot: model.ProfileTaskSnapshot{Profile: model.Profile{WorkspaceID: workspace.ID}}}
	const workers = 12
	results := make(chan *sandbox.Snapshot, workers)
	errors := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := b.ensureWorkspace(context.Background())
			results <- got
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *sandbox.Snapshot
	for got := range results {
		if first == nil {
			first = got
		} else if got != first {
			t.Fatal("concurrent actions received different snapshots")
		}
	}
	if first == nil {
		t.Fatal("missing snapshot")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodeClaimRejectsChangedOrUnreservedWorkspace(t *testing.T) {
	r, _, workspace, _ := changesetFixture(t)
	snapshot, err := r.prepareWorkspace(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	manifest, err := snapshot.Changes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	baseline := manifest.Hash
	manifest.Close()
	state := &brokerCall{invocation: toolInvocation{ID: "code-reservation"}}
	b := &toolBroker{relay: r, workspace: snapshot, mutation: state.invocation.ID}
	q := &preparedExecuteCode{workspace: snapshot, baseline: baseline}
	changed, err := snapshot.Stage("new-file", []byte("changed after approval"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	changed.Close()
	if err := b.claimExecuteCode(context.Background(), state, q); brokerStatus(err) != 409 {
		t.Fatalf("changed baseline accepted: %v", err)
	}
	b.mutation = "other"
	if err := b.claimExecuteCode(context.Background(), state, q); brokerStatus(err) != 409 {
		t.Fatalf("missing reservation accepted: %v", err)
	}
	b.mutation = state.invocation.ID
	b.workspace = nil
	if err := b.claimExecuteCode(context.Background(), state, q); brokerStatus(err) != 409 {
		t.Fatalf("different snapshot accepted: %v", err)
	}
}
