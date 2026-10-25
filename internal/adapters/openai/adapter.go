// Package openai implements a real OpenAI-compatible chat-completions adapter.
//
// The adapter speaks the OpenAI HTTP API directly with net/http: turns are
// POST <base>/chat/completions and discovery is GET <base>/models. It owns no
// provider session, so it never claims durable history; the relay reconstructs
// a bounded transcript from the exact conversation for every turn.
//
// Credentials are supplied by the caller through the endpoint resolver. This
// package never reads an API key from a file, an environment variable or a
// native credential store.
package openai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"meshra/internal/model"
)

const (
	// MaxToolCalls bounds the broker dispatches of a single turn.
	MaxToolCalls = 40
	// MaxResponseBytes bounds one endpoint response body.
	MaxResponseBytes = 8 << 20
	// MaxFrameBytes bounds the encoded tool result handed back to the model.
	MaxFrameBytes = 1 << 20
	// MaxArgumentBytes bounds one tool call's arguments.
	MaxArgumentBytes = 128 << 10
	// MaxTextPartBytes bounds one textual tool result part.
	MaxTextPartBytes = 64 << 10
	// MaxErrorBytes bounds a provider error snippet.
	MaxErrorBytes = 512
	// MaxURLBytes bounds the configured endpoint URL.
	MaxURLBytes = 2048
	// MaxModelsResponseBytes bounds one model list response body.
	MaxModelsResponseBytes = 2 << 20
	// MaxModels bounds a discovered model list.
	MaxModels = 500
)

// Config describes one configured OpenAI-compatible endpoint.
type Config struct {
	// BaseURL is the endpoint root. A missing or duplicated /v1 suffix is normalized.
	BaseURL string
	// APIKey is the bearer credential supplied by the caller.
	APIKey string
	// SupportsTools advertises tool calling for this endpoint.
	SupportsTools bool
	// SupportsImages advertises image input for the selected model.
	SupportsImages bool
	// ContextWindowTokens is the operator-declared context capacity, when known.
	ContextWindowTokens int
	// HTTPClient pins requests to the endpoint origin. The caller supplies the
	// origin-pinned client; the adapter falls back to NewClient without one.
	HTTPClient *http.Client
	// ChatCompletionsURL is the exact completion URL when an endpoint does not
	// follow the <root>/v1/chat/completions layout.
	ChatCompletionsURL string
	// ModelsURL is the exact model list URL when an endpoint does not follow
	// the <root>/v1/models layout.
	ModelsURL string
}

// Resolver returns the configuration of a configured endpoint identifier.
type Resolver func(context.Context, string) (Config, error)

// New registers the OpenAI-compatible harness. The resolver supplies the base
// URL, the API key and the declared endpoint capabilities for the endpoint
// identifier carried in agent.Config.Provider.
func New(resolve Resolver) model.Adapter {
	return model.Adapter{
		Capabilities: map[string]bool{"prompt": true, "stream": false, "resume": false, "durableHistory": false, "toolFreeDraft": true, "cancel": true, "mcp": false, "read_only": true, "approval": false, "interactiveApproval": false, "authenticatedSmokeVerified": false, "profileAutomation": true, "supportsImages": true},
		Factory: func(ctx context.Context, agent model.Agent, emit model.Emit) (model.Runtime, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if resolve == nil {
				return nil, errors.New("OpenAI-compatible harness requires an endpoint resolver")
			}
			if agent.Config.Provider == "" {
				return nil, errors.New("OpenAI-compatible harness requires a configured endpoint identifier in the provider field")
			}
			if agent.Config.Model == "" {
				return nil, errors.New("OpenAI-compatible harness requires a model")
			}
			cfg, err := resolve(ctx, agent.Config.Provider)
			if err != nil {
				return nil, err
			}
			base, err := NormalizeBaseURL(cfg.BaseURL)
			if err != nil {
				return nil, err
			}
			if cfg.ChatCompletionsURL != "" {
				if _, err = endpointURL(cfg.ChatCompletionsURL); err != nil {
					return nil, err
				}
			}
			if cfg.ModelsURL != "" {
				if _, err = endpointURL(cfg.ModelsURL); err != nil {
					return nil, err
				}
			}
			client := cfg.HTTPClient
			if client == nil {
				client = NewClient()
			}
			return &runtime{life: ctx, agent: agent, cfg: cfg, base: base, client: client, session: sessionID(agent), resolve: resolve}, nil
		},
	}
}

// NewClient returns the bounded HTTP client used for endpoint traffic. It
// never uses an environment proxy and never follows a redirect: the request
// stays on the configured origin.
func NewClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("OpenAI-compatible endpoint redirects are refused")
		},
	}
}

// NormalizeBaseURL validates an endpoint root and appends the
// OpenAI-compatible /v1 suffix exactly once.
func NormalizeBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("endpoint base URL is missing")
	}
	if len(raw) > MaxURLBytes {
		return "", errors.New("endpoint base URL exceeds 2048 bytes")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("endpoint base URL is not a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("endpoint base URL must use http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("endpoint base URL is missing a host")
	}
	if parsed.User != nil {
		return "", errors.New("endpoint base URL must not carry credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errors.New("endpoint base URL must not carry a query")
	}
	if parsed.Fragment != "" {
		return "", errors.New("endpoint base URL must not carry a fragment")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.String(), nil
}

type runtime struct {
	life    context.Context
	agent   model.Agent
	cfg     Config
	base    string
	client  *http.Client
	session string
	mu      sync.Mutex
	closed  bool
	cancel  context.CancelFunc
	run     sync.Mutex
	resolve Resolver
}

// SessionID is the Meshra-side turn identity of this stateless harness. The
// endpoint holds no resumable state, so this is a local identity that scopes
// the turn credential and the tool broker; it is never a provider session.
func (r *runtime) SessionID() string { return r.session }

// sessionID keeps the identity of a started agent and allocates a fresh one
// whenever the relay released the previous identity.
func sessionID(agent model.Agent) string {
	if strings.HasPrefix(agent.SessionID, "openai-") {
		return agent.SessionID
	}
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "openai-" + strings.ReplaceAll(agent.ID, ":", "-") + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "openai-" + strings.ReplaceAll(agent.ID, ":", "-") + "-" + hex.EncodeToString(buffer)
}
func (r *runtime) Close() error {
	r.mu.Lock()
	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.run.Lock()
	r.run.Unlock()
	return nil
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}
type imageURL struct {
	URL string `json:"url"`
}
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type toolDefinition struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}
type chatRequest struct {
	Model      string           `json:"model"`
	Messages   []chatMessage    `json:"messages"`
	Tools      []toolDefinition `json:"tools,omitempty"`
	ToolChoice string           `json:"tool_choice,omitempty"`
	Stream     bool             `json:"stream"`
}
type chatUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	TotalTokens         *int64 `json:"total_tokens"`
	CachedTokens        *int64 `json:"cached_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// assistantMessage is one provider reply.
type assistantMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []toolCall      `json:"tool_calls"`
}
type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int              `json:"index"`
		Message      assistantMessage `json:"message"`
		FinishReason string           `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// text returns the plain text of an assistant message. A structured content
// array contributes only its text parts; no markup is forwarded.
func (m assistantMessage) text() string {
	raw := bytes.TrimSpace(m.Content)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return ""
		}
		return value
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var text strings.Builder
	for _, part := range parts {
		if part.Type == "text" || part.Type == "" {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

// usageReport is one model call's reported usage.
type usageReport struct {
	prompt     *int64
	completion *int64
	cached     *int64
}
type usageTotals struct {
	reports []usageReport
}

func (u *usageTotals) add(report *chatUsage) {
	if report == nil {
		return
	}
	item := usageReport{}
	if report.PromptTokens != nil {
		value := *report.PromptTokens
		item.prompt = &value
	}
	if report.CompletionTokens != nil {
		value := *report.CompletionTokens
		item.completion = &value
	}
	cached := report.CachedTokens
	if cached == nil && report.PromptTokensDetails != nil {
		cached = report.PromptTokensDetails.CachedTokens
	}
	if cached != nil {
		value := *cached
		item.cached = &value
	}
	u.reports = append(u.reports, item)
}

// aggregate sums the values that the model calls of this turn reported. A
// value that no call reported stays unknown: it is never reported as zero.
func (u usageTotals) aggregate() (input, output, cached *int64) {
	if len(u.reports) == 0 {
		return nil, nil, nil
	}
	sum := func(pick func(usageReport) *int64) *int64 {
		total := int64(0)
		reported := false
		for _, report := range u.reports {
			value := pick(report)
			if value == nil {
				continue
			}
			reported = true
			total += *value
		}
		if !reported {
			return nil
		}
		return &total
	}
	return sum(func(r usageReport) *int64 { return r.prompt }), sum(func(r usageReport) *int64 { return r.completion }), sum(func(r usageReport) *int64 { return r.cached })
}

func (r *runtime) Run(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
	r.run.Lock()
	defer r.run.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopLife := context.AfterFunc(r.life, cancel)
	defer stopLife()
	if err := r.life.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", errors.New("OpenAI-compatible runtime closed")
	}
	r.cancel = cancel
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.cancel = nil; r.mu.Unlock() }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Resolve the admitted turn, not the revision seen at identity startup.
	cfg, err := r.resolve(ctx, r.agent.Config.Provider)
	if err != nil {
		return "", err
	}
	base, err := NormalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return "", err
	}
	r.cfg, r.base, r.client = cfg, base, cfg.HTTPClient
	if r.client == nil {
		r.client = NewClient()
	}
	executor := turn.Tools
	if r.agent.GenerationDraft {
		// A profile draft is tool-free by construction: no tool is ever sent.
		executor = nil
	}
	var catalog []model.ToolDefinition
	if executor != nil {
		catalog = executor.Definitions()
	}
	if len(catalog) > 0 && !r.cfg.SupportsTools {
		return "", errors.New("the configured OpenAI-compatible endpoint does not advertise tool support")
	}
	var totals usageTotals
	reportedModel := ""
	defer func() {
		if emit == nil {
			return
		}
		input, output, cached := totals.aggregate()
		body := map[string]any{}
		if input != nil {
			body["inputTokens"] = *input
		}
		if output != nil {
			body["outputTokens"] = *output
		}
		if cached != nil {
			body["cachedInputTokens"] = *cached
		}
		if safeIdentifier(reportedModel) {
			body["model"] = reportedModel
		}
		if safeIdentifier(r.agent.Config.Provider) {
			body["provider"] = r.agent.Config.Provider
		}
		if len(body) == 0 {
			return
		}
		data, err := json.Marshal(body)
		if err != nil {
			return
		}
		emit("usage", string(data))
	}()
	messages := []chatMessage{{Role: "user", Content: turn.Task.Text}}
	calls := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		request := chatRequest{Model: r.agent.Config.Model, Messages: messages, Stream: false}
		if len(catalog) > 0 {
			request.Tools = definitions(catalog)
			request.ToolChoice = "auto"
		}
		reply, err := r.chat(ctx, request)
		if err != nil {
			return "", err
		}
		totals.add(reply.Usage)
		if reply.Model != "" {
			reportedModel = reply.Model
		}
		if emit != nil && reply.Usage != nil && reply.Usage.PromptTokens != nil && r.cfg.ContextWindowTokens > 0 {
			observation, err := json.Marshal(model.ContextObservation{UsedTokens: *reply.Usage.PromptTokens, WindowTokens: int64(r.cfg.ContextWindowTokens), Estimated: true, Source: "OpenAI-compatible endpoint reported usage"})
			if err == nil {
				emit("context_usage", string(observation))
			}
		}
		if len(reply.Choices) == 0 {
			return "", errors.New("OpenAI-compatible response contained no choices")
		}
		choice := reply.Choices[0]
		text := choice.Message.text()
		assistant := chatMessage{Role: "assistant"}
		if text != "" {
			assistant.Content = text
		}
		assistant.ToolCalls = choice.Message.ToolCalls
		messages = append(messages, assistant)
		if len(choice.Message.ToolCalls) == 0 {
			return redact(text, turn.Token), nil
		}
		for _, call := range choice.Message.ToolCalls {
			calls++
			if calls > MaxToolCalls {
				return "", fmt.Errorf("OpenAI-compatible turn exceeded the %d tool call limit", MaxToolCalls)
			}
			// Calls are dispatched one at a time: at most one broker mutation
			// is ever outstanding for this turn.
			message, err := r.dispatch(ctx, executor, catalog, call, emit)
			if err != nil {
				return "", err
			}
			messages = append(messages, message)
		}
	}
}

// dispatch runs one model-requested tool call. An unknown name, invalid
// arguments or an oversized payload never reaches the executor.
func (r *runtime) dispatch(ctx context.Context, executor model.ToolExecutor, catalog []model.ToolDefinition, call toolCall, emit model.Emit) (chatMessage, error) {
	if call.ID == "" || len(call.ID) > 200 || !safeCallID(call.ID) {
		return chatMessage{}, errors.New("OpenAI-compatible tool call is missing a usable identifier")
	}
	if call.Type != "" && call.Type != "function" {
		return toolFailure(call, "unsupported tool call type"), nil
	}
	if len(call.Function.Arguments) > MaxArgumentBytes {
		return toolFailure(call, "tool arguments exceed the 128 KiB bound"), nil
	}
	definition, known := findDefinition(catalog, call.Function.Name)
	if !known {
		// The admitted catalog is the only authority for a tool name.
		return toolFailure(call, "unknown tool: the model requested a tool outside the admitted catalog"), nil
	}
	arguments, err := canonicalArguments(call.Function.Arguments)
	if err != nil {
		return toolFailure(call, "invalid tool arguments: "+err.Error()), nil
	}
	if err := validateArguments(definition, arguments); err != nil {
		return toolFailure(call, "invalid tool arguments: "+err.Error()), nil
	}
	if executor == nil {
		return toolFailure(call, "no tool executor is active for this turn"), nil
	}
	if emit != nil {
		emit("progress", "OpenAI-compatible dispatching tool "+call.Function.Name)
	}
	result, err := executor.Call(ctx, model.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
	if err != nil {
		return toolFailure(call, "tool call failed: "+err.Error()), nil
	}
	content, err := toolContent(result, r.cfg.SupportsImages)
	if err != nil {
		return toolFailure(call, err.Error()), nil
	}
	return chatMessage{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: content}, nil
}

// chatURL is the exact completion URL of the configured endpoint.
func (r *runtime) chatURL() (string, error) {
	if r.cfg.ChatCompletionsURL != "" {
		return endpointURL(r.cfg.ChatCompletionsURL)
	}
	return r.base + "/chat/completions", nil
}

// modelsURL is the exact model list URL of the configured endpoint.
func modelsURL(cfg Config) (string, error) {
	if cfg.ModelsURL != "" {
		return endpointURL(cfg.ModelsURL)
	}
	base, err := NormalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return "", err
	}
	return base + "/models", nil
}

// endpointURL validates an absolute endpoint URL supplied by the caller.
func endpointURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("endpoint URL is missing")
	}
	if len(raw) > MaxURLBytes {
		return "", errors.New("endpoint URL exceeds 2048 bytes")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("endpoint URL is not a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("endpoint URL must use http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("endpoint URL is missing a host")
	}
	if parsed.User != nil {
		return "", errors.New("endpoint URL must not carry credentials")
	}
	if parsed.Fragment != "" {
		return "", errors.New("endpoint URL must not carry a fragment")
	}
	return raw, nil
}

// chat performs exactly one endpoint request. A request whose outcome is
// unknown is never replayed.
func (r *runtime) chat(ctx context.Context, request chatRequest) (chatResponse, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return chatResponse{}, err
	}
	target, err := r.chatURL()
	if err != nil {
		return chatResponse{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return chatResponse{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if r.cfg.APIKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	}
	response, err := r.client.Do(httpRequest)
	if err != nil {
		return chatResponse{}, fmt.Errorf("OpenAI-compatible request failed: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return chatResponse{}, fmt.Errorf("OpenAI-compatible response read: %w", err)
	}
	if len(raw) > MaxResponseBytes {
		return chatResponse{}, errors.New("OpenAI-compatible response exceeds the 8 MiB bound")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return chatResponse{}, fmt.Errorf("OpenAI-compatible endpoint returned HTTP %d: %s", response.StatusCode, r.snippet(raw))
	}
	var reply chatResponse
	if err := json.Unmarshal(raw, &reply); err != nil {
		return chatResponse{}, errors.New("OpenAI-compatible endpoint returned an invalid JSON response")
	}
	if reply.Error != nil && reply.Error.Message != "" {
		return chatResponse{}, errors.New("OpenAI-compatible endpoint error: " + reply.Error.Message)
	}
	return reply, nil
}

// snippet renders a bounded, credential-free provider error.
func (r *runtime) snippet(raw []byte) string {
	text := string(raw)
	if len(text) > MaxErrorBytes {
		text = text[:MaxErrorBytes] + " [truncated]"
	}
	if r.cfg.APIKey != "" {
		text = strings.ReplaceAll(text, r.cfg.APIKey, "[redacted]")
	}
	var builder strings.Builder
	for _, ch := range text {
		if ch < 0x20 && ch != '\n' && ch != '\t' {
			continue
		}
		builder.WriteRune(ch)
	}
	return builder.String()
}

// ListModels reads the model list of a configured endpoint. The caller owns
// the endpoint definition and supplies the origin-pinned client. A stored
// credential is optional: the Authorization header is only sent when one
// exists, so a local endpoint without a secret stays listable.
func ListModels(ctx context.Context, client *http.Client, cfg Config) ([]model.ModelOption, error) {
	target, err := modelsURL(cfg)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = cfg.HTTPClient
	}
	if client == nil {
		client = NewClient()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if cfg.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("OpenAI-compatible model list failed: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxModelsResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("OpenAI-compatible model list read: %w", err)
	}
	if len(raw) > MaxModelsResponseBytes {
		return nil, errors.New("OpenAI-compatible model list exceeds the 2 MiB bound")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("OpenAI-compatible endpoint returned HTTP %d for the model list", response.StatusCode)
	}
	var reply struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, errors.New("OpenAI-compatible endpoint returned an invalid model list")
	}
	out := []model.ModelOption{}
	seen := map[string]bool{}
	add := func(raw json.RawMessage) {
		id, name := modelEntry(raw)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		if name == "" {
			name = id
		}
		out = append(out, model.ModelOption{ID: id, Name: name})
	}
	for _, item := range reply.Data {
		add(item)
	}
	for _, item := range reply.Models {
		add(item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > MaxModels {
		out = out[:MaxModels]
	}
	return out, nil
}

// modelEntry reads one model list entry. An entry is an object with an
// identifier, or a plain string identifier.
func modelEntry(raw json.RawMessage) (id, name string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", ""
	}
	if trimmed[0] == '"' {
		if json.Unmarshal(trimmed, &id) != nil {
			return "", ""
		}
		return id, ""
	}
	var entry struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	if json.Unmarshal(trimmed, &entry) != nil {
		return "", ""
	}
	id = entry.ID
	if id == "" {
		id = entry.Model
	}
	name = entry.Name
	if name == "" {
		name = entry.Model
	}
	return id, name
}

func definitions(catalog []model.ToolDefinition) []toolDefinition {
	out := make([]toolDefinition, 0, len(catalog))
	for _, definition := range catalog {
		if definition.Name == "" {
			continue
		}
		item := toolDefinition{Type: "function"}
		item.Function.Name = definition.Name
		item.Function.Description = definition.Description
		if len(definition.InputSchema) > 0 {
			item.Function.Parameters = definition.InputSchema
		}
		out = append(out, item)
	}
	return out
}

func findDefinition(catalog []model.ToolDefinition, name string) (model.ToolDefinition, bool) {
	if name == "" {
		return model.ToolDefinition{}, false
	}
	for _, definition := range catalog {
		if definition.Name == name {
			return definition, true
		}
	}
	return model.ToolDefinition{}, false
}

// toolFailure reports a refused tool call back to the model. The refused call
// was never executed.
func toolFailure(call toolCall, reason string) chatMessage {
	return chatMessage{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: "error: " + reason}
}

// canonicalArguments parses the provider's argument string into exactly one
// JSON object.
func canonicalArguments(raw string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = "{}"
	}
	if len(trimmed) > MaxArgumentBytes {
		return nil, errors.New("tool arguments exceed the 128 KiB bound")
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	if object == nil {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("tool arguments must contain exactly one JSON object")
	}
	body, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// validateArguments checks the arguments against the tool's declared schema.
// Unknown names, extra fields and wrong types are refused before execution.
func validateArguments(definition model.ToolDefinition, arguments json.RawMessage) error {
	if len(definition.InputSchema) == 0 {
		return nil
	}
	var schema struct {
		Type                 string                     `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return errors.New("tool schema is not a valid JSON schema object")
	}
	if schema.Type != "" && schema.Type != "object" {
		return errors.New("tool schema is not an object schema")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &values); err != nil {
		return errors.New("tool arguments must be a JSON object")
	}
	for _, name := range schema.Required {
		if _, ok := values[name]; !ok {
			return fmt.Errorf("the required field %q is missing", name)
		}
	}
	if schema.AdditionalProperties != nil && !*schema.AdditionalProperties {
		for name := range values {
			if _, ok := schema.Properties[name]; !ok {
				return fmt.Errorf("the field %q is not permitted", name)
			}
		}
	}
	for name, raw := range values {
		property, ok := schema.Properties[name]
		if !ok {
			continue
		}
		var rule struct {
			Type      string   `json:"type"`
			Minimum   *float64 `json:"minimum"`
			Maximum   *float64 `json:"maximum"`
			MinLength *int     `json:"minLength"`
			MaxLength *int     `json:"maxLength"`
		}
		if err := json.Unmarshal(property, &rule); err != nil {
			continue
		}
		if err := checkType(rule.Type, raw); err != nil {
			return fmt.Errorf("the field %q %w", name, err)
		}
		if rule.Type == "string" {
			var text string
			if json.Unmarshal(raw, &text) == nil {
				length := utf8.RuneCountInString(text)
				if rule.MinLength != nil && length < *rule.MinLength {
					return fmt.Errorf("the field %q is shorter than %d characters", name, *rule.MinLength)
				}
				if rule.MaxLength != nil && length > *rule.MaxLength {
					return fmt.Errorf("the field %q is longer than %d characters", name, *rule.MaxLength)
				}
			}
		}
		if rule.Minimum != nil || rule.Maximum != nil {
			number, err := jsonNumber(raw)
			if err != nil {
				return fmt.Errorf("the field %q must be a number", name)
			}
			if rule.Minimum != nil && number < *rule.Minimum {
				return fmt.Errorf("the field %q is below its minimum", name)
			}
			if rule.Maximum != nil && number > *rule.Maximum {
				return fmt.Errorf("the field %q is above its maximum", name)
			}
		}
	}
	return nil
}

func checkType(kind string, raw json.RawMessage) error {
	if kind == "" {
		return nil
	}
	switch kind {
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("must be a string")
		}
	case "boolean":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("must be a boolean")
		}
	case "integer":
		number, err := jsonNumber(raw)
		if err != nil {
			return errors.New("must be an integer")
		}
		if number != float64(int64(number)) {
			return errors.New("must be an integer")
		}
	case "number":
		if _, err := jsonNumber(raw); err != nil {
			return errors.New("must be a number")
		}
	case "array":
		var value []any
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("must be an array")
		}
	case "object":
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("must be an object")
		}
	default:
		return nil
	}
	return nil
}

func jsonNumber(raw json.RawMessage) (float64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return 0, errors.New("not a JSON number")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value json.Number
	if err := decoder.Decode(&value); err != nil {
		return 0, err
	}
	return value.Float64()
}

// allowedImageMIME lists the raster image types that may be delivered as
// typed image content. SVG and markup are never delivered.
var allowedImageMIME = map[string]bool{"image/png": true, "image/jpeg": true, "image/jpg": true, "image/webp": true, "image/gif": true}

// toolContent converts a broker tool result into the content of a tool
// message. Image bytes become typed image content only when the selected
// model declares image support, and the encoded result stays inside the
// process frame bound.
func toolContent(result model.ToolResult, images bool) (any, error) {
	parts := []contentPart{}
	size := 0
	for _, content := range result.Content {
		switch content.Type {
		case "text", "":
			if content.MimeType == "text/html" {
				parts = append(parts, contentPart{Type: "text", Text: "[html tool output withheld: markup is never delivered]"})
				continue
			}
			if content.MimeType != "" && !strings.HasPrefix(content.MimeType, "text/") {
				parts = append(parts, contentPart{Type: "text", Text: "[binary tool output withheld: " + content.MimeType + "]"})
				continue
			}
			text := content.Text
			if len(text) > MaxTextPartBytes {
				text = text[:MaxTextPartBytes] + " [truncated]"
			}
			parts = append(parts, contentPart{Type: "text", Text: text})
			size += len(text)
		case "image":
			if !images {
				parts = append(parts, contentPart{Type: "text", Text: "[image withheld: the selected model does not declare image support]"})
				continue
			}
			mime := strings.ToLower(strings.TrimSpace(content.MimeType))
			if !allowedImageMIME[mime] {
				parts = append(parts, contentPart{Type: "text", Text: "[image withheld: unsupported image type " + mime + "]"})
				continue
			}
			data := strings.TrimSpace(content.Data)
			if data == "" {
				parts = append(parts, contentPart{Type: "text", Text: "[image withheld: the tool returned no image data]"})
				continue
			}
			if _, err := base64.StdEncoding.DecodeString(data); err != nil {
				if _, err := base64.RawStdEncoding.DecodeString(data); err != nil {
					parts = append(parts, contentPart{Type: "text", Text: "[image withheld: the tool returned malformed image data]"})
					continue
				}
			}
			source := "data:" + mime + ";base64," + data
			parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: source}})
			size += len(source)
		default:
			parts = append(parts, contentPart{Type: "text", Text: "[unsupported tool content type withheld: " + content.Type + "]"})
		}
		if size > MaxFrameBytes {
			return nil, errors.New("tool result exceeds the 1 MiB frame bound")
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 && parts[0].Type == "text" {
		return parts[0].Text, nil
	}
	return parts, nil
}

// safeCallID accepts a native tool call identifier. It mirrors the relay's
// registry text rules so an identifier the broker would refuse never reaches
// it.
func safeCallID(value string) bool {
	if value == "" || len(value) > 200 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// safeIdentifier accepts a plain identifier that the relay can persist next to
// its usage accounting. It rejects control characters, credential-shaped text
// and anything the relay's redaction would rewrite.
func safeIdentifier(value string) bool {
	if value == "" || len(value) > 200 || !utf8.ValidString(value) {
		return false
	}
	if strings.Contains(value, "sk-") || strings.Contains(value, "://") {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '/' || r == ':' || r == '+' || r == '@':
		default:
			return false
		}
	}
	return true
}

func redact(text, token string) string {
	if token != "" {
		return strings.ReplaceAll(text, token, "[redacted]")
	}
	return text
}
