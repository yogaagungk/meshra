package model

import (
	"context"
	"time"
)

const SchemaVersion = 1
const MaxTextBytes = 64 * 1024

type Policy struct {
	Destinations []string `json:"destinations"`
	Operations   []string `json:"operations"`
	Delegate     bool     `json:"delegate"`
}
type Config struct {
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
}
type CreateAgent struct {
	Harness     string `json:"harness"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Cwd         string `json:"cwd"`
	Config      Config `json:"config"`
	Policy      Policy `json:"policy"`
	Concurrency int    `json:"concurrency,omitempty"`
}
type Agent struct {
	SchemaVersion int             `json:"schemaVersion"`
	ID            string          `json:"id"`
	Harness       string          `json:"harness"`
	Name          string          `json:"name"`
	DisplayName   string          `json:"displayName"`
	Cwd           string          `json:"cwd"`
	Config        Config          `json:"config"`
	Policy        Policy          `json:"policy"`
	Concurrency   int             `json:"concurrency"`
	SessionID     string          `json:"sessionId,omitempty"`
	State         string          `json:"state"`
	LastSeen      time.Time       `json:"lastSeen"`
	Error         string          `json:"error,omitempty"`
	Capabilities  map[string]bool `json:"capabilities"`
}
type Principal struct {
	Admin     bool
	AgentID   string
	SessionID string
	TaskID    string
}
type SendRequest struct {
	From           string `json:"from,omitempty"`
	To             string `json:"to"`
	Text           string `json:"text"`
	Type           string `json:"type,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	ParentTaskID   string `json:"parentTaskId,omitempty"`
	MaxHops        int    `json:"maxHops,omitempty"`
}
type Task struct {
	SchemaVersion        int       `json:"schemaVersion"`
	ID                   string    `json:"taskId"`
	MessageID            string    `json:"messageId"`
	CorrelationID        string    `json:"correlationId"`
	ParentTaskID         string    `json:"parentTaskId,omitempty"`
	OriginAgentID        string    `json:"originAgentId"`
	OriginSessionID      string    `json:"originSessionId"`
	From                 string    `json:"from"`
	FromSessionID        string    `json:"fromSessionId"`
	To                   string    `json:"to"`
	DestinationSessionID string    `json:"destinationSessionId,omitempty"`
	HopCount             int       `json:"hopCount"`
	MaxHops              int       `json:"maxHops"`
	Type                 string    `json:"type"`
	Text                 string    `json:"text"`
	State                string    `json:"state"`
	Result               string    `json:"result,omitempty"`
	Error                string    `json:"error,omitempty"`
	IdempotencyKey       string    `json:"idempotencyKey,omitempty"`
	Attempts             int       `json:"attempts"`
	CreatedAt            time.Time `json:"createdAt"`
	Deadline             time.Time `json:"deadline"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

func Terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled" || state == "timed_out"
}

type Event struct {
	ID     int64     `json:"id"`
	TaskID string    `json:"taskId"`
	Type   string    `json:"type"`
	Data   string    `json:"data"`
	Time   time.Time `json:"time"`
}
type Emit func(kind, data string)
type Turn struct {
	Task   Task
	MCPURL string
	Token  string
}
type Runtime interface {
	SessionID() string
	Run(context.Context, Turn, Emit) (string, error)
	Close() error
}
type Factory func(context.Context, Agent, Emit) (Runtime, error)
type Adapter struct {
	Factory      Factory
	Capabilities map[string]bool
}

// Service is the shared authenticated application boundary used by HTTP and MCP.
type Service interface {
	Authenticate(string) (Principal, error)
	Create(Principal, CreateAgent) (Agent, error)
	Agents(Principal) ([]Agent, error)
	Agent(Principal, string) (Agent, error)
	Action(Principal, string, string) (Agent, error)
	Remove(Principal, string) error
	Token(Principal, string) (string, error)
	Send(Principal, SendRequest) (Task, error)
	Task(Principal, string) (Task, error)
	Events(Principal, string, int64) ([]Event, error)
	Cancel(Principal, string) (Task, error)
	Retry(Principal, string) (Task, error)
}
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string             { return e.Message }
func Err(status int, message string) error { return &Error{status, message} }
