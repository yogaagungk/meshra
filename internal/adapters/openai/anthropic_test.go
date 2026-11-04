package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"meshra/internal/model"
)

func anthropicToolReply() string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_1","name":"meshra_workspace_read","input":{"path":"notes.md"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":4}}`
}

func anthropicTextReply(text string) string {
	body, _ := json.Marshal(text)
	return fmt.Sprintf(`{"id":"msg_2","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":%s}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":7}}`, body)
}

func TestAnthropicToolRoundTrip(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "file contents"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if call == 0 {
			fmt.Fprint(w, anthropicToolReply())
			return
		}
		fmt.Fprint(w, anthropicTextReply("done"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, APIKey: "sk-ant-fixture", SupportsTools: true, Format: FormatAnthropic})
	events := newRecorder()
	result, err := runtime.Run(context.Background(), model.Turn{System: "be brief", Task: model.Task{ID: "task-1", Text: "read notes"}, Tools: executor}, events.emit)
	if err != nil {
		t.Fatal(err)
	}
	if result != "done" {
		t.Fatalf("unexpected result %q", result)
	}
	if calls := executor.executed(); len(calls) != 1 || calls[0].ID != "toolu_1" || !strings.Contains(string(calls[0].Arguments), "notes.md") {
		t.Fatalf("unexpected tool calls %+v", calls)
	}
	head := f.header(t, 0)
	if head.Get("x-api-key") != "sk-ant-fixture" || head.Get("anthropic-version") != AnthropicVersion || head.Get("Authorization") != "" {
		t.Fatalf("unexpected auth headers %v", head)
	}
	var second struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				ToolUseID string          `json:"tool_use_id"`
				Input     json.RawMessage `json:"input"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice map[string]string `json:"tool_choice"`
	}
	f.mu.Lock()
	raw := f.bodies[1]
	f.mu.Unlock()
	if err := json.Unmarshal(raw, &second); err != nil {
		t.Fatal(err)
	}
	if second.System != "be brief" || second.MaxTokens != AnthropicMaxTokens || second.Model != "test-model" {
		t.Fatalf("unexpected request envelope: %s", raw)
	}
	if len(second.Messages) != 3 {
		t.Fatalf("expected user, assistant and tool-result messages: %s", raw)
	}
	assistant := second.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 2 || assistant.Content[1].Type != "tool_use" || assistant.Content[1].ID != "toolu_1" || !strings.Contains(string(assistant.Content[1].Input), "notes.md") {
		t.Fatalf("assistant tool_use was not replayed: %s", raw)
	}
	result0 := second.Messages[2]
	if result0.Role != "user" || result0.Content[0].Type != "tool_result" || result0.Content[0].ToolUseID != "toolu_1" || !strings.Contains(string(result0.Content[0].Content), "file contents") {
		t.Fatalf("tool_result was not returned: %s", raw)
	}
	if len(second.Tools) != 1 || second.Tools[0].Name != "meshra_workspace_read" || len(second.Tools[0].InputSchema) == 0 || second.ToolChoice["type"] != "auto" {
		t.Fatalf("tools were not offered: %s", raw)
	}
	if usage := events.event(t, "usage"); !strings.Contains(usage, `"inputTokens":34`) || !strings.Contains(usage, `"cachedInputTokens":4`) {
		t.Fatalf("usage not mapped: %s", usage)
	}
}

func TestAnthropicMergesConsecutiveToolResults(t *testing.T) {
	body, err := encodeAnthropicRequest(chatRequest{Model: "m", Messages: []chatMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "a", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "x", Arguments: "{}"}}, {ID: "b", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "x", Arguments: "not json"}}}},
		{Role: "tool", ToolCallID: "a", Content: "one"},
		{Role: "tool", ToolCallID: "b", Content: "two"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 3 || len(decoded.Messages[2].Content) != 2 {
		t.Fatalf("tool results must share one user message: %s", body)
	}
	if !strings.Contains(string(decoded.Messages[1].Content[1]), `"input":{}`) {
		t.Fatalf("invalid arguments must become an empty object: %s", decoded.Messages[1].Content[1])
	}
}

func TestAnthropicImageAndError(t *testing.T) {
	body, err := encodeAnthropicRequest(chatRequest{Model: "m", Messages: []chatMessage{{Role: "user", Content: []contentPart{{Type: "text", Text: "look"}, {Type: "image_url", ImageURL: &imageURL{URL: "data:image/png;base64,QUJD"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"source":{"type":"base64","media_type":"image/png","data":"QUJD"}`) {
		t.Fatalf("image block missing: %s", body)
	}
	if _, err := decodeAnthropicResponse([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("provider error not surfaced: %v", err)
	}
}

func TestTypeSafeCannotRunChatTurn(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, Format: FormatTypeSafe})
	_, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{ID: "t", Text: "hi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "TypeSafe") {
		t.Fatalf("expected a clear TypeSafe refusal, got %v", err)
	}
	if f.count() != 0 {
		t.Fatal("no request may be sent for a TypeSafe endpoint")
	}
}

func TestModelRequestEventsCarryTimingAndUsage(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "ok"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		if call == 0 {
			fmt.Fprint(w, anthropicToolReply())
			return
		}
		fmt.Fprint(w, anthropicTextReply("done"))
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, APIKey: "sk-ant-fixture", SupportsTools: true, Format: FormatAnthropic})
	events := newRecorder()
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{ID: "task-1", Text: "go"}, Tools: executor}, events.emit); err != nil {
		t.Fatal(err)
	}
	if events.count("model_request") != 2 {
		t.Fatalf("expected one event per model call, got %d", events.count("model_request"))
	}
	var last requestStats
	if err := json.Unmarshal([]byte(events.event(t, "model_request")), &last); err != nil {
		t.Fatal(err)
	}
	if last.Seq != 2 || last.StopReason != "stop" || last.DurationMs == nil || last.Output != 7 || last.Input != 20 || last.Provider != "endpoint-1" {
		t.Fatalf("unexpected request stats: %+v", last)
	}
	if strings.Contains(events.event(t, "model_request"), "sk-ant-fixture") {
		t.Fatal("a credential reached the stats event")
	}
}

func TestModelRequestEventForFailedCall(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	})
	runtime := testRuntime(t, Config{BaseURL: f.server.URL, Format: FormatOpenAI})
	events := newRecorder()
	if _, err := runtime.Run(context.Background(), model.Turn{Task: model.Task{ID: "task-1", Text: "go"}}, events.emit); err == nil {
		t.Fatal("expected a failure")
	}
	var stats requestStats
	if err := json.Unmarshal([]byte(events.event(t, "model_request")), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.StopReason != "error" || !strings.Contains(stats.Error, "429") {
		t.Fatalf("a failed call must be recorded with its status: %+v", stats)
	}
}
