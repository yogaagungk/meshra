package acp

import (
	"context"
	"encoding/json"
	"errors"

	"meshra/internal/model"
)

const maxTerminals = 8

type clientTerminal struct {
	cancel    context.CancelFunc
	done      chan struct{}
	output    string
	truncated bool
	exitCode  int
	err       error
}
type terminalCreateParams struct {
	SessionID       string        `json:"sessionId"`
	Command         string        `json:"command"`
	Args            []string      `json:"args,omitempty"`
	Env             []EnvVariable `json:"env,omitempty"`
	CWD             string        `json:"cwd,omitempty"`
	OutputByteLimit *int          `json:"outputByteLimit,omitempty"`
}
type terminalParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

func hasTool(catalog []model.ToolDefinition, name string) bool {
	for _, tool := range catalog {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// Terminal execution is always a broker command, never a process started by
// this adapter. The command worker receives neither native inference secrets
// nor the ACP worker's broker/egress sockets.
func (r *runtime) terminalCreate(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var q terminalCreateParams
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid terminal creation request"}
	}
	if q.SessionID != "" && q.SessionID != r.SessionID() {
		return nil, &rpcError{Code: -32602, Message: "unknown terminal session"}
	}
	if q.Command == "" || len(q.Command) > 4096 || len(q.Args) > 127 || len(q.Env) != 0 || q.CWD != "" && q.CWD != "/workspace" {
		return nil, &rpcError{Code: -32602, Message: "terminal requires an explicit command, private /workspace cwd and no environment overrides"}
	}
	limit := 256 * 1024
	if q.OutputByteLimit != nil {
		limit = *q.OutputByteLimit
		if limit < 1 || limit > 256*1024 {
			return nil, &rpcError{Code: -32602, Message: "terminal output limit must be 1..262144 bytes"}
		}
	}
	executor := r.currentExecutor()
	if executor == nil || !hasTool(executor.Definitions(), "meshra_workspace_command") {
		return nil, &rpcError{Code: -32601, Message: "broker command execution is disabled"}
	}
	argv := make([]string, 0, len(q.Args)+1)
	argv = append(argv, q.Command)
	argv = append(argv, q.Args...)
	arguments, err := json.Marshal(struct {
		Argv []string `json:"argv"`
	}{argv})
	if err != nil || len(arguments) > 64*1024 {
		return nil, &rpcError{Code: -32602, Message: "terminal arguments exceed the command bound"}
	}
	brokerID, failure := requestCallID(ctx)
	if failure != nil {
		return nil, failure
	}
	id := r.callID("terminal")
	child, cancel := context.WithCancel(ctx)
	terminal := &clientTerminal{cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	if r.closed || r.turnCtx == nil || len(r.terminals) >= maxTerminals {
		r.mu.Unlock()
		cancel()
		return nil, &rpcError{Code: -32000, Message: "terminal capacity reached or turn closed"}
	}
	if r.terminals == nil {
		r.terminals = make(map[string]*clientTerminal)
	}
	r.terminals[id] = terminal
	r.mu.Unlock()
	go func() {
		defer cancel()
		result, callErr := executor.Call(child, model.ToolCall{ID: brokerID, Name: "meshra_workspace_command", Arguments: arguments})
		var decoded struct {
			ExitCode  *int   `json:"exitCode"`
			Stdout    string `json:"stdout"`
			Stderr    string `json:"stderr"`
			Truncated bool   `json:"truncated"`
		}
		if callErr == nil {
			valid := false
			for _, content := range result.Content {
				if content.Type == "text" && json.Unmarshal([]byte(content.Text), &decoded) == nil && decoded.ExitCode != nil {
					valid = true
					break
				}
			}
			if !valid || result.IsError && *decoded.ExitCode == 0 {
				callErr = errors.New("broker command was denied or returned no execution result")
			}
		}
		r.mu.Lock()
		if callErr != nil {
			terminal.err = errors.New("broker command failed or was cancelled")
		} else {
			output := decoded.Stdout + decoded.Stderr
			terminal.output = truncate(output, limit)
			terminal.truncated = decoded.Truncated || len(output) > limit
			terminal.exitCode = *decoded.ExitCode
		}
		close(terminal.done)
		r.mu.Unlock()
	}()
	return map[string]any{"terminalId": id}, nil
}

func (r *runtime) terminalRequest(ctx context.Context, method string, raw json.RawMessage) (any, *rpcError) {
	var q terminalParams
	if err := json.Unmarshal(raw, &q); err != nil || q.TerminalID == "" {
		return nil, &rpcError{Code: -32602, Message: "invalid terminal request"}
	}
	if q.SessionID != "" && q.SessionID != r.SessionID() {
		return nil, &rpcError{Code: -32602, Message: "unknown terminal session"}
	}
	r.mu.Lock()
	terminal := r.terminals[q.TerminalID]
	r.mu.Unlock()
	if terminal == nil {
		return nil, &rpcError{Code: -32602, Message: "terminal is not owned by this active turn"}
	}
	switch method {
	case "terminal/kill":
		terminal.cancel()
		return map[string]any{}, nil
	case "terminal/release":
		terminal.cancel()
		select {
		case <-terminal.done:
		case <-ctx.Done():
			return nil, &rpcError{Code: -32000, Message: "terminal release cancelled"}
		}
		r.mu.Lock()
		delete(r.terminals, q.TerminalID)
		r.mu.Unlock()
		return map[string]any{}, nil
	case "terminal/wait_for_exit":
		select {
		case <-terminal.done:
		case <-ctx.Done():
			return nil, &rpcError{Code: -32000, Message: "terminal wait cancelled"}
		}
	case "terminal/output":
	default:
		return nil, &rpcError{Code: -32601, Message: "unsupported terminal method"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-terminal.done:
		if terminal.err != nil {
			return nil, &rpcError{Code: -32000, Message: terminal.err.Error()}
		}
		exit := map[string]any{"exitCode": terminal.exitCode}
		if method == "terminal/wait_for_exit" {
			return exit, nil
		}
		return map[string]any{"output": terminal.output, "truncated": terminal.truncated, "exitStatus": exit}, nil
	default:
		return map[string]any{"output": "", "truncated": false}, nil
	}
}

func (r *runtime) closeTerminals() {
	r.mu.Lock()
	terminals := r.terminals
	r.terminals = nil
	r.mu.Unlock()
	for _, terminal := range terminals {
		terminal.cancel()
	}
	for _, terminal := range terminals {
		<-terminal.done
	}
}
