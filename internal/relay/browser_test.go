package relay

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/model"
)

// fakeBrowserWorker stands in for the confined browser process. It records the
// exact actions it received so the ordering rules can be checked without a
// browser, a display server or a network.
type fakeBrowserWorker struct {
	mu      sync.Mutex
	actions []browserWorkerAction
	result  browserResult
	err     error
	closed  bool
}

func (f *fakeBrowserWorker) Execute(ctx context.Context, action browserWorkerAction) (browserResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action)
	return f.result, f.err
}

func (f *fakeBrowserWorker) Frame(ctx context.Context) ([]byte, error) {
	return []byte{0x89, 0x50, 0x4e, 0x47}, nil
}

func (f *fakeBrowserWorker) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeBrowserWorker) seen() []browserWorkerAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]browserWorkerAction{}, f.actions...)
}

func browserTestConversation(t *testing.T, r *Relay) string {
	t.Helper()
	id := randomID()
	if _, err := r.db.Exec("INSERT INTO conversations VALUES(?,?,?,?)", id, "Browser test", "admin", now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	return id
}

// automationTestSchema applies the owned migration when the daemon has not
// embedded it yet. The recorded schema version is left untouched.
func automationTestSchema(t *testing.T, r *Relay, file string) {
	t.Helper()
	var count int
	if err := r.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='browser_sessions'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count > 0 {
		return
	}
	body, err := os.ReadFile(filepath.Join("migrations", file))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "UPDATE schema_version") {
			continue
		}
		statements = append(statements, line)
	}
	if _, err = r.db.Exec(strings.Join(statements, "\n")); err != nil {
		t.Fatal(err)
	}
}

// browserTestProfile creates a profile and enables only the browser capability
// in the stored body. The profile store itself still refuses the capability
// until the browser backend is configured.
func browserTestProfile(t *testing.T, r *Relay) model.Profile {
	t.Helper()
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	r.mu.Lock()
	defer r.mu.Unlock()
	var body string
	if err := r.db.QueryRow("SELECT body FROM agent_profiles WHERE id=?", profile.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var stored model.Profile
	if err := json.Unmarshal([]byte(body), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Capabilities.Browser = true
	updated, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.db.Exec("UPDATE agent_profiles SET body=? WHERE id=?", string(updated), profile.ID); err != nil {
		t.Fatal(err)
	}
	return stored
}

func browserTestSession(t *testing.T, r *Relay, profileID, conversationID string) (*browserRuntime, *fakeBrowserWorker) {
	t.Helper()
	session := browserSession{ID: randomID(), ProfileID: profileID, ConversationID: conversationID, State: browserStateActive, Epoch: 1, CreatedAt: now(), UpdatedAt: now()}
	if err := r.storeBrowserSession(session); err != nil {
		t.Fatal(err)
	}
	worker := &fakeBrowserWorker{result: browserResult{URL: "http://127.0.0.1:9/", Title: "Fixture", Text: "fixture page"}}
	runtime := &browserRuntime{record: session, worker: worker, pending: map[string]bool{}}
	r.browserManager().put(runtime)
	return runtime, worker
}

func browserCall(t *testing.T, r *Relay, p model.Principal, op string, q any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return r.browserOperation(context.Background(), p, op, raw)
}

func TestBrowserActionValidationRejectsUnsafeInput(t *testing.T) {
	cases := []struct {
		name   string
		action browserAction
		status int
	}{
		{"unknown action", browserAction{Action: "evaluate"}, 400},
		{"click without selector", browserAction{Action: "click"}, 400},
		{"type without text", browserAction{Action: "type", Selector: "#a"}, 400},
		{"oversized text", browserAction{Action: "type", Selector: "#a", Text: strings.Repeat("x", browserMaxText+1)}, 400},
		{"javascript url", browserAction{Action: "navigate", URL: "javascript:alert(1)"}, 400},
		{"url credentials", browserAction{Action: "navigate", URL: "http://user:pass@example.com/"}, 400},
		{"oversized url", browserAction{Action: "navigate", URL: "http://example.com/" + strings.Repeat("a", browserMaxURL)}, 413},
		{"upload without attachment", browserAction{Action: "upload", Selector: "#file"}, 400},
		{"invalid scroll direction", browserAction{Action: "scroll", Direction: "sideways"}, 400},
		{"invalid tab", browserAction{Action: "inspect", Tab: browserMaxTabs}, 400},
		{"key without value", browserAction{Action: "key"}, 400},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := browserValidateAction(test.action)
			if err == nil {
				t.Fatal("unsafe browser action was accepted")
			}
			status := 0
			var typed *model.Error
			if errors.As(err, &typed) {
				status = typed.Status
			}
			if status != test.status {
				t.Fatalf("unexpected status %d (%v) for %+v", status, err, test.action)
			}
		})
	}
}

func TestBrowserOpenReportsMissingRuntimePrerequisites(t *testing.T) {
	r, _ := openTest(t, nil)
	automationTestSchema(t, r, "018.sql")
	profile := browserTestProfile(t, r)
	conversation := browserTestConversation(t, r)
	_, err := browserCall(t, r, admin, "browser.open", map[string]any{"profileId": profile.ID, "conversationId": conversation})
	if err == nil {
		t.Fatal("browser session started without a configured confined runtime")
	}
	status := 0
	var typed *model.Error
	if errors.As(err, &typed) {
		status = typed.Status
	}
	if status != 503 {
		t.Fatalf("unexpected status %d (%v)", status, err)
	}
	if !strings.Contains(err.Error(), "bubblewrap") && !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("missing runtime prerequisite is not actionable: %v", err)
	}
}

func TestBrowserEpochAndLeaseOrdering(t *testing.T) {
	r, _ := openTest(t, nil)
	automationTestSchema(t, r, "018.sql")
	r.SetConsoleSessionValidator(func(id string) bool { return id == "browser-session" })
	console := model.Principal{Admin: true, ConsoleSessionID: "browser-session"}
	profile := browserTestProfile(t, r)
	conversation := browserTestConversation(t, r)
	runtime, worker := browserTestSession(t, r, profile.ID, conversation)
	session := runtime.snapshot()


	// The human takeover advances the epoch and blocks model input.
	taken, err := browserCall(t, r, console, "browser.takeover", map[string]any{"id": session.ID, "expectedEpoch": session.Epoch})
	if err != nil {
		t.Fatal(err)
	}
	takenSession := taken.(browserSession)
	if takenSession.Epoch != session.Epoch+1 || takenSession.State != browserStateTakingOver || runtime.snapshot().LeaseOwner != console.ConsoleSessionID {
		t.Fatalf("takeover did not advance the epoch or record the lease: %+v", takenSession)
	}
	if takenSession.LeaseExpiresAt == nil || !takenSession.LeaseExpiresAt.After(now()) || takenSession.LeaseExpiresAt.After(now().Add(browserLeaseDuration+time.Second)) {
		t.Fatalf("takeover lease is not a bounded session lease: %+v", takenSession.LeaseExpiresAt)
	}
	seenBefore := len(worker.seen())
	// browser.input is a human-only API: a caller without a console session
	// cannot drive the browser, takeover or not.
	_, err = browserCall(t, r, model.Principal{}, "browser.input", map[string]any{"id": session.ID, "epoch": takenSession.Epoch, "frameSeq": takenSession.FrameSeq, "action": map[string]any{"action": "inspect"}})
	if err == nil {
		t.Fatal("a caller without a console session drove the browser")
	}
	// A stale epoch is refused before the worker sees anything.
	_, err = browserCall(t, r, console, "browser.input", map[string]any{"id": session.ID, "epoch": takenSession.Epoch + 5, "frameSeq": takenSession.FrameSeq, "action": map[string]any{"action": "inspect"}})
	if err == nil || !strings.Contains(err.Error(), "stale browser epoch") {
		t.Fatalf("stale epoch was not refused: %v", err)
	}
	// A stale frame is refused as well.
	_, err = browserCall(t, r, console, "browser.input", map[string]any{"id": session.ID, "epoch": takenSession.Epoch, "frameSeq": takenSession.FrameSeq + 3, "action": map[string]any{"action": "inspect"}})
	if err == nil || !strings.Contains(err.Error(), "stale browser frame") {
		t.Fatalf("stale frame was not refused: %v", err)
	}
	if len(worker.seen()) != seenBefore {
		t.Fatal("a refused action reached the browser worker")
	}
	// The human may act with the current epoch.
	if _, err = browserCall(t, r, console, "browser.input", map[string]any{"id": session.ID, "epoch": takenSession.Epoch, "frameSeq": takenSession.FrameSeq, "action": map[string]any{"action": "inspect"}}); err != nil {
		t.Fatalf("human input during the takeover failed: %v", err)
	}
	// A takeover from another console session is denied.
	other := model.Principal{Admin: true, ConsoleSessionID: "other-session"}
	r.SetConsoleSessionValidator(func(id string) bool { return id == "browser-session" || id == "other-session" })
	_, err = browserCall(t, r, other, "browser.input", map[string]any{"id": session.ID, "epoch": takenSession.Epoch, "frameSeq": takenSession.FrameSeq, "action": map[string]any{"action": "inspect"}})
	if err == nil || !strings.Contains(err.Error(), "another console session") {
		t.Fatalf("another console session could drive the takeover: %v", err)
	}

	// The release refreshes the snapshot, advances the epoch and returns
	// control to the model.
	released, err := browserCall(t, r, console, "browser.release", map[string]any{"id": session.ID, "expectedEpoch": takenSession.Epoch})
	if err != nil {
		t.Fatal(err)
	}
	releasedSession := released.(browserSession)
	if releasedSession.Epoch != takenSession.Epoch+1 || releasedSession.State != browserStateActive || releasedSession.LeaseOwner != "" {
		t.Fatalf("release did not return control to the model: %+v", releasedSession)
	}
	if releasedSession.SnapshotText == "" {
		t.Fatal("release did not refresh the visible snapshot")
	}
	// Human input is refused once the takeover is released.
	_, err = browserCall(t, r, console, "browser.input", map[string]any{"id": session.ID, "epoch": releasedSession.Epoch, "frameSeq": releasedSession.FrameSeq, "action": map[string]any{"action": "inspect"}})
	if err == nil {
		t.Fatal("human input was accepted after the release")
	}
}

func TestBrowserTakeoverInvalidatesPendingApproval(t *testing.T) {
	r, _ := openTest(t, nil)
	automationTestSchema(t, r, "018.sql")
	r.SetConsoleSessionValidator(func(id string) bool { return id == "browser-session" })
	console := model.Principal{Admin: true, ConsoleSessionID: "browser-session"}
	profile := browserTestProfile(t, r)
	conversation := browserTestConversation(t, r)
	runtime, _ := browserTestSession(t, r, profile.ID, conversation)
	session := runtime.snapshot()

	// A pending browser approval is registered with the session.
	invocation := randomID()
	taskID := randomID()
	r.mu.Lock()
	_, err := r.db.Exec("INSERT INTO tasks(id,sender,sender_session,recipient,operation,state,body) VALUES(?,?,?,?,?,?,?)", taskID, "admin", "session", "codex:fixture", "request", "running", "{}")
	if err == nil {
		_, err = r.db.Exec("INSERT INTO tool_invocations(id,task_id,attempt,call_id,action_hash,state,body) VALUES(?,?,?,?,?,?,?)", invocation, taskID, 1, "call-1", "hash", "pending", `{"name":"meshra_browser"}`)
	}
	if err == nil {
		_, err = r.db.Exec("INSERT INTO tool_approvals(id,invocation_id,revision,status,expires_at,body) VALUES(?,?,1,'pending',?,?)", randomID(), invocation, now().Add(time.Minute).Format(time.RFC3339Nano), `{}`)
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = r.browserRegisterInvocation(session.ID, invocation, session.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err = browserCall(t, r, console, "browser.takeover", map[string]any{"id": session.ID, "expectedEpoch": session.Epoch}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	var status string
	if err = r.db.QueryRow("SELECT status FROM tool_approvals WHERE invocation_id=?", invocation).Scan(&status); err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	r.mu.Unlock()
	if status != "invalidated" {
		t.Fatalf("the pending approval survived the takeover: %s", status)
	}
}

func TestBrowserClearDataRequiresConfirmation(t *testing.T) {
	r, _ := openTest(t, nil)
	automationTestSchema(t, r, "018.sql")
	profile := browserTestProfile(t, r)
	conversation := browserTestConversation(t, r)
	_, err := browserCall(t, r, admin, "browser.clearData", map[string]any{"profileId": profile.ID, "conversationId": conversation})
	if err == nil || !strings.Contains(err.Error(), "confirm") {
		t.Fatalf("browser data was cleared without an explicit confirmation: %v", err)
	}
}
