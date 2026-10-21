package model

import (
	"context"
	"time"
)

type QuotaWindow struct {
	ID          string     `json:"id"`
	Provider    string     `json:"provider"`
	Account     string     `json:"account,omitempty"`
	Label       string     `json:"label"`
	UsedPercent *float64   `json:"usedPercent,omitempty"`
	Remaining   *float64   `json:"remaining,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	ResetsAt    *time.Time `json:"resetsAt,omitempty"`
	ObservedAt  *time.Time `json:"observedAt,omitempty"`
}
type QuotaReader interface {
	Quotas(context.Context) ([]QuotaWindow, error)
}
type QuotaSnapshot struct {
	Harness     string        `json:"harness"`
	Source      string        `json:"source"`
	State       string        `json:"state"`
	Windows     []QuotaWindow `json:"windows"`
	CheckedAt   *time.Time    `json:"checkedAt,omitempty"`
	AttemptedAt *time.Time    `json:"attemptedAt,omitempty"`
	Stale       bool          `json:"stale"`
	Refreshing  bool          `json:"refreshing"`
	Message     string        `json:"message,omitempty"`
}
type ContextObservation struct {
	UsedTokens   int64  `json:"usedTokens"`
	WindowTokens int64  `json:"windowTokens"`
	Estimated    bool   `json:"estimated"`
	Source       string `json:"source"`
}
