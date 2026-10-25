package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"meshra/internal/model"
	"meshra/internal/sandbox"
	"meshra/internal/toolmcp"
)

const maxToolArguments = 128 * 1024

type toolPreview struct {
	Label        string          `json:"label"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	ManifestHash string          `json:"manifestHash,omitempty"`
	Diff         string          `json:"diff,omitempty"`
	Truncated    bool            `json:"truncated,omitempty"`
}
type toolInvocation struct {
	ID             string            `json:"id"`
	TaskID         string            `json:"taskId"`
	Attempt        int               `json:"attempt"`
	CallID         string            `json:"callId"`
	Name           string            `json:"name"`
	ActionHash     string            `json:"actionHash"`
	State          string            `json:"state"`
	ProfileID      string            `json:"profileId"`
	BindingID      string            `json:"bindingId"`
	WorkspaceID    string            `json:"workspaceId"`
	PolicyRevision int               `json:"policyRevision"`
	Epoch          int               `json:"epoch"`
	CreatedAt      time.Time         `json:"createdAt"`
	Preview        toolPreview       `json:"preview"`
	Error          string            `json:"error,omitempty"`
	Result         *model.ToolResult `json:"result,omitempty"`
}
type brokerCall struct {
	requestHash string
	invocation  toolInvocation
	action      *toolAction
	wake        chan struct{}
	cancel      context.CancelFunc
	owner       string
	approvalID  string
	answer      string
	done        bool
	externalDispatched bool
	humanApproved bool
	result      model.ToolResult
	err         error
}
type toolAction struct {
	args            any
	preview         toolPreview
	mutation        bool
	approval        bool
	question        bool
	manifest        *sandbox.Manifest
	runtimeRevision int
	cleanup         func()
}
type toolBroker struct {
	relay       *Relay
	taskID      string
	attempt     int
	slot        *slot
	snapshot    model.ProfileTaskSnapshot
	definitions []model.ToolDefinition
	ctx         context.Context
	cancel      context.CancelFunc
	calls       map[string]*brokerCall
	mutation    string
	closed      bool
	callsWG     sync.WaitGroup
	workspace   *sandbox.Snapshot
	socketMu    sync.Mutex
	socket      *activeToolSocket
	mcpHandler  *toolmcp.Handler
}

func (r *Relay) newToolBrokerLocked(ctx context.Context, t model.Task, s *slot) (*toolBroker, error) {
	snapshot, err := r.profileSnapshot(t)
	if err != nil {
		return nil, err
	}
	if snapshot.Binding.AgentID != t.To || snapshot.Binding.ConversationID != t.ConversationID {
		return nil, model.Err(403, "task snapshot binding mismatch")
	}
	if old := r.toolBrokers[t.ID]; old != nil {
		return nil, model.Err(409, "active tool broker already exists")
	}
	child, cancel := context.WithCancel(ctx)
	b := &toolBroker{relay: r, taskID: t.ID, attempt: t.Attempts, slot: s, snapshot: snapshot, definitions: snapshot.ToolDefinitions, ctx: child, cancel: cancel, calls: make(map[string]*brokerCall)}
	if snapshot.ToolPolicyRevision == 0 {
		b.definitions = nil
	}
	if toolCatalogDigest(b.definitions) != snapshot.Binding.ToolCatalogDigest {
		cancel()
		return nil, model.Err(409, "admitted tool catalog is unavailable; create a new profile request")
	}
	if r.toolBrokers == nil {
		r.toolBrokers = make(map[string]*toolBroker)
	}
	r.toolBrokers[t.ID] = b
	return b, nil
}
func (b *toolBroker) Definitions() []model.ToolDefinition { return b.definitions }

func (b *toolBroker) activeLocked() (model.Task, model.Principal, int, error) {
	return b.activeAttemptLocked(true)
}

// Only trusted transport allocation may precede the native session event.
// Every protocol request and tool invocation still requires activeLocked.
func (b *toolBroker) activeAttemptLocked(requireNative bool) (model.Task, model.Principal, int, error) {
	var principal model.Principal
	t, err := b.relay.task(b.taskID)
	if err != nil || b.closed || b.ctx.Err() != nil || b.relay.closed || t.State != "running" || t.Attempts != b.attempt || !t.Deadline.After(now()) || b.relay.slots[t.To] != b.slot || b.slot.taskID != t.ID || b.slot.runtime == nil {
		return t, principal, 0, model.Err(403, "tool attempt is no longer active")
	}
	if requireNative && (t.DestinationSessionID == "" || b.slot.runtime.SessionID() != t.DestinationSessionID) {
		return t, principal, 0, model.Err(409, "native session allocation has not completed")
	}
	if err = b.relay.currentProfileAgent(t.To); err != nil {
		return t, principal, 0, err
	}
	if err = b.relay.member(model.Principal{AgentID: t.To}, t.ConversationID, false); err != nil {
		return t, principal, 0, err
	}
	var revision, epoch int
	if err = b.relay.db.QueryRow("SELECT revision FROM profile_tool_policies WHERE profile_id=?", b.snapshot.Profile.ID).Scan(&revision); err != nil {
		return t, principal, 0, err
	}
	if revision != b.snapshot.ToolPolicyRevision {
		return t, principal, 0, model.Err(409, "tool policy changed; this admitted catalog is stale")
	}
	if err = b.relay.db.QueryRow("SELECT epoch FROM profile_context_epochs WHERE binding_id=?", b.snapshot.Binding.ID).Scan(&epoch); err != nil {
		return t, principal, 0, err
	}
	principal = model.Principal{AgentID: t.To, SessionID: t.DestinationSessionID, TaskID: t.ID}
	return t, principal, epoch, nil
}

func canonicalToolArguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > maxToolArguments {
		return nil, model.Err(413, "tool arguments exceed 128 KiB")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, model.Err(400, "tool arguments must be a JSON object")
	}
	if object == nil {
		return nil, model.Err(400, "tool arguments must be an object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, model.Err(400, "tool arguments must contain one object")
	}
	body, err := json.Marshal(object)
	return body, err
}

func (b *toolBroker) Call(ctx context.Context, call model.ToolCall) (model.ToolResult, error) {
	var zero model.ToolResult
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if call.ID == "" || len(call.ID) > 200 || !safeRegistryText(call.ID) || redact(call.ID) != call.ID {
		return zero, model.Err(400, "invalid native tool call identifier")
	}
	allowed := false
	for _, definition := range b.definitions {
		if definition.Name == call.Name {
			allowed = true
			break
		}
	}
	if !allowed {
		return zero, model.Err(403, "tool is not in this admitted catalog")
	}
	arguments, err := canonicalToolArguments(call.Arguments)
	if err != nil {
		return zero, err
	}
	call.Arguments = arguments
	action, err := parseToolAction(call)
	if err != nil {
		return zero, err
	}
	requestHash := tokenHash(call.Name + "\x00" + string(arguments))
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer stop()
	defer cancel()
	r := b.relay
	r.mu.Lock()
	t, _, epoch, err := b.activeLocked()
	if err != nil {
		r.mu.Unlock()
		return zero, err
	}
	if old := b.calls[call.ID]; old != nil {
		if old.requestHash != requestHash {
			r.mu.Unlock()
			return zero, model.Err(409, "native call identifier reused with different arguments")
		}
		if !old.done {
			r.mu.Unlock()
			return zero, model.Err(409, "native call is already pending; it will not be repeated")
		}
		result, oldErr := old.result, old.err
		r.mu.Unlock()
		return result, oldErr
	}
	if len(b.calls) >= 40 {
		r.mu.Unlock()
		return zero, model.Err(429, "turn tool-call capacity reached")
	}
	if action.mutation && b.mutation != "" {
		r.mu.Unlock()
		return zero, model.Err(409, "another mutation is outstanding for this turn")
	}
	invocation := toolInvocation{ID: randomID(), TaskID: t.ID, Attempt: t.Attempts, CallID: call.ID, Name: call.Name, State: "pending", ProfileID: b.snapshot.Profile.ID, BindingID: b.snapshot.Binding.ID, WorkspaceID: b.snapshot.Profile.WorkspaceID, PolicyRevision: b.snapshot.ToolPolicyRevision, Epoch: epoch, CreatedAt: now()}
	state := &brokerCall{requestHash: requestHash, invocation: invocation, action: action, wake: make(chan struct{}, 1), cancel: cancel}
	if action.approval || action.question {
		state.owner = b.snapshot.ConsoleSessionOwner
	}
	b.calls[call.ID] = state
	if action.mutation {
		b.mutation = invocation.ID
	}
	b.callsWG.Add(1)
	r.mu.Unlock()
	defer b.callsWG.Done()
	defer func() {
		r.mu.Lock()
		if b.mutation == invocation.ID {
			b.mutation = ""
		}
		r.mu.Unlock()
	}()
	defer func() {
		if action.cleanup != nil {
			action.cleanup()
		}
	}()
	if err = b.prepareAction(child, call, state); err != nil {
		b.finishCall(state, zero, err)
		return zero, err
	}
	invocation.Preview = state.action.preview
	hashBody, _ := json.Marshal(struct {
		Task                           string
		Attempt                        int
		Call, Name                     string
		Arguments                      json.RawMessage
		Workspace, Binding             string
		Policy, Epoch, RuntimeRevision int
		Manifest                       string
		ExternalMCP                    []model.ExternalMCPReference
	}{t.ID, t.Attempts, call.ID, call.Name, arguments, invocation.WorkspaceID, invocation.BindingID, invocation.PolicyRevision, epoch, action.runtimeRevision, invocation.Preview.ManifestHash, b.snapshot.ExternalMCP})
	invocation.ActionHash = tokenHash(string(hashBody))
	r.mu.Lock()
	state.invocation = invocation
	_, _, currentEpoch, activeErr := b.activeLocked()
	if activeErr == nil && currentEpoch != epoch {
		activeErr = model.Err(409, "context epoch changed before tool admission")
	}
	if activeErr == nil {
		tx, beginErr := r.db.Begin()
		activeErr = beginErr
		if activeErr == nil {
			body, _ := json.Marshal(invocation)
			_, activeErr = tx.Exec("INSERT INTO tool_invocations(id,task_id,attempt,call_id,action_hash,state,body) VALUES(?,?,?,?,?,?,?)", invocation.ID, t.ID, t.Attempts, call.ID, invocation.ActionHash, invocation.State, redact(string(body)))
			if activeErr == nil {
				activeErr = insertAutomationEvent(tx, "tools.pending", invocation.ID, invocation.ProfileID, map[string]any{"taskId": t.ID, "name": call.Name})
			}
			if activeErr == nil {
				activeErr = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
	}
	r.mu.Unlock()
	if activeErr != nil {
		b.finishCall(state, zero, activeErr)
		return zero, activeErr
	}
	if action.approval || action.question {
		if err = b.awaitHuman(child, state); err != nil {
			b.finishCall(state, zero, err)
			return zero, err
		}
	}
	result, err := b.executeAction(child, call, state)
	if err == nil && child.Err() != nil {
		err = child.Err()
	}
	b.finishCall(state, result, err)
	return result, err
}

func durableToolResult(result model.ToolResult) *model.ToolResult {
	out := &model.ToolResult{IsError: result.IsError, Content: []model.ToolContent{}}
	remaining := 16384
	for _, content := range result.Content {
		if content.Type != "text" {
			continue
		}
		text := truncate(redact(content.Text), remaining)
		out.Content = append(out.Content, model.ToolContent{Type: "text", Text: text})
		remaining -= len(text)
		if remaining == 0 {
			break
		}
	}
	return out
}
func (b *toolBroker) finishCall(call *brokerCall, result model.ToolResult, callErr error) {
	r := b.relay
	r.mu.Lock()
	defer r.mu.Unlock()
	call.result = result
	call.err = callErr
	call.done = true
	invocation := call.invocation
	if callErr == nil {
		invocation.State = "completed"
		invocation.Result = durableToolResult(result)
	} else {
		invocation.State = "failed"
		if errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
			invocation.State = "cancelled"
		}
		if call.invocation.State == "claimed" && call.action.mutation {
			invocation.State = "uncertain"
		}
		invocation.Error = truncate(redact(callErr.Error()), 2048)
	}
	if invocation.Name == "meshra_ask_user" {
		invocation.Result = nil
	}
	call.invocation = invocation
	body, _ := json.Marshal(invocation)
	tx, err := r.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	updated, err := tx.Exec("UPDATE tool_invocations SET state=?,body=? WHERE id=?", invocation.State, string(body), invocation.ID)
	if err != nil {
		return
	}
	n, _ := updated.RowsAffected()
	if n == 0 {
		return
	}
	if _, err = tx.Exec("UPDATE tool_approvals SET status=?,revision=revision+1 WHERE invocation_id=? AND status IN ('pending','approved','answered','claimed')", invocation.State, invocation.ID); err != nil {
		return
	}
	if err = insertAutomationEvent(tx, "tools."+invocation.State, invocation.ID, invocation.ProfileID, map[string]string{"taskId": invocation.TaskID, "name": invocation.Name}); err != nil {
		return
	}
	_ = tx.Commit()
}

func (b *toolBroker) claim(call *brokerCall) error {
	r := b.relay
	if call.owner != "" {
		if err := r.requireConsole(model.Principal{Admin: true, ConsoleSessionID: call.owner}); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _, epoch, err := b.activeLocked()
	if err != nil {
		return err
	}
	if call.action.runtimeRevision != 0 {
		_, revision, configErr := r.runtimeConfigLocked()
		if configErr != nil {
			return configErr
		}
		if revision != call.action.runtimeRevision {
			return model.Err(409, "runtime manifest changed after approval")
		}
	}
	if epoch != call.invocation.Epoch {
		return model.Err(409, "tool action epoch is stale")
	}
	var state string
	if err = r.db.QueryRow("SELECT state FROM tool_invocations WHERE id=? AND action_hash=?", call.invocation.ID, call.invocation.ActionHash).Scan(&state); err != nil {
		return err
	}
	if call.action.approval && !call.humanApproved {
		return model.Err(403, "a live exact human approval is required")
	}
	alreadyClaimed := state == "claimed" && call.invocation.State == "claimed"
	if !alreadyClaimed && state != "pending" && state != "approved" {
		return model.Err(409, "tool invocation cannot be dispatched again")
	}
	if call.action.approval {
		var approvalStatus string
		var expires string
		if err = r.db.QueryRow("SELECT status,expires_at FROM tool_approvals WHERE id=? AND invocation_id=?", call.approvalID, call.invocation.ID).Scan(&approvalStatus, &expires); err != nil {
			return err
		}
		deadline, parseErr := time.Parse(time.RFC3339Nano, expires)
		validStatus := approvalStatus == "approved" || alreadyClaimed && approvalStatus == "claimed"
		if parseErr != nil || !validStatus || !deadline.After(now()) {
			return model.Err(409, "approval expired or was invalidated")
		}
	}
	if alreadyClaimed { return nil }
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	claimed := call.invocation
	claimed.State = "claimed"
	body, _ := json.Marshal(claimed)
	result, err := tx.Exec("UPDATE tool_invocations SET state='claimed',body=? WHERE id=? AND state=?", redact(string(body)), call.invocation.ID, state)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return model.Err(409, "tool invocation claim conflicted")
	}
	if call.action.approval {
		if _, err = tx.Exec("UPDATE tool_approvals SET status='claimed',revision=revision+1 WHERE id=? AND status='approved'", call.approvalID); err != nil {
			return err
		}
	}
	if err = insertAutomationEvent(tx, "tools.claimed", call.invocation.ID, call.invocation.ProfileID, map[string]string{"taskId": call.invocation.TaskID}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	call.invocation = claimed
	return nil
}

func (b *toolBroker) Close() {
	r := b.relay
	r.mu.Lock()
	if b.closed {
		r.mu.Unlock()
		return
	}
	b.closed = true
	b.cancel()
	r.invalidateToolTaskLocked(b.taskID, "turn closed")
	if r.toolBrokers[b.taskID] == b {
		delete(r.toolBrokers, b.taskID)
	}
	r.mu.Unlock()
	b.socketMu.Lock()
	if b.mcpHandler != nil {
		b.mcpHandler.Close()
	}
	if b.socket != nil {
		b.socket.Close()
	}
	b.socketMu.Unlock()
	b.callsWG.Wait()
	if b.workspace != nil {
		b.workspace.Close()
	}
}

func (r *Relay) ActiveTools(p model.Principal) (model.ToolExecutor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.Admin {
		return nil, nil
	}
	a, err := r.agent(p.AgentID)
	if err != nil {
		return nil, err
	}
	if !a.ProfileManaged {
		return nil, nil
	}
	b := r.toolBrokers[p.TaskID]
	if b == nil {
		return nil, model.Err(403, "active profile tool broker required")
	}
	t, principal, _, err := b.activeLocked()
	if err != nil {
		return nil, err
	}
	if principal.AgentID != p.AgentID || principal.SessionID != p.SessionID || t.ID != p.TaskID {
		return nil, model.Err(403, "tool credential belongs to another attempt")
	}
	return b, nil
}

func toolJSON(value any) (model.ToolResult, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return model.ToolResult{}, err
	}
	return model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: redact(string(body))}}}, nil
}
func sandboxToolError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sandbox.ErrUnavailable):
		return model.Err(503, err.Error())
	case errors.Is(err, sandbox.ErrLimit):
		return model.Err(413, err.Error())
	case errors.Is(err, sandbox.ErrConflict):
		return model.Err(409, err.Error())
	case errors.Is(err, sandbox.ErrDenied):
		return model.Err(403, err.Error())
	default:
		return fmt.Errorf("confined tool: %w", err)
	}
}
