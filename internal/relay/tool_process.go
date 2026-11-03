package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
	"meshra/internal/sandbox"
)

type toolProcessArgs struct {
	Operation      string   `json:"operation"`
	ID             string   `json:"id,omitempty"`
	Argv           []string `json:"argv,omitempty"`
	Shell          string   `json:"shell,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
	Offset         int      `json:"offset,omitempty"`
	Limit          int      `json:"limit,omitempty"`
}
type preparedProcessCommand struct{ command *preparedCommand }
type processRecord struct {
	invocation string
	receipt    any
	finalized  bool
}
type brokerProcesses struct {
	mu      sync.Mutex
	manager *sandbox.ProcessManager
	records map[string]*processRecord
}

func validateProcessArgs(q *toolProcessArgs) error {
	switch q.Operation {
	case "start":
		if q.ID != "" || q.Offset != 0 || q.Limit != 0 {
			return model.Err(400, "start accepts command arguments only")
		}
		body, _ := json.Marshal(toolCommandArgs{Argv: q.Argv, Shell: q.Shell, TimeoutSeconds: q.TimeoutSeconds})
		action, err := parseToolAction(model.ToolCall{Name: "meshra_workspace_command", Arguments: body})
		if err != nil {
			return err
		}
		q.TimeoutSeconds = action.args.(*toolCommandArgs).TimeoutSeconds
	case "poll", "kill", "read-log":
		if q.ID == "" || len(q.ID) != 32 || len(q.Argv) != 0 || q.Shell != "" || q.TimeoutSeconds != 0 {
			return model.Err(400, "invalid process operation arguments")
		}
		for _, c := range q.ID {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return model.Err(400, "invalid process ID")
			}
		}
		if q.Operation != "read-log" && (q.Offset != 0 || q.Limit != 0) {
			return model.Err(400, "unexpected log bounds")
		}
		if q.Operation == "read-log" {
			if q.Offset < 0 || q.Limit < 0 || q.Limit > 65536 {
				return model.Err(400, "invalid log bounds")
			}
			if q.Limit == 0 {
				q.Limit = 65536
			}
		}
	default:
		return model.Err(400, "unknown process operation")
	}
	return nil
}

// The enclosing broker call reserves the workspace gate before preparation.
// Reuse the foreground command's exact validation, preview, manifest baseline,
// runtime pinning and denylist rather than creating a weaker execution path.
func (b *toolBroker) prepareProcess(ctx context.Context, call model.ToolCall, state *brokerCall, q *toolProcessArgs) error {
	if q.Operation != "start" {
		return nil
	}
	state.action.args = &toolCommandArgs{Argv: q.Argv, Shell: q.Shell, TimeoutSeconds: q.TimeoutSeconds}
	if err := b.prepareAction(ctx, call, state); err != nil {
		return err
	}
	command := state.action.args.(*preparedCommand)
	state.action.args = &preparedProcessCommand{command: command}
	state.action.preview.Label = "Start background: " + state.action.preview.Label
	return nil
}

func (b *toolBroker) startProcess(state *brokerCall, q *preparedProcessCommand) (model.ToolResult, error) {
	processes := b.processes
	processes.mu.Lock()
	defer processes.mu.Unlock()
	ownerCtx, release, err := b.relay.beginConsoleJob(b.ctx, state.owner)
	if err != nil {
		return model.ToolResult{}, err
	}
	id, done, err := processes.manager.StartContext(ownerCtx, q.command.config, sandbox.Request{Workspace: b.workspace, Argv: q.command.argv, Kind: sandbox.Command, Timeout: time.Duration(q.command.input.TimeoutSeconds) * time.Second, BeforeStart: func() error { return b.claim(state) }})
	if err != nil {
		release()
		return model.ToolResult{}, sandboxToolError(err)
	}
	go func() {
		<-done
		release()
	}()
	processes.records[id] = &processRecord{invocation: state.invocation.ID}
	b.relay.mu.Lock()
	state.processStarted = true
	b.relay.mu.Unlock()
	return toolJSON(map[string]any{"id": id, "status": "running"})
}

func (b *toolBroker) processOperation(ctx context.Context, q *toolProcessArgs) (model.ToolResult, error) {
	processes := b.processes
	processes.mu.Lock()
	defer processes.mu.Unlock()
	record := processes.records[q.ID]
	if record == nil {
		return model.ToolResult{}, model.Err(404, "process not found for this turn")
	}
	if q.Operation == "read-log" {
		data, err := processes.manager.ReadLog(q.ID, q.Offset, q.Limit)
		if err != nil {
			return model.ToolResult{}, sandboxToolError(err)
		}
		value := map[string]any{"id": q.ID, "offset": q.Offset, "nextOffset": q.Offset + len(data), "bytes": len(data)}
		if utf8.Valid(data) {
			value["text"] = redact(string(data))
		} else {
			value["encoding"] = "base64"
			value["data"] = base64.StdEncoding.EncodeToString(data)
		}
		return toolJSON(value)
	}
	if q.Operation == "kill" {
		if err := processes.manager.Kill(q.ID); err != nil {
			return model.ToolResult{}, sandboxToolError(err)
		}
	}
	status, err := processes.manager.Poll(q.ID)
	if err != nil {
		return model.ToolResult{}, sandboxToolError(err)
	}
	if status.Finished && !record.finalized {
		manifest, err := b.workspace.Changes(ctx)
		if err != nil {
			return model.ToolResult{}, sandboxToolError(err)
		}
		changeset, err := b.relay.persistChangeset(ctx, record.invocation, b.snapshot.Profile.WorkspaceID, manifest)
		manifest.Close()
		if err != nil {
			return model.ToolResult{}, err
		}
		record.receipt = changesetReceipt(changeset)
		record.finalized = true
		b.relay.mu.Lock()
		b.processPending = false
		b.relay.mu.Unlock()
	}
	value := map[string]any{"id": q.ID, "status": status, "changeset": record.receipt}
	return toolJSON(value)
}

func processWorkspaceConflict(name string) bool {
	return name == "meshra_workspace_write" || name == "meshra_workspace_command" || name == "meshra_attachment_publish" || name == "meshra_execute_code" || strings.HasPrefix(name, "meshra_browser") || name == "meshra_computer"
}
