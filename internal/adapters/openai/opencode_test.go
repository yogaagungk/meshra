package openai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"meshra/internal/model"
)

type sessionTransport func(*http.Request) (*http.Response, error)

func (f sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenCodeGoConversationHeaders(t *testing.T) {
	var sessions []string
	calls := 0
	client := &http.Client{Transport: sessionTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("User-Agent") != "meshra/0.4" {
			t.Errorf("user agent: %q", req.Header.Get("User-Agent"))
		}
		sessions = append(sessions, req.Header.Get("x-opencode-session"))
		status := 200
		body := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		if calls == 1 {
			status = 400
			body = `{"error":"stream unsupported"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	adapter := New(func(context.Context, string) (Config, error) {
		return Config{BaseURL: "https://opencode.ai/zen/go", HTTPClient: client}, nil
	})
	run := func(conversation string) {
		rt, err := adapter.Factory(context.Background(), testAgent(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		result, err := rt.Run(context.Background(), model.Turn{Task: model.Task{ID: "task", ConversationID: conversation, Text: "hello"}}, nil)
		if err != nil || result != "ok" {
			t.Fatalf("%q: %s %v", conversation, result, err)
		}
	}
	run("conversation-one")
	run("conversation-one")
	run("conversation-two")
	if len(sessions) != 4 || sessions[0] == "" || sessions[0] != sessions[1] || sessions[1] != sessions[2] || sessions[2] == sessions[3] || strings.Contains(sessions[0], "conversation") {
		t.Fatalf("incorrect routing sessions: %v", sessions)
	}
	req, _ := http.NewRequest("POST", "https://other.example/zen/go/v1/chat/completions", nil)
	(&runtime{providerSession: "secret-id"}).setProviderHeaders(req)
	if req.Header.Get("x-opencode-session") != "" {
		t.Fatal("session leaked to unrelated origin")
	}
}
