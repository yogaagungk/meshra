package model

import (
	"context"
	"encoding/json"
)

// AutomationService is optional; legacy services retain their original contract.
type AutomationService interface {
	Automation(context.Context, Principal, string, json.RawMessage) (any, error)
}

// ConsoleSessionService binds human-only decisions to the transport's live store.
type ConsoleSessionService interface {
	SetConsoleSessionValidator(func(string) bool)
	RevokeConsoleSession(string)
}

// ExternalMCPReference pins one reviewed external tool at task admission.
// Digests cover the complete server and binding, not model-provided values.
type ExternalMCPReference struct {
	ServerID       string `json:"serverId"`
	ServerRevision int    `json:"serverRevision"`
	ServerDigest   string `json:"serverDigest"`
	BindingDigest  string `json:"bindingDigest"`
	Tool           string `json:"tool"`
	ToolDigest     string `json:"toolDigest"`
	AuthorizationID string `json:"authorizationId,omitempty"`
	SecretVersions map[string]string `json:"secretVersions,omitempty"`
}
