package acp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"meshra/internal/model"
)

type terminalTestExecutor struct {
	call func(context.Context, model.ToolCall) (model.ToolResult, error)
}

func (terminalTestExecutor) Definitions() []model.ToolDefinition {
	return []model.ToolDefinition{{Name: "meshra_workspace_command"}}
}
func (e terminalTestExecutor) Call(ctx context.Context, call model.ToolCall) (model.ToolResult, error) {
	return e.call(ctx, call)
}

func TestACPTerminalBoundsOutputAndReleasesOwnership(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	executor := terminalTestExecutor{call: func(context.Context, model.ToolCall) (model.ToolResult, error) {
		return model.ToolResult{IsError: true, Content: []model.ToolContent{{Type: "text", Text: `{"exitCode":7,"stdout":"alpha","stderr":"omega","truncated":false}`}}}, nil
	}}
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "terminal-scope"}, nil
		case "session/prompt":
			created := p.request(t, "terminal/create", map[string]any{"sessionId": "terminal-scope", "command": "/usr/bin/false", "outputByteLimit": 5})
			var terminal struct {
				ID string `json:"terminalId"`
			}
			if created.Error != nil || json.Unmarshal(created.Result, &terminal) != nil || terminal.ID == "" {
				t.Errorf("terminal creation failed: %+v", created)
				return map[string]any{"stopReason": "end_turn"}, nil
			}
			waited := p.request(t, "terminal/wait_for_exit", map[string]any{"sessionId": "terminal-scope", "terminalId": terminal.ID})
			var exit struct {
				ExitCode int `json:"exitCode"`
			}
			if waited.Error != nil || json.Unmarshal(waited.Result, &exit) != nil || exit.ExitCode != 7 {
				t.Errorf("nonzero exit was not retained: %+v", waited)
			}
			output := p.request(t, "terminal/output", map[string]any{"sessionId": "terminal-scope", "terminalId": terminal.ID})
			var snapshot struct {
				Output    string `json:"output"`
				Truncated bool   `json:"truncated"`
			}
			if output.Error != nil || json.Unmarshal(output.Result, &snapshot) != nil || snapshot.Output != "alpha" || !snapshot.Truncated {
				t.Errorf("terminal output bound was not applied: %+v", output)
			}
			released := p.request(t, "terminal/release", map[string]any{"sessionId": "terminal-scope", "terminalId": terminal.ID})
			if released.Error != nil {
				t.Errorf("terminal release failed: %+v", released.Error)
			}
			stale := p.request(t, "terminal/output", map[string]any{"sessionId": "terminal-scope", "terminalId": terminal.ID})
			if stale.Error == nil || stale.Error.Code != -32602 {
				t.Errorf("released terminal retained ownership: %+v", stale)
			}
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	native, _ := startRuntime(t, peer, client, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := native.Run(ctx, model.Turn{Task: model.Task{Text: "exercise terminal lifetime"}, Tools: executor}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestACPTerminalKillCancelsPendingBrokerCommand(t *testing.T) {
	peer, client := newFakePeer()
	defer peer.transport.Close()
	started, cancelled := make(chan struct{}), make(chan struct{})
	executor := terminalTestExecutor{call: func(ctx context.Context, _ model.ToolCall) (model.ToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return model.ToolResult{}, ctx.Err()
	}}
	peer.handler = func(p *fakePeer, request peerRequest) (any, *rpcError) {
		switch request.Method {
		case "initialize":
			return initializePayload(ProtocolVersion), nil
		case "session/new":
			return map[string]any{"sessionId": "cancel-scope"}, nil
		case "session/prompt":
			created := p.request(t, "terminal/create", map[string]any{"sessionId": "cancel-scope", "command": "/usr/bin/true"})
			var terminal struct {
				ID string `json:"terminalId"`
			}
			if created.Error != nil || json.Unmarshal(created.Result, &terminal) != nil || terminal.ID == "" {
				t.Errorf("terminal creation failed: %+v", created)
				return map[string]any{"stopReason": "end_turn"}, nil
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Error("broker command did not start")
			}
			killed := p.request(t, "terminal/kill", map[string]any{"sessionId": "cancel-scope", "terminalId": terminal.ID})
			if killed.Error != nil {
				t.Errorf("terminal cancellation failed: %+v", killed)
			}
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Error("terminal cancellation did not reach pending command")
			}
			waited := p.request(t, "terminal/wait_for_exit", map[string]any{"sessionId": "cancel-scope", "terminalId": terminal.ID})
			if waited.Error == nil {
				t.Error("cancelled terminal reported successful execution")
			}
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unsupported method"}
	}
	native, _ := startRuntime(t, peer, client, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := native.Run(ctx, model.Turn{Task: model.Task{Text: "cancel pending terminal"}, Tools: executor}, nil); err != nil {
		t.Fatal(err)
	}
}
