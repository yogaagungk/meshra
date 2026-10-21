package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"meshra/internal/model"
)

type fakeServer struct {
	t        *testing.T
	http     *httptest.Server
	mu       sync.Mutex
	sessions map[string]string
	policies map[string][]map[string]string
	patches  int
	created  int
	prompts  int
	starts   atomic.Int32
	stops    atomic.Int32
	aborted  chan struct{}
	waiting  chan struct{}
	asks     chan string
	denied   chan struct{}
	tokens   []string
	fail     bool
	mismatch bool
}

func newFake(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, sessions: map[string]string{}, aborted: make(chan struct{}, 10), waiting: make(chan struct{}, 10), asks: make(chan string, 10), denied: make(chan struct{}, 10)}
	f.policies = make(map[string][]map[string]string)
	f.http = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.http.Close)
	return f
}
func (f *fakeServer) adapter() model.Adapter {
	return adapter(func() (starter, error) {
		return func(ctx context.Context, a model.Agent, turn model.Turn) (*server, error) {
			f.starts.Add(1)
			f.mu.Lock()
			f.tokens = append(f.tokens, a.ID+":"+turn.Token)
			f.mu.Unlock()
			return &server{base: f.http.URL, password: "local-password", client: f.http.Client(), cwd: a.Cwd, stop: func() { f.stops.Add(1) }}, nil
		}, nil
	})
}
func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	user, password, ok := r.BasicAuth()
	if !ok || user != "opencode" || password != "local-password" {
		f.t.Error("missing scoped authentication")
		w.WriteHeader(401)
		return
	}
	if r.Header.Get("x-opencode-directory") == "" {
		f.t.Error("missing directory scope")
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/config/providers":
		fmt.Fprint(w, `{"providers":[{"id":"test-provider","apiKey":"never expose this","models":{"test-model":{"id":"test-model","name":"Test model"}}}]}`)
	case r.Method == "POST" && r.URL.Path == "/session":
		var body struct {
			Permission []map[string]string `json:"permission"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Error(err)
		}
		if len(body.Permission) == 0 || body.Permission[0]["action"] != "ask" {
			f.t.Error("unsafe session permissions")
		}
		f.mu.Lock()
		f.created++
		id := fmt.Sprintf("ses_%d", f.created)
		f.sessions[id] = r.Header.Get("x-opencode-directory")
		f.policies[id] = body.Permission
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "directory": r.Header.Get("x-opencode-directory"), "permission": body.Permission})
	case r.URL.Path == "/event":
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case id := <-f.asks:
				fmt.Fprintf(w, "data: {\"type\":\"permission.asked\",\"properties\":{\"id\":\"perm_1\",\"sessionID\":%q,\"permission\":\"bash\"}}\n\n", id)
				w.(http.Flusher).Flush()
			}
		}
	case strings.HasSuffix(r.URL.Path, "/abort"):
		f.aborted <- struct{}{}
		fmt.Fprint(w, "true")
	case r.URL.Path == "/permission/perm_1/reply":
		var body struct {
			Reply string `json:"reply"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Reply != "reject" {
			f.t.Error("unsafe permission reply")
		}
		f.denied <- struct{}{}
		fmt.Fprint(w, "true")
	case strings.HasSuffix(r.URL.Path, "/message"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/session/"), "/message")
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Error(err)
		}
		if len(body.Parts) != 1 {
			f.t.Error("prompt parts missing")
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.prompts++
		fail, mismatch := f.fail, f.mismatch
		f.mu.Unlock()
		switch body.Parts[0].Text {
		case "wait":
			f.waiting <- struct{}{}
			<-r.Context().Done()
			return
		case "denied":
			f.asks <- id
			select {
			case <-f.denied:
			case <-r.Context().Done():
				return
			}
		}
		if fail {
			w.WriteHeader(502)
			fmt.Fprint(w, "secret upstream detail")
			return
		}
		if mismatch {
			id = "another-session"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"info": map[string]string{"sessionID": id}, "parts": []map[string]string{{"type": "text", "text": id + ": " + body.Parts[0].Text}}})
	case strings.HasPrefix(r.URL.Path, "/session/"):
		id := strings.TrimPrefix(r.URL.Path, "/session/")
		f.mu.Lock()
		cwd, ok := f.sessions[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		if r.Method == "PATCH" {
			var body struct {
				Permission []map[string]string `json:"permission"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Permission) == 0 || body.Permission[0]["action"] != "ask" {
				f.t.Error("resume did not reset safe permission rules")
			}
			f.mu.Lock()
			f.policies[id] = append(f.policies[id], body.Permission...)
			f.patches++
			f.mu.Unlock()
		}
		f.mu.Lock()
		policy := append([]map[string]string(nil), f.policies[id]...)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "directory": cwd, "permission": policy})
	default:
		w.WriteHeader(404)
	}
}
func TestSessionOwnershipResumePermissions(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	one, err := a.Factory(context.Background(), model.Agent{ID: "opencode:one", Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := a.Factory(context.Background(), model.Agent{ID: "opencode:two", Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if f.starts.Load() != 0 || one.SessionID() != "" {
		t.Fatal("factory launched a turn or fabricated a session")
	}
	var events []string
	answer, err := one.Run(context.Background(), model.Turn{Task: model.Task{Text: "denied"}, MCPURL: "http://127.0.0.1/mcp", Token: "first"}, func(kind, data string) { events = append(events, kind) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(answer, one.SessionID()+":") || len(events) != 2 || events[0] != "session" || events[1] != "approval_required" {
		t.Fatalf("wrong sequence %q %v", answer, events)
	}
	id := one.SessionID()
	if _, err = one.Run(context.Background(), model.Turn{Task: model.Task{Text: "again"}, Token: "second"}, nil); err != nil {
		t.Fatal(err)
	}
	if one.SessionID() != id {
		t.Fatal("session changed on resume")
	}
	if _, err = two.Run(context.Background(), model.Turn{Task: model.Task{Text: "other"}, Token: "third"}, nil); err != nil {
		t.Fatal(err)
	}
	if two.SessionID() == id {
		t.Fatal("session isolation failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created != 2 || f.prompts != 3 || len(f.tokens) != 3 || f.tokens[0] != "opencode:one:first" || f.tokens[1] != "opencode:one:second" || f.tokens[2] != "opencode:two:third" {
		t.Fatalf("bad ownership %#v", f.tokens)
	}
	if f.starts.Load() != 3 || f.stops.Load() != 3 {
		t.Fatal("server lifecycle leak")
	}
}

func TestResumeRestoresPermissionsWithoutRepeatedAppend(t *testing.T) {
	f := newFake(t)
	cwd := t.TempDir()
	f.sessions["saved"] = cwd
	f.policies["saved"] = []map[string]string{{"permission": "*", "pattern": "*", "action": "allow"}}
	r, err := f.adapter().Factory(context.Background(), model.Agent{Cwd: cwd, SessionID: "saved"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for range 3 {
		if _, err = r.Run(context.Background(), model.Turn{Task: model.Task{Text: "resumed"}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patches != 1 || len(f.policies["saved"]) != 1+len(permissions()) || !hasPermissionTail(f.policies["saved"], permissions()) {
		t.Fatalf("resume must restore safe policy once, not append each turn: patches=%d rules=%d", f.patches, len(f.policies["saved"]))
	}
}
func TestResumeOnlyOwnedSession(t *testing.T) {
	f := newFake(t)
	r, err := f.adapter().Factory(context.Background(), model.Agent{Cwd: t.TempDir(), SessionID: "missing-owned-session"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = r.Run(context.Background(), model.Turn{}, nil); err == nil {
		t.Fatal("missing owned session silently replaced")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created != 0 || f.prompts != 0 {
		t.Fatal("resume allocated or prompted another session")
	}
}
func TestPromptFailures(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			f := newFake(t)
			f.fail = !mismatch
			f.mismatch = mismatch
			r, err := f.adapter().Factory(context.Background(), model.Agent{Cwd: t.TempDir()}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			_, err = r.Run(context.Background(), model.Turn{Task: model.Task{Text: "failure"}}, nil)
			if err == nil || strings.Contains(err.Error(), "secret upstream") {
				t.Fatalf("expected sanitized failure: %v", err)
			}
			if f.stops.Load() != 1 {
				t.Fatal("failed turn leaked server")
			}
		})
	}
}
func TestCancelAbortsNativeSession(t *testing.T) {
	f := newFake(t)
	r, err := f.adapter().Factory(context.Background(), model.Agent{Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.Run(ctx, model.Turn{Task: model.Task{Text: "wait"}}, nil); done <- err }()
	select {
	case <-f.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not start")
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not finish")
	}
	select {
	case <-f.aborted:
	case <-time.After(time.Second):
		t.Fatal("native abort not sent")
	}
	if f.stops.Load() != 1 {
		t.Fatal("cancelled server not stopped")
	}
}
func TestEnvironmentOverridesDoNotDuplicateSecrets(t *testing.T) {
	got := environment([]string{"KEEP=value", "OPENCODE_SERVER_PASSWORD=old", "OPENCODE_CONFIG_CONTENT=old"}, map[string]string{"OPENCODE_SERVER_PASSWORD": "new", "OPENCODE_CONFIG_CONTENT": "new"})
	if len(got) != 3 {
		t.Fatal(got)
	}
	for _, value := range got {
		if strings.HasSuffix(value, "=old") {
			t.Fatal("inherited secret/config survived override")
		}
	}
}
func TestAuthenticatedSmoke(t *testing.T) {
	if os.Getenv("MESHRA_AUTH_SMOKE") != "1" {
		t.Skip("opt-in paid OpenCode smoke; see docs/adapters-sdk.md")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, err := New().Factory(ctx, model.Agent{ID: "opencode:smoke", Cwd: t.TempDir(), Config: model.Config{Provider: os.Getenv("MESHRA_OPENCODE_PROVIDER"), Model: os.Getenv("MESHRA_OPENCODE_MODEL")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for range 2 {
		text, err := r.Run(ctx, model.Turn{Task: model.Task{Text: "Reply exactly MESHRA_SMOKE_OK. Do not use tools."}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "MESHRA_SMOKE_OK") || r.SessionID() == "" {
			t.Fatalf("unexpected smoke result %q", text)
		}
	}
}

func TestCloseCancelsTurnAndAborts(t *testing.T) {
	f := newFake(t)
	r, err := f.adapter().Factory(context.Background(), model.Agent{Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), model.Turn{Task: model.Task{Text: "wait"}}, nil)
		done <- err
	}()
	select {
	case <-f.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not start")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not finish")
	}
	select {
	case <-f.aborted:
	case <-time.After(time.Second):
		t.Fatal("Close did not abort native session")
	}
	if _, err = r.Run(context.Background(), model.Turn{}, nil); err == nil {
		t.Fatal("closed runtime accepted a prompt")
	}
	if f.stops.Load() != 1 {
		t.Fatal("Close leaked owned server")
	}
}

func TestResumeRejectsDifferentDirectory(t *testing.T) {
	f := newFake(t)
	f.sessions["saved"] = "another-working-directory"
	r, err := f.adapter().Factory(context.Background(), model.Agent{Cwd: t.TempDir(), SessionID: "saved"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = r.Run(context.Background(), model.Turn{}, nil); err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("cross-directory resume: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created != 0 || f.prompts != 0 {
		t.Fatal("directory mismatch performed work")
	}
}

func TestNativeModelCatalogProtocol(t *testing.T) {
	f := newFake(t)
	rt, err := f.adapter().Factory(context.Background(), model.Agent{ID: "opencode:catalog", Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	items, err := rt.(model.ModelCatalog).Models(context.Background())
	if err != nil || len(items) != 1 || items[0].Provider != "test-provider" || items[0].ID != "test-model" {
		t.Fatal(items, err)
	}
	if f.created != 0 || f.prompts != 0 {
		t.Fatal("catalog discovery created a generation")
	}
}
