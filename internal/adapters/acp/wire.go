package acp

import "encoding/json"

// The ACP v1 wire structures below are implemented independently from the
// published schema. Only the members this client needs are modelled. Unknown
// members are ignored instead of rejected, so a newer agent stays usable while
// an unsupported protocol version is still refused explicitly.

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type clientCapabilities struct {
	FS       *fsCapabilities `json:"fs,omitempty"`
	Terminal bool            `json:"terminal,omitempty"`
}

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         clientInfo         `json:"clientInfo"`
}

type promptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

type mcpCapabilities struct {
	HTTP bool `json:"http,omitempty"`
	SSE  bool `json:"sse,omitempty"`
}

type sessionCapabilities struct {
	Resume                *struct{} `json:"resume,omitempty"`
	Close                 *struct{} `json:"close,omitempty"`
	AdditionalDirectories *struct{} `json:"additionalDirectories,omitempty"`
}

type authCapabilities struct {
	Logout *struct{} `json:"logout,omitempty"`
}

type agentCapabilities struct {
	LoadSession         bool                `json:"loadSession,omitempty"`
	PromptCapabilities  promptCapabilities  `json:"promptCapabilities,omitempty"`
	MCPCapabilities     mcpCapabilities     `json:"mcpCapabilities,omitempty"`
	SessionCapabilities sessionCapabilities `json:"sessionCapabilities,omitempty"`
	Auth                *authCapabilities   `json:"auth,omitempty"`
}

type authMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AgentInfo         *clientInfo       `json:"agentInfo,omitempty"`
	AuthMethods       []authMethod      `json:"authMethods"`
}

type authenticateParams struct {
	MethodID string `json:"methodId"`
}

type envVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type httpHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type mcpServer struct {
	Type    string        `json:"type,omitempty"`
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []envVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []httpHeader  `json:"headers,omitempty"`
}

type sessionParams struct {
	SessionID  string      `json:"sessionId,omitempty"`
	CWD        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

type sessionResult struct {
	SessionID string `json:"sessionId"`
}

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

type sessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	MessageID     string          `json:"messageId,omitempty"`
	Content       *contentBlock   `json:"content,omitempty"`
	ToolCallID    string          `json:"toolCallId,omitempty"`
	Title         string          `json:"title,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	Status        string          `json:"status,omitempty"`
	RawInput      json.RawMessage `json:"rawInput,omitempty"`
}

type updateParams struct {
	SessionID string        `json:"sessionId"`
	Update    sessionUpdate `json:"update"`
}

type permissionToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Name       string          `json:"name,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type permissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  permissionToolCall `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

type readTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

type writeTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}
