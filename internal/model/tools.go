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
