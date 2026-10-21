package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"meshra/internal/api"
	"meshra/internal/model"
	"meshra/internal/relay"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Deterministic browser fixture only. Production contains no fixture adapters.
func TestConsoleFixture(t *testing.T) {
	if os.Getenv("MESHRA_UI_FIXTURE") != "1" {
		t.Skip("browser test fixture only")
	}
	dir := os.Getenv("MESHRA_UI_TEST_DIR")
	if dir == "" {
		t.Fatal("MESHRA_UI_TEST_DIR required")
	}
	adapters := map[string]model.Adapter{}
	for _, h := range []string{"claude", "omp", "codex", "opencode"} {
		adapters[h] = model.Adapter{Quotas: func(context.Context) ([]model.QuotaWindow, error) {
			percent := 82.0
			reset := time.Now().Add(2 * time.Hour)
			observed := time.Now()
			return []model.QuotaWindow{{Provider: h, Label: "Primary window", UsedPercent: &percent, ResetsAt: &reset, ObservedAt: &observed}}, nil
		}, Models: func(context.Context) ([]model.ModelOption, error) {
			return []model.ModelOption{{ID: "fixture-model", Name: "Fixture model", Provider: map[string]string{"claude": "anthropic", "opencode": "fixture", "omp": "fixture"}[h]}}, nil
		}, Factory: func(_ context.Context, a model.Agent, _ model.Emit) (model.Runtime, error) {
			return &consoleFixtureRuntime{id: a.ID}, nil
		}}
	}
	r, err := relay.Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p := model.Principal{Admin: true}
	ids := []string{"claude:apmobileprod", "claude:apmobilewallet", "omp:adsengine", "codex:reviewer", "opencode:builder"}
	for _, id := range ids {
		parts := strings.Split(id, ":")
		a, err := r.Create(p, model.CreateAgent{Harness: parts[0], Name: parts[1], Cwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if id != "claude:apmobilewallet" {
			if _, err = r.Action(p, a.ID, "start"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, title := range []string{"Console implementation", "API contract review", "Workspace research"} {
		if _, err = r.Collaborate(p, "conversation.create", model.CollaborationRequest{Title: title, Participants: []string{ids[0], ids[2], ids[3]}}); err != nil {
			t.Fatal(err)
		}
	}
	v, err := r.Collaborate(model.Principal{AgentID: ids[0]}, "context.publish", model.CollaborationRequest{Title: "Payment API contract", Type: "contract", Content: "Keep public endpoints compatible.\n\n```go\nfunc Version() string { return \"0.4\" }\n```"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := v.(model.SharedContext)
	for _, id := range []string{ids[2], ids[3]} {
		if _, err = r.Collaborate(p, "context.grant", model.CollaborationRequest{ContextID: ctx.ID, AgentID: id}); err != nil {
			t.Fatal(err)
		}
	}
	srv := &http.Server{Addr: "127.0.0.1:37731", Handler: api.New(r), ReadHeaderTimeout: 5 * time.Second}
	defer srv.Close()
	fmt.Println("Console browser fixture on http://127.0.0.1:37731/console/")
	if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

type consoleFixtureRuntime struct {
	id string
	mu sync.Mutex
}

func (r *consoleFixtureRuntime) SessionID() string { return "fixture-" + r.id }
func (r *consoleFixtureRuntime) Close() error      { return nil }
func (r *consoleFixtureRuntime) Run(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
	emit("usage", `{"inputTokens":120,"outputTokens":40,"cachedInputTokens":80}`)
	emit("context_usage", `{"usedTokens":160,"windowTokens":1000,"estimated":false,"source":"Fixture reported snapshot"}`)
	emit("progress", "Deterministic browser-test turn; not a provider")
	delay := 150 * time.Millisecond
	if strings.Contains(turn.Task.Text, "slow") {
		delay = 1800 * time.Millisecond
		if turn.Task.Chat {
			delay = 5 * time.Second
		}
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(delay):
	}
	if strings.Contains(turn.Task.Text, "approval") {
		emit("approval_required", "Denied shell tool: no interactive approval channel")
	}
	if turn.Task.Chat {
		parts := strings.SplitN(turn.Task.Text, "\n\n", 2)
		var envelope struct {
			Message string `json:"currentUserMessage"`
			History []struct {
				User string `json:"user"`
			} `json:"recentConversation"`
		}
		if len(parts) == 2 {
			content := strings.SplitN(parts[1], "\n\nShared context follows", 2)[0]
			if err := json.Unmarshal([]byte(content), &envelope); err == nil {
				reply := "This is a deterministic test reply.\n\n" + envelope.Message
				for _, exchange := range envelope.History {
					reply += "\n\nEarlier you said: " + exchange.User
				}
				return reply + "\n\n```go\nfmt.Println(\"Meshra console\")\n```", nil
			}
		}
	}
	return "Received by **" + r.id + "**.\n\n" + strings.Split(turn.Task.Text, "\n\nShared context follows")[0] + "\n\n```go\nfmt.Println(\"Meshra console\")\n```", nil
}
