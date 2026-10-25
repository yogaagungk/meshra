package externalmcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MaxResultBytes bounds a single tool result that may reach a model.
const MaxResultBytes = 1 << 20

// MaxToolCount bounds the number of tools one server may advertise.
const MaxToolCount = 256

// Tool is the stable description of one advertised external tool. The digest
// covers the exact definition, so a changed definition needs a renewed
// binding review.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Digest      string          `json:"digest"`
}

// Content is one bounded piece of an external tool result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

// Result is the bounded outcome of one external tool call.
type Result struct {
	Content    []Content      `json:"content"`
	IsError    bool           `json:"isError"`
	Structured json.RawMessage `json:"structuredContent,omitempty"`
}

// Process is a launched stdio server. The daemon never exposes a host process
// to a transport helper: only these two pipes cross the boundary.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Close() error
}

// Launcher starts a stdio server. The production launcher runs it inside the
// confined read-only worker.
type Launcher interface {
	Launch(ctx context.Context, config Config, env map[string]string) (Process, error)
}

// Options configures one session with an external server.
type Options struct {
	Config     Config
	HTTPClient *http.Client
	// Headers carries operator-provided header values resolved from secret
	// references. They are attached to the exact configured origin only.
	Headers map[string]string
	// BearerToken carries the OAuth access token for an http server.
	BearerToken string
	// Launcher starts a stdio server. It is required for stdio transport.
	Launcher Launcher
	// Env carries the explicitly selected environment of a stdio server.
	Env map[string]string
	// Timeout bounds one session's lifetime.
	Timeout time.Duration
}

// Session is a live connection to one external MCP server.
type Session struct {
	client   *mcp.Client
	session  *mcp.ClientSession
	closer   func() error
	tools    []Tool
	toolCall int
	cancel   context.CancelFunc
	ctx      context.Context
}

// Open connects to an external MCP server and completes initialization.
func Open(ctx context.Context, options Options) (*Session, error) {
	if err := options.Config.Validate(); err != nil {
		return nil, err
	}
	cancel := func() {}
	if options.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, options.Timeout)
	}
	connected := false
	defer func() { if !connected { cancel() } }()
	client := mcp.NewClient(&mcp.Implementation{Name: "meshra", Version: "0.4.0"}, nil)
	var transport mcp.Transport
	closer := func() error { return nil }
	switch options.Config.Transport {
	case TransportStdio:
		if options.Launcher == nil {
			return nil, errors.New("a stdio server requires a confined launcher")
		}
		process, err := options.Launcher.Launch(ctx, options.Config, options.Env)
		if err != nil {
			return nil, err
		}
		closer = process.Close
		transport = &mcp.IOTransport{Reader: process.Stdout(), Writer: process.Stdin()}
	case TransportHTTP:
		if options.HTTPClient == nil {
			return nil, errors.New("an http server requires a policy-scoped client")
		}
		transport = &mcp.StreamableClientTransport{
			Endpoint:   options.Config.URL,
			HTTPClient: ScopedClient(options.HTTPClient, options.Headers, options.BearerToken),
			// A failed request is surfaced, never replayed by the transport.
			MaxRetries: -1,
		}
	default:
		return nil, errors.New("unsupported transport")
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		_ = closer()
		return nil, fmt.Errorf("external MCP session failed: %w", err)
	}
	connected = true
	return &Session{client: client, session: session, closer: closer, cancel: cancel, ctx: ctx}, nil
}

// ScopedClient wraps a policy-scoped HTTP client so that operator-provided
// headers and the OAuth bearer token are attached to the configured origin
// only. The underlying client already refuses any other destination.
func ScopedClient(client *http.Client, headers map[string]string, bearer string) *http.Client {
	if len(headers) == 0 && bearer == "" {
		return client
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{
		Transport: &headerTransport{base: base, headers: headers, bearer: bearer},
		Timeout:   client.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// headerTransport attaches reviewed credentials to every request. The policy
// client below it decides which origin may be reached at all.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
	bearer  string
}

func (t *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	for name, value := range t.headers {
		cloned.Header.Set(name, value)
	}
	if t.bearer != "" {
		cloned.Header.Set("Authorization", "Bearer "+t.bearer)
	}
	return t.base.RoundTrip(cloned)
}

// Tools lists the exact tool definitions the server advertises.
func (s *Session) Tools(ctx context.Context) ([]Tool, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil { return nil, err }
	if s.tools != nil {
		return s.tools, nil
	}
	result, err := s.session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, fmt.Errorf("external MCP tool listing failed: %w", err)
	}
	if len(result.Tools) > MaxToolCount {
		return nil, fmt.Errorf("external MCP server advertises more than %d tools", MaxToolCount)
	}
	tools := make([]Tool, 0, len(result.Tools))
	for _, advertised := range result.Tools {
		tool, err := describeTool(advertised)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	s.tools = tools
	return tools, nil
}

// describeTool converts an advertised definition into its stable description.
func describeTool(advertised *mcp.Tool) (Tool, error) {
	if advertised == nil || advertised.Name == "" {
		return Tool{}, errors.New("external MCP server advertised a tool without a name")
	}
	if !SafeText(advertised.Name) || len(advertised.Name) > 200 {
		return Tool{}, errors.New("external MCP server advertised a tool with an invalid name")
	}
	schema := json.RawMessage(nil)
	if advertised.InputSchema != nil {
		encoded, err := json.Marshal(advertised.InputSchema)
		if err != nil {
			return Tool{}, errors.New("external MCP tool schema is not serializable")
		}
		schema = encoded
	}
	if len(schema) > 64<<10 {
		return Tool{}, errors.New("external MCP tool schema is too large")
	}
	tool := Tool{Name: advertised.Name, Description: advertised.Description, InputSchema: schema}
	definition, err := json.Marshal(advertised)
	if err != nil { return Tool{}, errors.New("external MCP tool definition is not serializable") }
	if len(definition) > 128<<10 { return Tool{}, errors.New("external MCP tool definition is too large") }
	sum := sha256.Sum256(definition)
	tool.Digest = hex.EncodeToString(sum[:])
	return tool, nil
}

// DigestTool hashes the exact definition of one tool.
func DigestTool(tool Tool) string {
	body, _ := json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}{tool.Name, tool.Description, tool.InputSchema})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Call invokes one exact tool and returns a bounded result.
func (s *Session) Call(ctx context.Context, name string, arguments json.RawMessage) (*Result, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil { return nil, err }
	if name == "" || !SafeText(name) {
		return nil, errors.New("tool name is required")
	}
	s.toolCall++
	if s.toolCall > MaxToolCount {
		return nil, fmt.Errorf("external MCP server exceeded %d calls in one session", MaxToolCount)
	}
	var payload any
	if len(strings.TrimSpace(string(arguments))) > 0 {
		decoder := json.NewDecoder(strings.NewReader(string(arguments)))
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			return nil, errors.New("tool arguments must be one JSON value")
		}
	}
	result, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: payload})
	if err != nil {
		return nil, fmt.Errorf("external MCP tool call failed: %w", err)
	}
	return boundResult(result)
}

func (s *Session) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	if s.ctx.Err() != nil { cancel() }
	return child, func() { stop(); cancel() }
}

// boundResult converts and truncates a tool result to the model-facing shape.
func boundResult(result *mcp.CallToolResult) (*Result, error) {
	if result == nil {
		return nil, errors.New("external MCP tool returned no result")
	}
	out := &Result{IsError: result.IsError}
	remaining := MaxResultBytes
	for _, content := range result.Content {
		if remaining <= 0 {
			break
		}
		switch typed := content.(type) {
		case *mcp.TextContent:
			text := typed.Text
			if len(text) > remaining {
				text = text[:remaining]
			}
			remaining -= len(text)
			out.Content = append(out.Content, Content{Type: "text", Text: text})
		case *mcp.ImageContent:
			encoded := base64Data(typed.Data)
			if len(encoded) > remaining {
				break
			}
			remaining -= len(encoded)
			out.Content = append(out.Content, Content{Type: "image", MimeType: typed.MIMEType, Data: encoded})
		case *mcp.AudioContent:
			encoded := base64Data(typed.Data)
			if len(encoded) > remaining {
				break
			}
			remaining -= len(encoded)
			out.Content = append(out.Content, Content{Type: "audio", MimeType: typed.MIMEType, Data: encoded})
		case *mcp.EmbeddedResource:
			text := ""
			mime := ""
			if typed.Resource != nil {
				text = typed.Resource.Text
				mime = typed.Resource.MIMEType
				if text == "" {
					text = base64Data(typed.Resource.Blob)
				}
			}
			if len(text) > remaining {
				text = text[:remaining]
			}
			remaining -= len(text)
			out.Content = append(out.Content, Content{Type: "resource", MimeType: mime, Text: text})
		default:
			encoded, err := json.Marshal(content)
			if err != nil {
				continue
			}
			text := string(encoded)
			if len(text) > remaining {
				text = text[:remaining]
			}
			remaining -= len(text)
			out.Content = append(out.Content, Content{Type: "text", Text: text})
		}
	}
	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		if err == nil && len(encoded) <= remaining {
			out.Structured = encoded
		}
	}
	return out, nil
}

// base64Data encodes binary content without exceeding the result bound.
func base64Data(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if len(data) > MaxResultBytes/2 {
		data = data[:MaxResultBytes/2]
	}
	return base64.StdEncoding.EncodeToString(data)
}

// Close terminates the session and its process or connection.
func (s *Session) Close() error {
	if s.cancel != nil { s.cancel() }
	var first error
	if s.session != nil {
		if err := s.session.Close(); err != nil {
			first = err
		}
	}
	if s.closer != nil {
		if err := s.closer(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
