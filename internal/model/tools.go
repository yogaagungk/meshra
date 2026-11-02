package model

import (
 "context"
 "encoding/json"
)

type ToolDefinition struct {
 Name string `json:"name"`
 Description string `json:"description"`
 InputSchema json.RawMessage `json:"inputSchema"`
}
type ToolCall struct {
 ID string `json:"id"`
 Name string `json:"name"`
 Arguments json.RawMessage `json:"arguments"`
}
type ToolContent struct {
 Type string `json:"type"`
 Text string `json:"text,omitempty"`
 MimeType string `json:"mimeType,omitempty"`
 Data string `json:"data,omitempty"`
}
type ToolResult struct {
 Content []ToolContent `json:"content"`
 IsError bool `json:"isError"`
}
type ToolExecutor interface {
 Definitions() []ToolDefinition
 Call(context.Context,ToolCall)(ToolResult,error)
}

// ActiveToolService narrows profile credentials to the owning active attempt.
// A nil executor preserves the legacy server for non-profile identities only.
type ActiveToolService interface {
 ActiveTools(Principal)(ToolExecutor,error)
}

// ScopedToolEndpoint is available only to trusted confined harness adapters.
type ScopedToolEndpoint interface {
 ScopedMCP(context.Context)(socketPath string,credential string,err error)
}

// ToolEventLimit bounds the input or output carried by one tool event.
const ToolEventLimit = 4000

// ToolEvent is the payload of a "tool_use" or "tool_result" task event. Every
// adapter reports tool activity in this one shape so the console can render it.
type ToolEvent struct {
	ID      string          `json:"id"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"isError,omitempty"`
}

// ToolEventData encodes a tool event with bounded input and output.
func ToolEventData(e ToolEvent) string {
	if len(e.Input) > ToolEventLimit || !json.Valid(e.Input) {
		text := string(e.Input)
		if len(text) > ToolEventLimit {
			text = text[:ToolEventLimit] + " [truncated]"
		}
		e.Input, _ = json.Marshal(text)
	}
	if len(e.Output) > ToolEventLimit {
		cut := ToolEventLimit
		for cut > 0 && e.Output[cut]&0xC0 == 0x80 {
			cut--
		}
		e.Output = e.Output[:cut] + " [truncated]"
	}
	b, err := json.Marshal(e)
	if err != nil {
		return `{"id":"unknown"}`
	}
	return string(b)
}
