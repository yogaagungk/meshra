package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"meshra/internal/model"
)

// chatStream retries only a definite, explicit rejection of streaming, never a
// transport failure or a response whose completion outcome is unknown.
func (r *runtime) chatStream(ctx context.Context, request chatRequest, token string, emit model.Emit) (chatResponse, error) {
	if r.cfg.Format == FormatAnthropic {
		// The Messages API streams a different event protocol; use one
		// complete reply until that protocol is implemented.
		return r.chat(ctx, request)
	}
	start := time.Now()
	request.Stream = true
	request.StreamOptions = &struct {
		IncludeUsage bool `json:"include_usage"`
	}{IncludeUsage: true}
	body, err := json.Marshal(request)
	if err != nil {
		return chatResponse{}, err
	}
	target, err := r.chatURL()
	if err != nil {
		return chatResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return chatResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	if r.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return chatResponse{}, fmt.Errorf("OpenAI-compatible request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, e := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBytes+1))
		if e != nil {
			return chatResponse{}, fmt.Errorf("OpenAI-compatible response read: %w", e)
		}
		message := strings.ToLower(string(raw))
		if (resp.StatusCode == 400 || resp.StatusCode == 422 || resp.StatusCode == 501) && strings.Contains(message, "stream") && (strings.Contains(message, "unsupported") || strings.Contains(message, "not support") || strings.Contains(message, "not implemented") || strings.Contains(message, "unknown") || strings.Contains(message, "unrecognized")) {
			request.StreamOptions = nil
			request.Stream = false
			return r.chat(ctx, request)
		}
		return chatResponse{}, fmt.Errorf("OpenAI-compatible endpoint returned HTTP %d: %s", resp.StatusCode, r.snippet(raw))
	}
	// Some compatible servers ignore stream and return an ordinary JSON body.
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		raw, e := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
		if e != nil {
			return chatResponse{}, e
		}
		if len(raw) > MaxResponseBytes {
			return chatResponse{}, errors.New("OpenAI-compatible response exceeds the 8 MiB bound")
		}
		var reply chatResponse
		if json.Unmarshal(raw, &reply) != nil {
			return chatResponse{}, errors.New("OpenAI-compatible endpoint returned an invalid JSON response")
		}
		if reply.Error != nil && reply.Error.Message != "" {
			return chatResponse{}, errors.New("OpenAI-compatible endpoint error: " + r.snippet([]byte(reply.Error.Message)))
		}
		return reply, nil
	}
	reader := bufio.NewScanner(io.LimitReader(resp.Body, MaxResponseBytes+1))
	reader.Buffer(make([]byte, 4096), MaxResponseBytes+1)
	var reply chatResponse
	var text strings.Builder
	calls := map[int]*toolCall{}
	var pending strings.Builder
	var data []string
	done := false
	total := 0
	flushDelta := func(final bool) {
		if emit == nil {
			pending.Reset()
			return
		}
		s := pending.String()
		// Retain any suffix that could be the beginning of a secret, so no
		// credential can cross an emitted fragment boundary.
		keep := 0
		if !final {
			for _, secret := range []string{token, r.cfg.APIKey} {
				for n := 1; n < len(secret) && n <= len(s); n++ {
					if strings.HasSuffix(s, secret[:n]) && n > keep {
						keep = n
					}
				}
			}
		}
		part := s[:len(s)-keep]
		pending.Reset()
		pending.WriteString(s[len(s)-keep:])
		part = redact(part, token)
		part = redact(part, r.cfg.APIKey)
		if part != "" {
			emit("delta", part)
		}
	}
	process := func(raw string) error {
		if raw == "[DONE]" {
			done = true
			return nil
		}
		var chunk struct {
			ID    string     `json:"id"`
			Model string     `json:"model"`
			Usage *chatUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Index        int    `json:"index"`
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(raw), &chunk) != nil {
			return errors.New("OpenAI-compatible endpoint returned an invalid stream event")
		}
		if chunk.Error != nil {
			return errors.New("OpenAI-compatible endpoint error: " + r.snippet([]byte(chunk.Error.Message)))
		}
		if chunk.ID != "" {
			reply.ID = chunk.ID
		}
		if chunk.Model != "" {
			reply.Model = chunk.Model
		}
		if chunk.Usage != nil {
			reply.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if len(reply.Choices) == 0 {
				reply.Choices = append(reply.Choices, struct {
					Index        int              `json:"index"`
					Message      assistantMessage `json:"message"`
					FinishReason string           `json:"finish_reason"`
				}{})
			}
			if choice.FinishReason != "" {
				reply.Choices[0].FinishReason = choice.FinishReason
			}
			if r.ttft == 0 && (choice.Delta.Content != "" || len(choice.Delta.ToolCalls) > 0) {
				r.ttft = time.Since(start)
			}
			text.WriteString(choice.Delta.Content)
			pending.WriteString(choice.Delta.Content)
			flushDelta(false)
			for _, delta := range choice.Delta.ToolCalls {
				if delta.Index < 0 || delta.Index >= 1000 {
					return errors.New("OpenAI-compatible stream has invalid tool index")
				}
				call := calls[delta.Index]
				if call == nil {
					call = &toolCall{}
					calls[delta.Index] = call
				}
				call.ID += delta.ID
				call.Type += delta.Type
				call.Function.Name += delta.Function.Name
				call.Function.Arguments += delta.Function.Arguments
			}
		}
		return nil
	}
	for reader.Scan() {
		line := reader.Text()
		total += len(line) + 1
		if total > MaxResponseBytes {
			return chatResponse{}, errors.New("OpenAI-compatible response exceeds the 8 MiB bound")
		}
		if line == "" {
			if len(data) != 0 {
				if err := process(strings.Join(data, "\n")); err != nil {
					return chatResponse{}, err
				}
				data = nil
			}
			if done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := reader.Err(); err != nil {
		return chatResponse{}, fmt.Errorf("OpenAI-compatible response read: %w", err)
	}
	if !done {
		return chatResponse{}, errors.New("OpenAI-compatible stream ended before completion")
	}
	flushDelta(true)
	if len(reply.Choices) > 0 {
		raw, _ := json.Marshal(text.String())
		reply.Choices[0].Message.Content = raw
		indices := make([]int, 0, len(calls))
		for i := range calls {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, i := range indices {
			reply.Choices[0].Message.ToolCalls = append(reply.Choices[0].Message.ToolCalls, *calls[i])
		}
	}
	return reply, nil
}
