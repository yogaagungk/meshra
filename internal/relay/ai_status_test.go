package relay

import (
	"context"
	"encoding/json"
	"errors"
	"meshra/internal/model"
	"strings"
	"testing"
	"time"
)

func TestQuotaCacheCooldownStaleAndAdminBoundary(t *testing.T) {
	r, _ := openTest(t, nil)
	calls := 0
	fail := false
	percent := 81.0
	r.mu.Lock()
	ad := r.adapters["codex"]
	ad.Quotas = func(context.Context) ([]model.QuotaWindow, error) {
		calls++
		if fail {
			return nil, errors.New("private upstream detail")
		}
		return []model.QuotaWindow{{ID: "secret-id", Provider: "codex", Label: "Primary window", UsedPercent: &percent}}, nil
	}
	r.adapters["codex"] = ad
	r.mu.Unlock()
	_, err := r.Console(model.Principal{}, "quota", model.ConsoleRequest{Harness: "codex"})
	requireStatus(t, err, 403)
	for i := 0; i < 3; i++ {
		v, e := r.Console(admin, "quota", model.ConsoleRequest{Harness: "codex", Refresh: true})
		if e != nil {
			t.Fatal(e)
		}
		s := v.(model.QuotaSnapshot)
		if s.State != "reported" || len(s.Windows) != 1 || s.Windows[0].ID == "secret-id" {
			t.Fatal(s)
		}
	}
	if calls != 1 {
		t.Fatal("refresh cooldown ignored", calls)
	}
	r.mu.Lock()
	v := r.quotaCache["codex"]
	old := now().Add(-time.Minute * 2)
	v.AttemptedAt = &old
	v.CheckedAt = &old
	r.quotaCache["codex"] = v
	r.mu.Unlock()
	fail = true
	value, err := r.Console(admin, "quota", model.ConsoleRequest{Harness: "codex", Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	v = value.(model.QuotaSnapshot)
	if !v.Stale || len(v.Windows) != 1 || v.State != "unavailable" || strings.Contains(v.Message, "private") {
		t.Fatal(v)
	}
	value, err = r.Console(admin, "quota", model.ConsoleRequest{Harness: "claude"})
	if err != nil || value.(model.QuotaSnapshot).State != "not_supported" {
		t.Fatal(value, err)
	}
	_, err = r.Console(admin, "quota", model.ConsoleRequest{Harness: "arbitrary"})
	requireStatus(t, err, 400)
}
func TestQuotaRefreshSerializedWithoutHoldingRelayMutex(t *testing.T) {
	r, _ := openTest(t, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	r.mu.Lock()
	a := r.adapters["codex"]
	a.Quotas = func(ctx context.Context) ([]model.QuotaWindow, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	}
	r.adapters["codex"] = a
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.Console(admin, "quota", model.ConsoleRequest{Harness: "codex"}) }()
	<-entered
	if _, err := r.Agents(admin); err != nil {
		t.Fatal(err)
	}
	_, err := r.Console(admin, "quota", model.ConsoleRequest{Harness: "omp"})
	requireStatus(t, err, 409)
	r.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh ignored shutdown")
	}
}
func TestQuotaSanitizationAndBounds(t *testing.T) {
	p := 55.0
	items := []model.QuotaWindow{}
	for i := 0; i < 100; i++ {
		items = append(items, model.QuotaWindow{ID: "secret-account", Provider: "openai", Account: "Account user@private", Label: "user@private", UsedPercent: &p, Unit: "secret-unit"})
	}
	clean := sanitizeQuota(items)
	if len(clean) != 64 {
		t.Fatal(len(clean))
	}
	b, _ := json.Marshal(clean)
	for _, secret := range []string{"secret-account", "user@private", "secret-unit"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("quota metadata leaked", secret)
		}
	}
}
func TestContextUsageAndHistoryAreScopedAndRetained(t *testing.T) {
	r, _ := openTest(t, func(_ context.Context, _ model.Turn, emit model.Emit) (string, error) {
		emit("usage", `{"inputTokens":120,"outputTokens":40,"cachedInputTokens":80}`)
		emit("context_usage", `{"usedTokens":160,"windowTokens":1000,"estimated":true,"source":"fixture native estimate"}`)
		return "reply", nil
	})
	ai := createTest(t, r, "codex", "contextstatus")
	startTest(t, r, ai.ID)
	c := createChat(t, r, ai.ID)
	task, err := r.Send(admin, model.SendRequest{From: "human:operator", To: ai.ID, ConversationID: c.ID, Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	awaitTask(t, r, task.ID)
	value, err := r.Console(admin, "ai-status", model.ConsoleRequest{ConversationID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(value)
	var body struct {
		Latest struct {
			Used      *int64 `json:"contextUsedTokens"`
			Window    *int64 `json:"contextWindowTokens"`
			Cached    *int64 `json:"cachedInputTokens"`
			Estimated *bool  `json:"estimated"`
		} `json:"latest"`
	}
	if err = json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if body.Latest.Used == nil || *body.Latest.Used != 160 || body.Latest.Window == nil || *body.Latest.Window != 1000 || body.Latest.Cached == nil || *body.Latest.Cached != 80 || body.Latest.Estimated == nil || !*body.Latest.Estimated {
		t.Fatal(string(b))
	}
	value, err = r.Console(admin, "usage-history", model.ConsoleRequest{ConversationID: c.ID, Harness: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(value)
	if !strings.Contains(string(b), `"cachedReportedTurns":1`) {
		t.Fatal(string(b))
	}
	other := createChat(t, r, ai.ID)
	value, err = r.Console(admin, "usage-history", model.ConsoleRequest{ConversationID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(value)
	if !strings.Contains(string(b), `"items":[]`) {
		t.Fatal("conversation usage leaked", string(b))
	}
	_, err = r.Console(model.Principal{}, "ai-status", model.ConsoleRequest{ConversationID: c.ID})
	requireStatus(t, err, 403)
}
