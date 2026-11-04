package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"meshra/internal/model"
)

func TestAnthropicCacheEncoding(t *testing.T) {
	tool := toolDefinition{}
	tool.Function.Name = "lookup"
	tool.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}}`)
	req := chatRequest{Model: "claude-sonnet-4-5", Tools: []toolDefinition{tool, tool}, Messages: []chatMessage{{Role: "system", Content: strings.Repeat("instructions ", 400)}, {Role: "user", Content: "question"}}}
	for _, mode := range []string{"auto", "1h", "off"} {
		body, err := encodeAnthropicRequestWithCaching(req, mode)
		if err != nil {
			t.Fatal(err)
		}
		again, err := encodeAnthropicRequestWithCaching(req, mode)
		if err != nil || !bytes.Equal(body, again) {
			t.Fatalf("non-deterministic encoding: %v", err)
		}
		count := bytes.Count(body, []byte(`"cache_control"`))
		if mode == "off" && count != 0 || mode != "off" && count != 3 {
			t.Fatalf("%s markers=%d: %s", mode, count, body)
		}
		if mode == "1h" && bytes.Count(body, []byte(`"ttl":"1h"`)) != 3 {
			t.Fatalf("missing ttl: %s", body)
		}
		if mode == "auto" && bytes.Contains(body, []byte(`"ttl"`)) {
			t.Fatalf("unexpected ttl: %s", body)
		}
		var decoded struct {
			Tools []struct {
				Cache json.RawMessage `json:"cache_control"`
			} `json:"tools"`
			System   []json.RawMessage `json:"system"`
			Messages []struct {
				Content []json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if mode != "off" {
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Tools[0].Cache) != 0 || len(decoded.Tools[1].Cache) == 0 || !bytes.Contains(decoded.System[0], []byte(`"cache_control"`)) || !bytes.Contains(decoded.Messages[0].Content[0], []byte(`"cache_control"`)) {
				t.Fatalf("incorrect breakpoints: %s", body)
			}
		}
	}
	small, err := encodeAnthropicRequestWithCaching(chatRequest{Model: req.Model, Messages: []chatMessage{{Role: "user", Content: "short"}}}, "auto")
	if err != nil || bytes.Contains(small, []byte(`"cache_control"`)) {
		t.Fatalf("short prompt cached: %s %v", small, err)
	}
}

func TestCacheEncodingBaseline(t *testing.T) {
	catalog := []model.ToolDefinition{
		{Name: "zeta", Description: "first", InputSchema: json.RawMessage(`{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}}`)},
		{Name: "alpha", Description: "second", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
	}
	request := chatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []chatMessage{
			{Role: "system", Content: strings.Repeat("stable instructions ", 300)},
			{Role: "user", Content: "question"},
		},
		Tools: definitions(catalog), ToolChoice: "auto",
	}
	for _, tc := range []struct {
		name   string
		encode func(chatRequest) ([]byte, error)
	}{
		{"openai", func(req chatRequest) ([]byte, error) { return json.Marshal(req) }},
		{"anthropic", func(req chatRequest) ([]byte, error) { return encodeAnthropicRequestWithCaching(req, "auto") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, err := tc.encode(request)
			if err != nil {
				t.Fatal(err)
			}
			second, err := tc.encode(request)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatalf("request encoding changed:\n%s\n%s", first, second)
			}
			if bytes.Index(first, []byte(`"zeta"`)) >= bytes.Index(first, []byte(`"alpha"`)) {
				t.Fatalf("tool ordering changed: %s", first)
			}
		})
	}
}

func TestAnthropicCacheBreakpointMoves(t *testing.T) {
	request := chatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []chatMessage{
			{Role: "system", Content: strings.Repeat("instructions ", 400)},
			{Role: "user", Content: "first question"},
		},
	}
	first, err := encodeAnthropicRequestWithCaching(request, "auto")
	if err != nil {
		t.Fatal(err)
	}
	request.Messages = append(request.Messages,
		chatMessage{Role: "assistant", Content: "first answer"},
		chatMessage{Role: "user", Content: "second question"},
	)
	second, err := encodeAnthropicRequestWithCaching(request, "auto")
	if err != nil {
		t.Fatal(err)
	}
	var previous, next anthropicRequest
	if err := json.Unmarshal(first, &previous); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &next); err != nil {
		t.Fatal(err)
	}
	previousMessages, _ := json.Marshal(previous.Messages)
	nextMessages, _ := json.Marshal(next.Messages)
	if bytes.Count(previousMessages, []byte(`"cache_control"`)) != 1 ||
		bytes.Count(nextMessages, []byte(`"cache_control"`)) != 1 {
		t.Fatalf("expected one message breakpoint: %s / %s", previousMessages, nextMessages)
	}
	for i, message := range next.Messages {
		content, _ := json.Marshal(message.Content)
		if bytes.Contains(content, []byte(`"cache_control"`)) != (i == len(next.Messages)-1) {
			t.Fatalf("breakpoint did not move to final message: %s", nextMessages)
		}
	}
}

func TestAnthropicCacheUsage(t *testing.T) {
	reply, err := decodeAnthropicResponse([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}`))
	if err != nil {
		t.Fatal(err)
	}
	var totals usageTotals
	totals.add(reply.Usage)
	input, output, read := totals.aggregate()
	write := totals.cacheWrites()
	if *input != 60 || *output != 5 || *read != 20 || write == nil || *write != 30 {
		t.Fatalf("unexpected usage: %+v", reply.Usage)
	}
}

func TestAnthropicUncachedUsage(t *testing.T) {
	reply, err := decodeAnthropicResponse([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":42,"output_tokens":7,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	var totals usageTotals
	totals.add(reply.Usage)
	input, output, read := totals.aggregate()
	write := totals.cacheWrites()
	if input == nil || *input != 42 || output == nil || *output != 7 || read == nil || *read != 0 || write == nil || *write != 0 {
		t.Fatalf("unexpected uncached usage: %+v", reply.Usage)
	}
}

func TestAnthropicCacheUsageEvents(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		fmt.Fprint(w, `{"model":"claude-test","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}`)
	})
	r := testRuntime(t, Config{BaseURL: f.server.URL, Format: FormatAnthropic})
	events := newRecorder()
	if _, err := r.Run(context.Background(), model.Turn{Task: model.Task{ID: "usage", Text: "question"}}, events.emit); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"usage", "model_request"} {
		event := events.event(t, kind)
		for _, field := range []string{`"inputTokens":60`, `"cachedInputTokens":20`, `"cacheWriteInputTokens":30`} {
			if !strings.Contains(event, field) {
				t.Fatalf("%s missing %s: %s", kind, field, event)
			}
		}
	}
}

func TestAnthropicCacheFallback(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"unsupported cache_control"}}`)
			return
		}
		fmt.Fprint(w, anthropicTextReply("done"))
	})
	r := testRuntime(t, Config{BaseURL: f.server.URL, Format: FormatAnthropic})
	turn := model.Turn{System: strings.Repeat("instructions ", 1500), Token: "private-turn-token", Task: model.Task{ID: "cache-fallback", Text: "question"}}
	for range 2 {
		if _, err := r.Run(context.Background(), turn, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 3 || !bytes.Contains(f.bodies[0], []byte(`"cache_control"`)) || bytes.Contains(f.bodies[1], []byte(`"cache_control"`)) || bytes.Contains(f.bodies[2], []byte(`"cache_control"`)) {
		t.Fatalf("fallback requests: %q", f.bodies)
	}
	for _, body := range f.bodies {
		if bytes.Contains(body, []byte(turn.Token)) {
			t.Fatalf("turn credential sent in cached request")
		}
	}
}

func TestAnthropicCacheDoesNotRetryOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":{"message":"invalid model"}}`},
		{500, `{"error":{"message":"unsupported cache_control"}}`},
		{400, `{"request":{"cache_control":"echoed"},"error":{"message":"invalid model"}}`},
	} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			r := testRuntime(t, Config{BaseURL: f.server.URL, Format: FormatAnthropic})
			_, err := r.Run(context.Background(), model.Turn{System: strings.Repeat("instructions ", 1500), Task: model.Task{ID: "no-retry", Text: "question"}}, nil)
			if err == nil {
				t.Fatal("expected provider error")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.bodies) != 1 {
				t.Fatalf("unexpected retry: %d", len(f.bodies))
			}
		})
	}
}
