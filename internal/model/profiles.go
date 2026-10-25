package model

import "encoding/json"

// ProfileCapabilities describe explicit opt-ins, not native harness permissions.
type ProfileCapabilities struct {
	WorkspaceRead  bool `json:"workspaceRead"`
	WorkspaceWrite bool `json:"workspaceWrite"`
	Command        bool `json:"command"`
	Browser        bool `json:"browser"`
	Computer       bool `json:"computer"`
	MemoryWrite    bool `json:"memoryWrite"`
	ExternalMCP    bool `json:"externalMcp"`
	Attachments    bool `json:"attachments"`
}
type ProfileProvider struct {
	Harness      string `json:"harness"`
	Config       Config `json:"config"`
	ConnectionID string `json:"connectionId,omitempty"`
}
type Profile struct {
	ID                   string              `json:"id"`
	Revision             int                 `json:"revision"`
	Name                 string              `json:"name"`
	Instructions         string              `json:"instructions"`
	AvatarAttachmentID   string              `json:"avatarAttachmentId,omitempty"`
	WorkspaceID          string              `json:"workspaceId"`
	Binding              ProfileProvider     `json:"binding"`
	Capabilities         ProfileCapabilities `json:"capabilities"`
	AllowedProfileIDs    []string            `json:"allowedProfileIds"`
	NotificationsEnabled bool                `json:"notificationsEnabled"`
	Archived             bool                `json:"archived"`
}
type ProfileBinding struct {
	ID             string `json:"id"`
	ProfileID      string `json:"profileId"`
	ConversationID string `json:"conversationId"`
	Generation     int    `json:"generation"`
	AgentID        string `json:"agentId"`
	Harness        string `json:"harness"`
	Config         Config `json:"config"`
	ConnectionID   string `json:"connectionId,omitempty"`
	// Monotonic audience classification; source receipt retention cannot widen it.
	ExternalConversation bool   `json:"externalConversation"`
	ToolCatalogDigest    string `json:"toolCatalogDigest"`
}
type ProfileTaskSnapshot struct {
	Connection          json.RawMessage        `json:"connection,omitempty"`
	SecretVersions      map[string]string      `json:"secretVersions,omitempty"`
	ConsoleSessionOwner string                 `json:"consoleSessionOwner,omitempty"`
	ToolPolicyRevision  int                    `json:"toolPolicyRevision,omitempty"`
	ToolDefinitions     []ToolDefinition       `json:"toolDefinitions,omitempty"`
	ExternalMCP         []ExternalMCPReference `json:"externalMcp,omitempty"`
	Skills              []SkillReference       `json:"skills,omitempty"`
	External            bool                   `json:"external,omitempty"`
	Profile             Profile                `json:"profile"`
	Binding             ProfileBinding         `json:"binding"`
}
type AutomationPage[T any] struct {
	Items     []T    `json:"items"`
	NextAfter string `json:"nextAfter,omitempty"`
	HasMore   bool   `json:"hasMore"`
}
