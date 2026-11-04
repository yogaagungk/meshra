package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const (
	// AnthropicVersion is the Messages API version header value.
	AnthropicVersion = "2023-06-01"
	// AnthropicMaxTokens is the per-reply output cap sent to the Messages API,
	// which requires max_tokens. It stays within every current model's limit.
	AnthropicMaxTokens = 8192
)

// setAuthHeaders applies the credential in the form the protocol expects. The
// key is only ever set on the request to the origin-pinned client.
func setAuthHeaders(header http.Header, cfg Config) {
	if cfg.Format == FormatAnthropic {
		header.Set("anthropic-version", AnthropicVersion)
		if cfg.APIKey != "" {
			header.Set("x-api-key", cfg.APIKey)
		}
		return
	}
	if cfg.APIKey != "" {
		header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
}

type anthropicBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	Source       *anthropicSource       `json:"source,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        json.RawMessage        `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      any                    `json:"content,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}
type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}
type anthropicCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}
type anthropicTool struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  json.RawMessage        `json:"input_schema"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}
type anthropicRequest struct {
	Model      string             `json:"model"`
	MaxTokens  int                `json:"max_tokens"`
	System     any                `json:"system,omitempty"`
	Messages   []anthropicMessage `json:"messages"`
	Tools      []anthropicTool    `json:"tools,omitempty"`
	ToolChoice map[string]string  `json:"tool_choice,omitempty"`
}

// encodeAnthropicRequest translates the neutral chat request to the Messages
// API. System messages become the system parameter; consecutive tool results
// share one user message, as the API requires.
func encodeAnthropicRequest(request chatRequest) ([]byte, error) {
	return encodeAnthropicRequestWithCaching(request, "off")
}

func encodeAnthropicRequestWithCaching(request chatRequest, mode string) ([]byte, error) {
	out := anthropicRequest{Model: request.Model, MaxTokens: AnthropicMaxTokens}
	var system []string
	for _, message := range request.Messages {
		switch message.Role {
		case "system":
			if text := contentText(message.Content); text != "" {
				system = append(system, text)
			}
		case "user":
			blocks := contentBlocks(message.Content)
			if len(blocks) == 0 {
				continue
			}
			out.Messages = appendMessage(out.Messages, "user", blocks)
		case "assistant":
			var blocks []anthropicBlock
			if text := contentText(message.Content); text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: text})
			}
			for _, call := range message.ToolCalls {
				input := json.RawMessage(strings.TrimSpace(call.Function.Arguments))
				if len(input) == 0 || !json.Valid(input) || input[0] != '{' {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: input})
			}
			if len(blocks) == 0 {
				continue
			}
			out.Messages = appendMessage(out.Messages, "assistant", blocks)
		case "tool":
			result := anthropicBlock{Type: "tool_result", ToolUseID: message.ToolCallID}
			if blocks := contentBlocks(message.Content); len(blocks) > 0 {
				result.Content = blocks
			} else {
				result.Content = ""
			}
			out.Messages = appendMessage(out.Messages, "user", []anthropicBlock{result})
		default:
			return nil, errors.New("unsupported message role for the Anthropic protocol: " + message.Role)
		}
	}
	if len(out.Messages) == 0 {
		return nil, errors.New("the Anthropic request has no messages")
	}
	out.System = strings.Join(system, "\n\n")
	for _, tool := range request.Tools {
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: schema})
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = map[string]string{"type": "auto"}
	}
	if mode != "off" && anthropicCacheable(request, out) {
		control := &anthropicCacheControl{Type: "ephemeral"}
		if mode == "1h" {
			control.TTL = "1h"
		}
		if len(out.Tools) > 0 {
			out.Tools[len(out.Tools)-1].CacheControl = control
		}
		if text := strings.Join(system, "\n\n"); text != "" {
			out.System = []anthropicBlock{{Type: "text", Text: text, CacheControl: control}}
		}
		blocks := out.Messages[len(out.Messages)-1].Content.([]anthropicBlock)
		blocks[len(blocks)-1].CacheControl = control
	}
	return json.Marshal(out)
}

// anthropicCacheable estimates text tokens conservatively from encoded text.
// Unknown models use 4096 tokens; known Claude models with lower thresholds
// can start caching at 1024. Images are deliberately excluded.
func anthropicCacheable(request chatRequest, out anthropicRequest) bool {
	minimum := 4096
	model := strings.ToLower(request.Model)
	if strings.Contains(model, "claude-sonnet-4-5") || strings.Contains(model, "claude-sonnet-4-2025") || strings.Contains(model, "claude-3-7-sonnet") {
		minimum = 1024
	}
	size := 0
	for _, tool := range out.Tools {
		size += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	if text, ok := out.System.(string); ok {
		size += len(text)
	}
	for _, message := range out.Messages {
		for _, block := range message.Content.([]anthropicBlock) {
			size += len(block.Text) + len(block.Input)
			if nested, ok := block.Content.([]anthropicBlock); ok {
				for _, item := range nested {
					size += len(item.Text)
				}
			}
		}
	}
	return size/4 >= minimum
}

// appendMessage merges a message into the previous one when the role repeats.
func appendMessage(messages []anthropicMessage, role string, blocks []anthropicBlock) []anthropicMessage {
	if n := len(messages); n > 0 && messages[n-1].Role == role {
		if previous, ok := messages[n-1].Content.([]anthropicBlock); ok {
			messages[n-1].Content = append(previous, blocks...)
			return messages
		}
	}
	return append(messages, anthropicMessage{Role: role, Content: blocks})
}

// contentText returns the text of a neutral message content value.
func contentText(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []contentPart:
		var text strings.Builder
		for _, part := range value {
			if part.Type == "text" {
				text.WriteString(part.Text)
			}
		}
		return text.String()
	}
	return ""
}

// contentBlocks converts neutral content to Anthropic blocks. Empty text is
// dropped because the API rejects empty text blocks.
func contentBlocks(content any) []anthropicBlock {
	var blocks []anthropicBlock
	switch value := content.(type) {
	case string:
		if value != "" {
			blocks = append(blocks, anthropicBlock{Type: "text", Text: value})
		}
	case []contentPart:
		for _, part := range value {
			switch {
			case part.Type == "text" && part.Text != "":
				blocks = append(blocks, anthropicBlock{Type: "text", Text: part.Text})
			case part.Type == "image_url" && part.ImageURL != nil:
				if media, data, ok := splitDataURL(part.ImageURL.URL); ok {
					blocks = append(blocks, anthropicBlock{Type: "image", Source: &anthropicSource{Type: "base64", MediaType: media, Data: data}})
				}
			}
		}
	}
	return blocks
}

// splitDataURL parses data:<mime>;base64,<data>.
func splitDataURL(raw string) (media, data string, ok bool) {
	rest, found := strings.CutPrefix(raw, "data:")
	if !found {
		return "", "", false
	}
	header, data, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	media, encoding, _ := strings.Cut(header, ";")
	if encoding != "base64" || media == "" || data == "" {
		return "", "", false
	}
	return media, data, true
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Model   string `json:"model"`
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      *struct {
		InputTokens              *int64 `json:"input_tokens"`
		OutputTokens             *int64 `json:"output_tokens"`
		CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// decodeAnthropicResponse maps a Messages API reply to the neutral shape the
// turn loop consumes.
func decodeAnthropicResponse(raw []byte) (chatResponse, error) {
	var reply anthropicResponse
	if err := json.Unmarshal(raw, &reply); err != nil {
		return chatResponse{}, errors.New("Anthropic endpoint returned an invalid JSON response")
	}
	if reply.Error != nil && reply.Error.Message != "" {
		return chatResponse{}, errors.New("Anthropic endpoint error: " + reply.Error.Message)
	}
	var text bytes.Buffer
	var calls []toolCall
	for _, block := range reply.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			arguments := strings.TrimSpace(string(block.Input))
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			call := toolCall{ID: block.ID, Type: "function"}
			call.Function.Name = block.Name
			call.Function.Arguments = arguments
			calls = append(calls, call)
		}
	}
	content, err := json.Marshal(text.String())
	if err != nil {
		return chatResponse{}, err
	}
	out := chatResponse{ID: reply.ID, Model: reply.Model}
	out.Choices = append(out.Choices, struct {
		Index        int              `json:"index"`
		Message      assistantMessage `json:"message"`
		FinishReason string           `json:"finish_reason"`
	}{Message: assistantMessage{Role: "assistant", Content: content, ToolCalls: calls}, FinishReason: reply.StopReason})
	if reply.Usage != nil {
		usage := &chatUsage{CompletionTokens: reply.Usage.OutputTokens, CachedTokens: reply.Usage.CacheReadInputTokens, CacheWriteTokens: reply.Usage.CacheCreationInputTokens}
		if reply.Usage.InputTokens != nil {
			// Anthropic reports uncached input separately; the prompt size
			// the console tracks is the whole input.
			total := *reply.Usage.InputTokens
			if reply.Usage.CacheReadInputTokens != nil {
				total += *reply.Usage.CacheReadInputTokens
			}
			if reply.Usage.CacheCreationInputTokens != nil {
				total += *reply.Usage.CacheCreationInputTokens
			}
			usage.PromptTokens = &total
		}
		out.Usage = usage
	}
	return out, nil
}
