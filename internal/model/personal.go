package model

import (
	"context"
	"time"
)

type PersonalSettings struct {
	DisplayName      string `json:"displayName"`
	MaxParallelTurns int    `json:"maxParallelTurns"`
	MaxStartedAgents int    `json:"maxStartedAgents"`
	IdleMinutes      int    `json:"idleMinutes"`
	DailyTurnLimit   int    `json:"dailyTurnLimit"`
}
type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"createdAt"`
}
type ChatSettings struct {
	WorkspaceID string `json:"workspaceId"`
	Config      Config `json:"config"`
}
type ModelOption struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
}

// Catalog discovery never generates a model response.
type ModelCatalog interface {
	Models(context.Context) ([]ModelOption, error)
}
