package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"meshra/internal/model"
)

func TestToolEventsArePersistedCountedAndRedacted(t *testing.T) {
	r, _ := openTest(t, func(_ context.Context, turn model.Turn, emit model.Emit) (string, error) {
		emit("tool_use", model.ToolEventData(model.ToolEvent{ID: "call-1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}))
		emit("tool_result", model.ToolEventData(model.ToolEvent{ID: "call-1", Output: "file.txt password=hunter2hunter2"}))
		return "done", nil
	})
	ai := createTest(t, r, "omp", "toolie")
	startTest(t, r, ai.ID)
	c := createChat(t, r, ai.ID)
	task := awaitTask(t, r, sendChat(t, r, c, "list files").ID)
	if task.State != "completed" || task.ToolCalls != 1 {
		t.Fatalf("unexpected task: state=%s toolCalls=%d", task.State, task.ToolCalls)
	}
	events, err := r.Events(admin, task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var use, result string
	for _, e := range events {
		switch e.Type {
		case "tool_use":
			use = e.Data
		case "tool_result":
			result = e.Data
		}
	}
	if !strings.Contains(use, `"name":"Bash"`) || !strings.Contains(use, `"command":"ls"`) {
		t.Fatalf("tool_use missing or malformed: %s", use)
	}
	if result == "" || strings.Contains(result, "hunter2hunter2") {
		t.Fatalf("tool_result missing or leaked a credential: %s", result)
	}
}

func TestToolEventDataBoundsInputAndOutput(t *testing.T) {
	big := strings.Repeat("界", model.ToolEventLimit)
	data := model.ToolEventData(model.ToolEvent{ID: "x", Name: "n", Input: json.RawMessage(`"` + big + `"`), Output: big})
	var e model.ToolEvent
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		t.Fatalf("bounded event is not valid JSON: %v", err)
	}
	if len(e.Output) > model.ToolEventLimit+len(" [truncated]") || len(data) > 3*model.ToolEventLimit {
		t.Fatalf("event was not bounded: %d bytes", len(data))
	}
}
