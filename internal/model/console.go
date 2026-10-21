package model

// ConsoleService is the local human-operator boundary; it never impersonates a harness.
type ConsoleService interface {
	Console(Principal, string, ConsoleRequest) (any, error)
}
type ConsoleRequest struct {
	Refresh        bool              `json:"refresh,omitempty"`
	ID             string            `json:"id,omitempty"`
	Name           string            `json:"name,omitempty"`
	WorkspaceID    string            `json:"workspaceId,omitempty"`
	Config         Config            `json:"config,omitempty"`
	Settings       *PersonalSettings `json:"settings,omitempty"`
	Harness        string            `json:"harness,omitempty"`
	Title          string            `json:"title,omitempty"`
	Path           string            `json:"path,omitempty"`
	AfterName      string            `json:"afterName,omitempty"`
	TaskID         string            `json:"taskId,omitempty"`
	ConversationID string            `json:"conversationId,omitempty"`
	Text           string            `json:"text,omitempty"`
	Recipients     []string          `json:"recipients,omitempty"`
	ContextRefs    []ContextRef      `json:"contextRefs,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
	Before         int64             `json:"before,omitempty"`
	After          int64             `json:"after,omitempty"`
	Limit          int               `json:"limit,omitempty"`
	Keyword        string            `json:"keyword,omitempty"`
	Status         string            `json:"status,omitempty"`
}
type ConsoleTask struct {
	Seq int64 `json:"seq"`
	Task
}
type ConsolePage struct {
	Items      []ConsoleTask `json:"items"`
	NextBefore int64         `json:"nextBefore"`
	HasMore    bool          `json:"hasMore"`
}
