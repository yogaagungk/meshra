package openai

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"meshra/internal/model"
)

// requestStats is one model call as the stats dashboard records it. It carries
// numbers and short identifiers only, never message text.
type requestStats struct {
	Seq        int    `json:"seq"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	DurationMs *int64 `json:"durationMs,omitempty"`
	TTFTMs     *int64 `json:"ttftMs,omitempty"`
	StopReason string `json:"stopReason"`
	Error      string `json:"error,omitempty"`
	Input      int64  `json:"inputTokens"`
	Output     int64  `json:"outputTokens"`
	Cached     int64  `json:"cachedInputTokens"`
	CacheWrite int64  `json:"cacheWriteInputTokens"`
	Compacted  bool   `json:"compacted,omitempty"`
}

// normalizeStop maps provider finish reasons to the names the dashboard uses.
func normalizeStop(reason string) string {
	switch reason {
	case "stop", "end_turn", "stop_sequence", "":
		return "stop"
	case "tool_calls", "tool_use", "function_call":
		return "toolUse"
	case "length", "max_tokens":
		return "length"
	}
	return reason
}

// emitRequest reports one completed or failed model call.
func (r *runtime) emitRequest(emit model.Emit, ctx context.Context, seq int, started time.Time, ttft time.Duration, reply chatResponse, callErr error, compacted bool) {
	if emit == nil {
		return
	}
	duration := time.Since(started).Milliseconds()
	stats := requestStats{Seq: seq, Model: r.agent.Config.Model, Provider: r.agent.Config.Provider, DurationMs: &duration, Compacted: compacted}
	if ttft > 0 {
		v := ttft.Milliseconds()
		stats.TTFTMs = &v
	}
	if callErr != nil {
		stats.StopReason = "error"
		if ctx.Err() != nil {
			stats.StopReason = "aborted"
		} else {
			stats.Error = redact(callErr.Error(), r.cfg.APIKey)
		}
	} else {
		if reply.Model != "" {
			stats.Model = reply.Model
		}
		if len(reply.Choices) > 0 {
			stats.StopReason = normalizeStop(reply.Choices[0].FinishReason)
			if len(reply.Choices[0].Message.ToolCalls) > 0 {
				stats.StopReason = "toolUse"
			}
		}
		if u := reply.Usage; u != nil {
			if u.PromptTokens != nil {
				stats.Input = *u.PromptTokens
			}
			if u.CompletionTokens != nil {
				stats.Output = *u.CompletionTokens
			}
			switch {
			case u.CachedTokens != nil:
				stats.Cached = *u.CachedTokens
			case u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil:
				stats.Cached = *u.PromptTokensDetails.CachedTokens
			}
			if u.CacheWriteTokens != nil {
				stats.CacheWrite = *u.CacheWriteTokens
			}
		}
	}
	stats.Error = strings.TrimSpace(stats.Error)
	if data, err := json.Marshal(stats); err == nil {
		emit("model_request", string(data))
	}
}
