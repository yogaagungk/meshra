package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
	"meshra/internal/sandbox"
)

type toolHistorySearchArgs struct {
	Query string `json:"query"`
	After string `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
type toolHistoryReadArgs struct {
	TaskID string `json:"taskId,omitempty"`
	Before int64  `json:"before,omitempty"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}
type toolMemoryArgs struct {
	ID               string `json:"id,omitempty"`
	ExpectedRevision int    `json:"expectedRevision,omitempty"`
	Content          string `json:"content"`
	Pinned           bool   `json:"pinned"`
	SourceTaskID     string `json:"sourceTaskId,omitempty"`
}
type toolProfileRequestArgs struct {
	ProfileID      string `json:"profileId"`
	Text           string `json:"text"`
	IdempotencyKey string `json:"idempotencyKey"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}
type toolReadArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}
type toolWriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
}
type toolCommandArgs struct {
	Argv           []string `json:"argv,omitempty"`
	Shell          string   `json:"shell,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
}
type toolAttachmentReadArgs struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}
type toolPublishArgs struct {
	Path string `json:"path"`
	Name string `json:"name"`
	MIME string `json:"mime,omitempty"`
}
type toolQuestionArgs struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
}
type toolSkillReadArgs struct {
	SkillID   string `json:"skillId"`
	AssetPath string `json:"assetPath,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type preparedCommand struct {
	input  toolCommandArgs
	argv   []string
	config sandbox.Config
}
type preparedPublication struct {
	input toolPublishArgs
	file  *os.File
	hash  string
	size  int64
}

func parseToolAction(call model.ToolCall) (*toolAction, error) {
	action := &toolAction{preview: toolPreview{Label: call.Name}}
	var dst any
	switch call.Name {
	case "meshra_history_search":
		dst = &toolHistorySearchArgs{}
	case "meshra_history_read":
		dst = &toolHistoryReadArgs{}
	case "meshra_memory_save":
		dst = &toolMemoryArgs{}
		action.mutation = true
	case "meshra_agents_list":
		dst = &struct{}{}
	case "meshra_profile_request":
		dst = &toolProfileRequestArgs{}
		action.mutation = true
	case "meshra_routine_list":
		dst = &toolRoutineListArgs{}
	case "meshra_routine_propose":
		dst = &toolRoutineProposeArgs{}
		action.mutation = true
	case "meshra_workspace_read":
		dst = &toolReadArgs{}
	case "meshra_workspace_write":
		dst = &toolWriteArgs{}
		action.mutation = true
		action.approval = true
	case "meshra_workspace_command":
		dst = &toolCommandArgs{}
		action.mutation = true
		action.approval = true
	case "meshra_external_mcp":
		dst = &externalMCPRequest{}
		action.mutation = true
		action.approval = true
	case "meshra_attachment_read":
		dst = &toolAttachmentReadArgs{}
	case "meshra_attachment_publish":
		dst = &toolPublishArgs{}
		action.mutation = true
		action.approval = true
	case "meshra_ask_user":
		dst = &toolQuestionArgs{}
		action.mutation = true
		action.question = true
	case "meshra_skill_read":
		dst = &toolSkillReadArgs{}
	default:
		return nil, model.Err(403, "unknown broker tool")
	}
	if err := decodeAutomation(call.Arguments, dst); err != nil {
		return nil, err
	}
	action.args = dst
	preview := redact(string(call.Arguments))
	if json.Valid([]byte(preview)) {
		action.preview.Arguments = json.RawMessage(preview)
	} else {
		body, _ := json.Marshal(map[string]string{"redacted": preview})
		action.preview.Arguments = body
	}
	switch q := dst.(type) {
	case *toolRoutineListArgs:
		if q.After != "" && !profileIdentifier(q.After) {
			return nil, model.Err(400, "invalid routine cursor")
		}
		if q.Limit == 0 {
			q.Limit = 20
		}
		if q.Limit < 1 || q.Limit > 20 {
			return nil, model.Err(400, "routine limit must be 1..20")
		}
	case *toolRoutineProposeArgs:
		if err := validateRoutineProposal(q); err != nil {
			return nil, err
		}
	case *externalMCPRequest:
		if err := validateExternalMCPRequest(q); err != nil {
			return nil, err
		}
	case *toolReadArgs:
		if q.Path == "" || len(q.Path) > 4096 || q.Offset < 0 || q.Limit < 0 || q.Limit > 65536 {
			return nil, model.Err(400, "invalid rooted read bounds")
		}
		if q.Limit == 0 {
			q.Limit = 65536
		}
	case *toolWriteArgs:
		if q.Path == "" || len(q.Path) > 4096 || len(q.Content) > 65536 || !utf8.ValidString(q.Content) || q.Mode > 0777 {
			return nil, model.Err(400, "invalid exact file replacement")
		}
	case *toolCommandArgs:
		if (len(q.Argv) == 0) == (q.Shell == "") {
			return nil, model.Err(400, "provide exactly one of argv or shell")
		}
		if len(q.Argv) > 128 || len(q.Shell) > 65536 || strings.ContainsRune(q.Shell, 0) {
			return nil, model.Err(413, "command argument bound exceeded")
		}
		for _, arg := range q.Argv {
			if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
				return nil, model.Err(400, "invalid argv argument")
			}
		}
		if q.TimeoutSeconds == 0 {
			q.TimeoutSeconds = 60
		}
		if q.TimeoutSeconds < 1 || q.TimeoutSeconds > 300 {
			return nil, model.Err(400, "command timeout must be 1..300 seconds")
		}
	case *toolAttachmentReadArgs:
		if q.ID == "" || q.Offset < 0 || q.Limit < 0 || q.Limit > 65536 {
			return nil, model.Err(400, "invalid attachment read bounds")
		}
		if q.Limit == 0 {
			q.Limit = 65536
		}
	case *toolPublishArgs:
		if q.Path == "" || len(q.Path) > 4096 || q.Name == "" || len(q.Name) > 200 || len(q.MIME) > 200 {
			return nil, model.Err(400, "invalid publication target")
		}
	case *toolQuestionArgs:
		if q.Question == "" || len(q.Question) > 4096 || !utf8.ValidString(q.Question) || strings.ContainsRune(q.Question, 0) || len(q.Choices) > 8 {
			return nil, model.Err(400, "invalid clarification question")
		}
		for _, choice := range q.Choices {
			if choice == "" || len(choice) > 256 || !safeRegistryText(choice) {
				return nil, model.Err(400, "invalid clarification choice")
			}
		}
	}
	return action, nil
}

func (b *toolBroker) ensureWorkspace(ctx context.Context) (*sandbox.Snapshot, error) {
	r := b.relay
	r.mu.Lock()
	existing := b.workspace
	closed := b.closed
	r.mu.Unlock()
	if closed {
		return nil, model.Err(403, "tool turn closed")
	}
	if existing != nil {
		return existing, nil
	}
	workspace, err := r.prepareWorkspace(ctx, b.snapshot.Profile.WorkspaceID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if b.closed {
		r.mu.Unlock()
		workspace.Close()
		return nil, model.Err(403, "tool turn closed")
	}
	b.workspace = workspace
	r.mu.Unlock()
	return workspace, nil
}
func requireRuntime(ctx context.Context, r *Relay) (sandbox.Config, int, error) {
	config, revision, err := r.runtimeConfig()
	if err != nil {
		return config, revision, err
	}
	status := sandbox.Check(ctx, config)
	if status.State != "ready" {
		return config, revision, model.Err(503, "confined runtime "+status.State+": "+strings.Join(status.Prerequisites, "; "))
	}
	return config, revision, nil
}
func resolveRuntimeExecutable(config sandbox.Config, name string) (string, error) {
	if path := config.Executables[name]; path != "" {
		return path, nil
	}
	for _, path := range config.Executables {
		if name == path {
			return path, nil
		}
	}
	return "", model.Err(403, "executable is not in the operator-reviewed runtime manifest")
}
func isShell(path string) bool {
	switch filepath.Base(path) {
	case "sh", "bash", "dash", "zsh", "fish", "ksh", "csh", "tcsh":
		return true
	}
	return false
}

func (b *toolBroker) prepareAction(ctx context.Context, call model.ToolCall, state *brokerCall) error {
	action := state.action
	switch q := action.args.(type) {
	case *toolWriteArgs:
		_, revision, err := requireRuntime(ctx, b.relay)
		if err != nil {
			return err
		}
		action.runtimeRevision = revision
		workspace, err := b.ensureWorkspace(ctx)
		if err != nil {
			return err
		}
		manifest, err := workspace.PreviewStage(q.Path, []byte(q.Content), os.FileMode(q.Mode))
		if err != nil {
			return sandboxToolError(err)
		}
		action.manifest = manifest
		action.cleanup = func() { manifest.Close() }
		action.preview.Label = "Stage exact private file replacement (host Apply is separate)"
		action.preview.ManifestHash = manifest.Hash
		action.preview.Diff = redact(manifest.Preview)
		action.preview.Truncated = manifest.PreviewTruncated
	case *toolCommandArgs:
		config, revision, err := requireRuntime(ctx, b.relay)
		if err != nil {
			return err
		}
		action.runtimeRevision = revision
		workspace, err := b.ensureWorkspace(ctx)
		if err != nil {
			return err
		}
		baseline, err := workspace.Changes(ctx)
		if err != nil {
			return sandboxToolError(err)
		}
		action.preview.ManifestHash = baseline.Hash
		baseline.Close()
		argv := q.Argv
		if q.Shell != "" {
			executable, err := resolveRuntimeExecutable(config, "sh")
			if err != nil {
				return err
			}
			argv = []string{executable, "-lc", q.Shell}
			action.preview.Label = "Execute explicit /bin/sh -lc shell action in private snapshot"
		} else {
			executable, err := resolveRuntimeExecutable(config, argv[0])
			if err != nil {
				return err
			}
			if isShell(argv[0]) || isShell(executable) {
				return model.Err(400, "shell execution must use the explicitly labeled shell field")
			}
			argv[0] = executable
			action.preview.Label = "Execute exact argv in confined private snapshot"
		}
		body, _ := json.Marshal(map[string]any{"argv": argv, "timeoutSeconds": q.TimeoutSeconds, "runtimeRevision": revision})
		action.preview.Arguments = json.RawMessage(redact(string(body)))
		action.args = &preparedCommand{input: *q, argv: argv, config: config}
	case *toolPublishArgs:
		b.relay.mu.Lock()
		workspace := b.workspace
		b.relay.mu.Unlock()
		if workspace == nil {
			return model.Err(404, "no approved private workspace result exists for this turn")
		}
		file, entry, err := workspace.OpenRegular(q.Path)
		if err != nil {
			return sandboxToolError(err)
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return err
		}
		action.cleanup = func() { file.Close() }
		action.preview.Label = "Publish exact private file to this conversation"
		action.preview.ManifestHash = entry.Hash
		body, _ := json.Marshal(map[string]any{"path": q.Path, "name": attachmentName(q.Name), "mime": q.MIME, "size": info.Size(), "sha256": entry.Hash})
		action.preview.Arguments = json.RawMessage(redact(string(body)))
		action.args = &preparedPublication{input: *q, file: file, hash: entry.Hash, size: info.Size()}
	}
	return nil
}

func (b *toolBroker) executeAction(ctx context.Context, call model.ToolCall, state *brokerCall) (model.ToolResult, error) {
	var zero model.ToolResult
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	r := b.relay
	r.mu.Lock()
	task, principal, _, err := b.activeLocked()
	r.mu.Unlock()
	if err != nil {
		return zero, err
	}
	if call.Name != "meshra_workspace_command" {
		if err = b.claim(state); err != nil {
			return zero, err
		}
	}
	switch q := state.action.args.(type) {
	case *toolRoutineListArgs:
		return r.routineListForTool(ctx, b, *q)
	case *toolRoutineProposeArgs:
		return r.routineProposeForTool(ctx, b, *q)
	case *externalMCPRequest:
		return r.dispatchExternalMCP(ctx, principal, b.snapshot.Profile.ID, state.invocation.ID, *q)
	case *toolHistorySearchArgs:
		raw, _ := json.Marshal(profileHistorySearchRequest{ProfileID: b.snapshot.Profile.ID, ConversationID: task.ConversationID, Query: q.Query, After: q.After, Limit: q.Limit})
		v, err := r.profileHistoryOperation(ctx, principal, "history.search", raw)
		if err != nil {
			return zero, err
		}
		return toolJSON(v)
	case *toolHistoryReadArgs:
		raw, _ := json.Marshal(profileHistoryReadRequest{ProfileID: b.snapshot.Profile.ID, ConversationID: task.ConversationID, TaskID: q.TaskID, Before: q.Before, Offset: q.Offset, Limit: q.Limit})
		v, err := r.profileHistoryOperation(ctx, principal, "history.read", raw)
		if err != nil {
			return zero, err
		}
		return toolJSON(v)
	case *toolMemoryArgs:
		raw, _ := json.Marshal(profileMemorySaveRequest{ProfileID: b.snapshot.Profile.ID, ID: q.ID, ExpectedRevision: q.ExpectedRevision, Content: q.Content, Pinned: q.Pinned, Scope: "conversation", ConversationID: task.ConversationID, SourceTaskID: q.SourceTaskID})
		v, err := r.profileMemoryOperation(ctx, principal, "memory.save", raw)
		if err != nil {
			return zero, err
		}
		return toolJSON(v)
	case *toolProfileRequestArgs:
		r.mu.Lock()
		current, err := r.profile(b.snapshot.Profile.ID)
		if err == nil && !contains(current.AllowedProfileIDs, q.ProfileID) {
			err = model.Err(403, "target profile is not explicitly allowed")
		}
		var target model.Profile
		var binding model.ProfileBinding
		var snapshot model.ProfileTaskSnapshot
		var admitted model.Task
		if err == nil {
			target, err = r.profile(q.ProfileID)
		}
		if err == nil {
			binding, err = r.profileBinding(target.ID, task.ConversationID)
			if err != nil {
				err = model.Err(403, "target must already share this explicitly authorized conversation")
			}
		}
		if err == nil {
			snapshot, err = r.prepareProfileSnapshot(target, binding)
			snapshot.External = b.snapshot.External
			snapshot.ConsoleSessionOwner = b.snapshot.ConsoleSessionOwner
		}
		if err == nil {
			admitted, err = r.admitLocked(principal, model.SendRequest{To: binding.AgentID, Text: q.Text, Type: "request", ConversationID: task.ConversationID, IdempotencyKey: q.IdempotencyKey, TimeoutSeconds: q.TimeoutSeconds}, &admissionMeta{profile: &snapshot})
		}
		r.mu.Unlock()
		if err != nil {
			return zero, err
		}
		return toolJSON(admitted)
	case *toolReadArgs:
		if !b.snapshot.Profile.Capabilities.WorkspaceRead {
			return zero, model.Err(403, "workspace reads are disabled")
		}
		r.mu.Lock()
		workspace := b.workspace
		w, err := r.workspace(b.snapshot.Profile.WorkspaceID)
		r.mu.Unlock()
		if err != nil {
			return zero, err
		}
		var data []byte
		if workspace != nil {
			data, err = workspace.Read(q.Path, q.Offset, q.Limit, true)
		} else {
			data, err = sandbox.ReadWorkspace(w.Path, q.Path, q.Offset, q.Limit)
		}
		if err != nil {
			return zero, sandboxToolError(err)
		}
		if utf8.Valid(data) {
			return toolJSON(map[string]any{"path": q.Path, "offset": q.Offset, "text": redact(string(data)), "bytes": len(data)})
		}
		return toolJSON(map[string]any{"path": q.Path, "offset": q.Offset, "encoding": "base64", "data": base64.StdEncoding.EncodeToString(data), "bytes": len(data)})
	case *toolWriteArgs:
		manifest, err := b.workspace.Stage(q.Path, []byte(q.Content), os.FileMode(q.Mode))
		if err != nil {
			return zero, sandboxToolError(err)
		}
		defer manifest.Close()
		if manifest.Hash != state.action.manifest.Hash {
			return zero, model.Err(409, "private file diff changed after approval")
		}
		changeset, err := r.persistChangeset(ctx, state.invocation.ID, b.snapshot.Profile.WorkspaceID, manifest)
		if err != nil {
			return zero, err
		}
		return toolJSON(changesetReceipt(changeset))
	case *preparedCommand:
		result, err := sandbox.Run(ctx, q.config, sandbox.Request{Workspace: b.workspace, Argv: q.argv, Kind: sandbox.Command, Timeout: time.Duration(q.input.TimeoutSeconds) * time.Second, BeforeStart: func() error { return b.claim(state) }})
		if err != nil {
			return zero, sandboxToolError(err)
		}
		var receipt any
		if result.Changes != nil {
			defer result.Changes.Close()
			changeset, err := r.persistChangeset(ctx, state.invocation.ID, b.snapshot.Profile.WorkspaceID, result.Changes)
			if err != nil {
				return zero, err
			}
			receipt = changesetReceipt(changeset)
		}
		value, err := toolJSON(map[string]any{"exitCode": result.ExitCode, "stdout": redact(result.Stdout), "stderr": redact(result.Stderr), "truncated": result.Truncated, "changeset": receipt})
		value.IsError = result.ExitCode != 0
		return value, err
	case *toolAttachmentReadArgs:
		attachment, reader, err := r.ReadAttachment(ctx, principal, q.ID)
		if err != nil {
			return zero, err
		}
		defer reader.Close()
		if q.Offset > attachment.Size {
			return zero, model.Err(400, "offset exceeds attachment size")
		}
		if seeker, ok := reader.(io.Seeker); ok {
			_, err = seeker.Seek(q.Offset, io.SeekStart)
		} else {
			_, err = io.CopyN(io.Discard, reader, q.Offset)
		}
		if err != nil {
			return zero, err
		}
		remaining := min(int64(q.Limit), attachment.Size-q.Offset)
		data, err := io.ReadAll(io.LimitReader(reader, remaining))
		if err != nil {
			return zero, err
		}
		output := map[string]any{"id": attachment.ID, "offset": q.Offset, "nextOffset": q.Offset + int64(len(data)), "eof": q.Offset+int64(len(data)) == attachment.Size, "mime": attachment.MIME}
		if attachmentIsText(attachment) && utf8.Valid(data) {
			output["text"] = redact(string(data))
		} else {
			output["encoding"] = "base64"
			output["data"] = base64.StdEncoding.EncodeToString(data)
		}
		return toolJSON(output)
	case *preparedPublication:
		attachment, err := r.storeTaskPublication(ctx, principal, task, q.input.Name, q.input.MIME, q.file, q.size, q.hash)
		if err != nil {
			return zero, err
		}
		return toolJSON(attachment)
	case *toolSkillReadArgs:
		r.mu.Lock()
		var value any
		var err error
		if q.AssetPath != "" {
			value, err = r.readPinnedSkillAsset(b.snapshot, q.SkillID, q.AssetPath, q.Offset, q.Limit)
		} else {
			value, err = r.readPinnedSkillForTool(b.snapshot, q.SkillID)
		}
		r.mu.Unlock()
		if err != nil {
			return zero, err
		}
		return toolJSON(value)
	case *toolQuestionArgs:
		r.mu.Lock()
		answer := state.answer
		r.mu.Unlock()
		return model.ToolResult{Content: []model.ToolContent{{Type: "text", Text: redact(answer)}}}, nil
	case *struct{}:
		r.mu.Lock()
		current, err := r.profile(b.snapshot.Profile.ID)
		if err != nil {
			r.mu.Unlock()
			return zero, err
		}
		profiles := []map[string]any{}
		for _, id := range current.AllowedProfileIDs {
			profile, err := r.profile(id)
			if err != nil || profile.Archived {
				continue
			}
			binding, err := r.profileBinding(id, task.ConversationID)
			if err != nil {
				continue
			}
			profiles = append(profiles, map[string]any{"id": id, "name": profile.Name, "bindingId": binding.ID, "agentId": binding.AgentID, "harness": binding.Harness})
		}
		r.mu.Unlock()
		return toolJSON(map[string]any{"profiles": profiles})
	}
	return zero, model.Err(403, "tool action is unavailable")
}
func changesetReceipt(v workspaceChangeset) map[string]any {
	return map[string]any{"id": v.ID, "manifestHash": v.ManifestHash, "revision": v.Revision, "state": v.State, "changes": v.Changes}
}
