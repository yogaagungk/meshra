package relay

import (
	"context"
	"encoding/json"
	"meshra/internal/model"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func savePreferences(t *testing.T, r *Relay, update func(*model.PersonalSettings)) {
	t.Helper()
	r.mu.Lock()
	v := r.settings
	r.mu.Unlock()
	update(&v)
	if _, err := r.Console(admin, "preferences/save", model.ConsoleRequest{Settings: &v}); err != nil {
		t.Fatal(err)
	}
}
func TestPersonalSettingsPersistAndRequireAdmin(t *testing.T) {
	r, dir := openTest(t, nil)
	_, err := r.Console(model.Principal{}, "preferences", model.ConsoleRequest{})
	requireStatus(t, err, 403)
	savePreferences(t, r, func(v *model.PersonalSettings) {
		v.DisplayName = "My workspace"
		v.MaxParallelTurns = 1
		v.IdleMinutes = 5
		v.DailyTurnLimit = 20
	})
	r.mu.Lock()
	v := r.settings
	r.mu.Unlock()
	v.MaxParallelTurns = 0
	_, err = r.Console(admin, "preferences/save", model.ConsoleRequest{Settings: &v})
	requireStatus(t, err, 400)
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.settings.DisplayName != "My workspace" || next.settings.MaxParallelTurns != 1 || next.settings.DailyTurnLimit != 20 {
		t.Fatal(next.settings)
	}
}
func TestPersonalConcurrencyAndDailyBudgetSurviveRestart(t *testing.T) {
	var active, max atomic.Int32
	r, dir := openTest(t, func(ctx context.Context, _ model.Turn, emit model.Emit) (string, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := max.Load(); n > old; old = max.Load() {
			if max.CompareAndSwap(old, n) {
				break
			}
		}
		emit("usage", `{"inputTokens":42,"outputTokens":9}`)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
			return "result", nil
		}
	})
	savePreferences(t, r, func(v *model.PersonalSettings) { v.MaxParallelTurns = 1; v.DailyTurnLimit = 2 })
	a := createTest(t, r, "codex", "budgetone")
	b := createTest(t, r, "codex", "budgettwo")
	startTest(t, r, a.ID)
	startTest(t, r, b.ID)
	first, _ := r.Send(admin, model.SendRequest{From: "human:operator", To: a.ID, Text: "one"})
	second, _ := r.Send(admin, model.SendRequest{From: "human:operator", To: b.ID, Text: "two"})
	if awaitTask(t, r, first.ID).State != "completed" || awaitTask(t, r, second.ID).State != "completed" {
		t.Fatal("turn failed")
	}
	if max.Load() != 1 {
		t.Fatal("concurrency cap ignored", max.Load())
	}
	third, err := r.Send(admin, model.SendRequest{From: "human:operator", To: a.ID, Text: "three"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	current, _ := r.Task(admin, third.ID)
	if current.State != "queued" {
		t.Fatal("daily cap ignored", current.State)
	}
	r.mu.Lock()
	var tokens, rows int
	if err = r.db.QueryRow("SELECT count(*),sum(input_tokens) FROM run_usage").Scan(&rows, &tokens); err != nil {
		t.Fatal(err)
	}
	r.mu.Unlock()
	if rows != 2 || tokens != 84 {
		t.Fatal(rows, tokens)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	startTest(t, next, a.ID)
	time.Sleep(60 * time.Millisecond)
	current, _ = next.Task(admin, third.ID)
	if current.State != "queued" {
		t.Fatal("budget reset on restart")
	}
	savePreferences(t, next, func(v *model.PersonalSettings) { v.DailyTurnLimit = 3 })
	if awaitTask(t, next, third.ID).State != "completed" {
		t.Fatal("raising budget did not release queue")
	}
}
func TestStartedLimitAndIdleReapingPreservePendingAgents(t *testing.T) {
	r, _ := openTest(t, nil)
	savePreferences(t, r, func(v *model.PersonalSettings) { v.MaxStartedAgents = 1; v.IdleMinutes = 1 })
	a := createTest(t, r, "omp", "idleone")
	b := createTest(t, r, "omp", "idletwo")
	startTest(t, r, a.ID)
	_, err := r.Action(admin, b.ID, "start")
	requireStatus(t, err, 429)
	r.mu.Lock()
	r.slots[a.ID].lastUsed = now().Add(-2 * time.Minute)
	r.mu.Unlock()
	r.releaseIdle()
	current, _ := r.Agent(admin, a.ID)
	if current.State != "stopped" {
		t.Fatal(current.State)
	}
	startTest(t, r, b.ID)
	// A persistent queued dependency prevents idle reaping.
	r.mu.Lock()
	r.slots[b.ID].lastUsed = now().Add(-2 * time.Minute)
	q := model.Task{ID: randomID(), From: "human:operator", To: b.ID, State: "queued", Text: "pending", Deadline: now().Add(time.Hour)}
	body, _ := json.Marshal(q)
	_, err = r.db.Exec("INSERT INTO tasks(id,sender,sender_session,recipient,operation,state,body) VALUES(?,?,?,?,?,?,?)", q.ID, q.From, "", q.To, "request", q.State, string(body))
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r.releaseIdle()
	current, _ = r.Agent(admin, b.ID)
	if current.State == "stopped" {
		t.Fatal("pending agent reaped")
	}
}
func TestWorkspaceIsolationBoundedPreviewAndChatModel(t *testing.T) {
	calls := make(chan model.Agent, 8)
	ad := fakeAdapters(nil)
	ad["codex"] = model.Adapter{Factory: func(_ context.Context, a model.Agent, _ model.Emit) (model.Runtime, error) {
		calls <- a
		return &fakeRuntime{session: randomID()}, nil
	}}
	r, err := Open(t.TempDir(), ad)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	project := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(project, "main.go"), []byte("package main\n"), 0600)
	os.WriteFile(filepath.Join(project, "large.txt"), []byte(strings.Repeat("x", 70000)), 0600)
	os.WriteFile(filepath.Join(outside, "private.txt"), []byte("outside"), 0600)
	os.Symlink(outside, filepath.Join(project, "escape"))
	value, err := r.Console(admin, "workspaces/add", model.ConsoleRequest{Name: "Project", Path: project})
	if err != nil {
		t.Fatal(err)
	}
	w := value.(model.Workspace)
	for _, path := range []string{"../private.txt", "escape/private.txt", "/etc/passwd", ".git/config"} {
		_, err = r.Console(admin, "workspace/file", model.ConsoleRequest{WorkspaceID: w.ID, Path: path})
		if err == nil {
			t.Fatal("escaped workspace", path)
		}
	}
	value, err = r.Console(admin, "workspace/file", model.ConsoleRequest{WorkspaceID: w.ID, Path: "large.txt"})
	if err != nil {
		t.Fatal(err)
	}
	preview := value.(map[string]any)
	if !preview["truncated"].(bool) || len(preview["content"].(string)) > 65536 {
		t.Fatal("unbounded preview")
	}
	value, err = r.Console(admin, "chat-create", model.ConsoleRequest{Harness: "codex", WorkspaceID: w.ID, Config: model.Config{Model: "test-model"}})
	if err != nil {
		t.Fatal(err)
	}
	c := value.(model.Conversation)
	startTest(t, r, c.Participants[0])
	<-calls
	task, err := r.Send(admin, model.SendRequest{From: "human:operator", To: c.Participants[0], ConversationID: c.ID, Text: "review project"})
	if err != nil {
		t.Fatal(err)
	}
	awaitTask(t, r, task.ID)
	a := <-calls
	if a.Cwd != project || a.Config.Model != "test-model" || !a.Ephemeral {
		t.Fatalf("chat configuration lost: %+v", a)
	}
	_, err = r.Console(admin, "workspaces/remove", model.ConsoleRequest{ID: w.ID})
	requireStatus(t, err, 409)
}
func TestModelDiscoveryBoundsAndAuthentication(t *testing.T) {
	r, _ := openTest(t, nil)
	r.mu.Lock()
	ad := r.adapters["codex"]
	ad.Models = func(context.Context) ([]model.ModelOption, error) {
		out := []model.ModelOption{{ID: "sk-credential", Name: "hidden"}}
		for i := 0; i < 250; i++ {
			out = append(out, model.ModelOption{ID: strings.Repeat("m", i%100+1), Name: "Model"})
		}
		return out, nil
	}
	r.adapters["codex"] = ad
	r.mu.Unlock()
	_, err := r.Console(model.Principal{}, "models", model.ConsoleRequest{Harness: "codex"})
	requireStatus(t, err, 403)
	value, err := r.Console(admin, "models", model.ConsoleRequest{Harness: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	items := value.(map[string]any)["items"].([]model.ModelOption)
	if len(items) != 100 {
		t.Fatal("catalog not bounded or deduplicated", len(items))
	}
}
