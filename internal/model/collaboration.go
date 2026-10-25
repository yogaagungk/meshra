package model

// CollaborationService is additive; v0.1/v0.2 Service implementations stay valid.
type CollaborationService interface {
	Collaborate(Principal, string, CollaborationRequest) (any, error)
}
type ContextRef struct {
	ContextID string `json:"contextId"`
	Version   int    `json:"version"`
}
type CollaborationRequest struct {
	Mode           string       `json:"mode,omitempty"`
	TargetTaskID   string       `json:"targetTaskId,omitempty"`
	From           string       `json:"from,omitempty"`
	To             string       `json:"to,omitempty"`
	Instruction    string       `json:"instruction,omitempty"`
	ContextRefs    []ContextRef `json:"contextRefs,omitempty"`
	DependsOn      []string     `json:"dependsOn,omitempty"`
	TimeoutSeconds int          `json:"timeoutSeconds,omitempty"`
	ParentTaskID   string       `json:"parentTaskId,omitempty"`

	ContextID      string `json:"contextId,omitempty"`
	Version        int    `json:"version,omitempty"`
	Content        string `json:"content,omitempty"`
	Type           string `json:"type,omitempty"`
	Scope          string `json:"scope,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	Keyword        string `json:"keyword,omitempty"`

	ID           string   `json:"id,omitempty"`
	Title        string   `json:"title,omitempty"`
	AgentID      string   `json:"agentId,omitempty"`
	Participants []string `json:"participants,omitempty"`
	After        string   `json:"after,omitempty"`
	Limit        int      `json:"limit,omitempty"`
}
type Conversation struct {
	ProfileKind  string        `json:"profileKind,omitempty"`
	ProfileIDs   []string      `json:"profileIds,omitempty"`
	ChatSettings *ChatSettings `json:"chatSettings,omitempty"`
	Mode         string        `json:"mode,omitempty"`
	ContextRefs  []ContextRef  `json:"contextRefs"`
	ID           string        `json:"conversationId"`
	Title        string        `json:"title"`
	Owner        string        `json:"owner"`
	CreatedAt    string        `json:"createdAt"`
	Participants []string      `json:"participants"`
}

type SharedContext struct {
	ID      string  `json:"contextId"`
	Owner   string  `json:"owner"`
	Title   string  `json:"title"`
	Type    string  `json:"type"`
	Scope   string  `json:"scope"`
	Version int     `json:"version"`
	SHA256  string  `json:"sha256"`
	Size    int     `json:"size"`
	Content *string `json:"content,omitempty"`
}
