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

func TestDirectSystemAndRepeatedCalls(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "ok"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call < 3 {
			fmt.Fprint(w, toolReply(fmt.Sprintf("call-%d", call), "meshra_workspace_read", `{"path":"same"}`))
			return
		}
		fmt.Fprint(w, textReply("done"))
	})
	rt := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	result, err := rt.Run(context.Background(), model.Turn{System: "stable instructions", Task: model.Task{Text: "question"}, Tools: executor}, nil)
	if err != nil || result != "done" {
		t.Fatalf("result %q: %v", result, err)
	}
	first := f.request(t, 0)
	if len(first.Messages) != 2 || first.Messages[0].Role != "system" || first.Messages[1].Role != "user" {
		t.Fatalf("wrong message roles: %+v", first.Messages)
	}
	if len(executor.executed()) != 2 {
		t.Fatalf("repeated call executed: %+v", executor.executed())
	}
	last := f.request(t, 3)
	if !strings.Contains(string(last.Messages[len(last.Messages)-1].Content), "change approach") {
		t.Fatal("missing repeated-call guidance")
	}
}

func TestDirectHistoryAlternatesAndKeepsPrefix(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) { fmt.Fprint(w, textReply("answer")) })
	rt := testRuntime(t, Config{BaseURL: f.server.URL})
	history := []model.ChatMessage{{Role: "user", Content: "first"}, {Role: "assistant", Content: "answer one"}}
	for _, turn := range []model.Turn{
		{System: "stable", History: history, Task: model.Task{Text: "second"}, Token: "secret-one"},
		{System: "stable", History: append(history, model.ChatMessage{Role: "user", Content: "second"}, model.ChatMessage{Role: "assistant", Content: "answer two"}), Task: model.Task{Text: "third"}, Token: "secret-two"},
	} {
		if _, err := rt.Run(context.Background(), turn, nil); err != nil {
			t.Fatal(err)
		}
	}
	first, second := f.request(t, 0), f.request(t, 1)
	if len(first.Messages) != 4 || len(second.Messages) != 6 {
		t.Fatalf("wrong history lengths: %+v %+v", first.Messages, second.Messages)
	}
	for i := range first.Messages {
		a, _ := json.Marshal(first.Messages[i])
		b, _ := json.Marshal(second.Messages[i])
		if string(a) != string(b) || strings.Contains(string(a), "secret-") {
			t.Fatalf("unstable or secret prefix: %s vs %s", a, b)
		}
	}
}

func TestDirectProfileToolLimit(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool()}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		fmt.Fprint(w, toolReply(fmt.Sprintf("call-%d", call), "meshra_workspace_read", fmt.Sprintf(`{"path":"%d"}`, call)))
	})
	rt := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	_, err := rt.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor, MaxToolCalls: 2}, nil)
	if err == nil || !strings.Contains(err.Error(), "2 tool call limit") || len(executor.executed()) != 2 {
		t.Fatalf("limit not enforced: %v (%d calls)", err, len(executor.executed()))
	}
}

func TestStreamAssemblyAndSecretAcrossDeltas(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello sec\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ret!\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	rt := testRuntime(t, Config{BaseURL: f.server.URL})
	events := newRecorder()
	result, err := rt.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Token: "secret"}, events.emit)
	if err != nil || result != "hello [redacted]!" {
		t.Fatalf("result %q: %v", result, err)
	}
	if got := strings.Join(events.events["delta"], ""); got != result {
		t.Fatalf("unsafe or incomplete deltas: %q", got)
	}
}

func TestStreamingExplicitRejectionFallsBack(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			http.Error(w, "stream not supported", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, textReply("ok"))
	})
	rt := testRuntime(t, Config{BaseURL: f.server.URL})
	result, err := rt.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}}, nil)
	if err != nil || result != "ok" || !f.request(t, 0).Stream || f.request(t, 1).Stream {
		t.Fatalf("fallback: %q %v", result, err)
	}
}

func TestCompactionPreservesLatestExchange(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) { fmt.Fprint(w, textReply("summary")) })
	rt := testRuntime(t, Config{BaseURL: f.server.URL}).(*runtime)
	mk := func(id string) chatMessage { return chatMessage{Role: "assistant", ToolCalls: []toolCall{{ID: id}}} }
	messages := []chatMessage{{Role: "system", Content: "rules"}, {Role: "user", Content: "question"}, mk("old"), {Role: "tool", ToolCallID: "old", Content: strings.Repeat("x", 100)}, mk("new"), {Role: "tool", ToolCallID: "new", Content: "latest"}}
	events := newRecorder()
	var totals usageTotals
	compacted, changed := rt.compact(context.Background(), messages, &totals, events.emit)
	if !changed || len(compacted) != 5 || compacted[0].Role != "system" || compacted[1].Role != "user" || compacted[3].ToolCalls[0].ID != "new" || compacted[4].Content != "latest" || events.count("context_compacted") != 1 {
		t.Fatalf("invalid compacted messages: %+v", compacted)
	}
	if !strings.Contains(string(f.request(t, 0).Messages[3].Content), "truncated") {
		t.Fatal("old tool result was not truncated before summary")
	}
}

func TestStreamReassemblesToolCalls(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: "read"}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call != 0 {
			fmt.Fprint(w, textReply("done"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"meshra_workspace_read\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"a\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	})
	rt := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	result, err := rt.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, nil)
	if err != nil || result != "done" || len(executor.executed()) != 1 || string(executor.executed()[0].Arguments) != `{"path":"a"}` {
		t.Fatalf("streamed call: %q %v %+v", result, err, executor.executed())
	}
}

func TestMutationBatchRemainsSequential(t *testing.T) {
	definition := workspaceTool()
	definition.Name = "meshra_workspace_write"
	executor := &concurrencyExecutor{definition: definition}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 0 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"one","type":"function","function":{"name":"meshra_workspace_write","arguments":"{\"path\":\"a\"}"}},{"id":"two","type":"function","function":{"name":"meshra_workspace_write","arguments":"{\"path\":\"b\"}"}}]}}]}`)
			return
		}
		fmt.Fprint(w, textReply("done"))
	})
	rt := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true})
	_, err := rt.Run(context.Background(), model.Turn{Task: model.Task{Text: "go"}, Tools: executor}, nil)
	if err != nil || executor.count() != 2 || executor.peak() != 1 {
		t.Fatalf("mutation overlap: %v, count=%d peak=%d", err, executor.count(), executor.peak())
	}
}

func TestHighContextCompactsInsideTurn(t *testing.T) {
	executor := &fakeExecutor{definition: workspaceTool(), result: model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: strings.Repeat("x", 200)}}}}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, call int) {
		switch call {
		case 0:
			fmt.Fprint(w, toolReply("one", "meshra_workspace_read", `{"path":"one"}`))
		case 1:
			reply := toolReply("two", "meshra_workspace_read", `{"path":"two"}`)
			fmt.Fprint(w, strings.TrimSuffix(reply, "}")+`,"usage":{"prompt_tokens":80}}`)
		case 2:
			fmt.Fprint(w, textReply("previous work"))
		default:
			fmt.Fprint(w, textReply("done"))
		}
	})
	rt := testRuntime(t, Config{BaseURL: f.server.URL, SupportsTools: true, ContextWindowTokens: 100})
	events := newRecorder()
	result, err := rt.Run(context.Background(), model.Turn{System: "rules", Task: model.Task{Text: "go"}, Tools: executor}, events.emit)
	if err != nil || result != "done" || events.count("context_compacted") != 1 {
		t.Fatalf("compaction: %q %v events=%d", result, err, events.count("context_compacted"))
	}
	request := f.request(t, 3)
	if len(request.Messages) != 5 || request.Messages[0].Role != "system" || request.Messages[1].Role != "user" || request.Messages[4].ToolCallID != "two" {
		t.Fatalf("compacted request: %+v", request.Messages)
	}
	var stats requestStats
	if err := json.Unmarshal([]byte(events.event(t, "model_request")), &stats); err != nil || !stats.Compacted {
		t.Fatalf("compacted request not marked: %+v %v", stats, err)
	}
}
