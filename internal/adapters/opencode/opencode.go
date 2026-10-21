// Package opencode owns authenticated localhost OpenCode server processes.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"meshra/internal/model"
)

type runtimeSession struct {
	life    context.Context
	agent   model.Agent
	start   starter
	mu      sync.Mutex
	session string
	closed  bool
	cancel  context.CancelFunc
	running sync.Mutex
}

func New() model.Adapter {
	return adapter(func() (starter, error) {
		path, err := executable()
		if err != nil {
			return nil, err
		}
		return startServer(path), nil
	})
}
func adapter(resolve func() (starter, error)) model.Adapter {
	return model.Adapter{Capabilities: map[string]bool{"prompt": true, "stream": false, "resume": true, "cancel": true, "mcp": true, "interactiveApproval": false, "authenticatedSmokeVerified": false}, Factory: func(ctx context.Context, agent model.Agent, emit model.Emit) (model.Runtime, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if (agent.Config.Model == "") != (agent.Config.Provider == "") {
			return nil, errors.New("OpenCode requires both provider and model, or neither for its native default")
		}
		start, err := resolve()
		if err != nil {
			return nil, err
		}
		return &runtimeSession{life: ctx, agent: agent, start: start, session: agent.SessionID}, nil
	}}
}
func (r *runtimeSession) SessionID() string { r.mu.Lock(); defer r.mu.Unlock(); return r.session }
func (r *runtimeSession) Close() error {
	r.mu.Lock()
	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.running.Lock()
	r.running.Unlock()
	return nil
}
func permissions() []map[string]string {
	rules := []map[string]string{{"permission": "*", "pattern": "*", "action": "ask"}}
	for _, name := range []string{"read", "glob", "grep", "meshra_meshra_agents_list", "meshra_meshra_agent_status", "meshra_meshra_send", "meshra_meshra_request", "meshra_meshra_task_status", "meshra_meshra_task_events", "meshra_meshra_task_cancel", "meshra_meshra_task_trace", "meshra_meshra_collaboration_conversation_create", "meshra_meshra_collaboration_conversation_get", "meshra_meshra_collaboration_conversation_list", "meshra_meshra_collaboration_conversation_addParticipant", "meshra_meshra_collaboration_conversation_events", "meshra_meshra_collaboration_context_publish", "meshra_meshra_collaboration_context_get", "meshra_meshra_collaboration_context_search", "meshra_meshra_collaboration_context_attach", "meshra_meshra_collaboration_context_grant", "meshra_meshra_collaboration_context_revoke", "meshra_meshra_collaboration_context_audit", "meshra_meshra_collaboration_task_delegate", "meshra_meshra_collaboration_task_list"} {
		rules = append(rules, map[string]string{"permission": name, "pattern": "*", "action": "allow"})
	}
	return rules
}

// OpenCode 1.19.3 PATCH appends rules; the last matching rule wins.
// Reinstall only when needed, rather than growing the transcript every turn.
func hasPermissionTail(current, policy []map[string]string) bool {
	if len(current) < len(policy) {
		return false
	}
	tail := current[len(current)-len(policy):]
	for i := range policy {
		if !maps.Equal(tail[i], policy[i]) {
			return false
		}
	}
	return true
}
func (r *runtimeSession) Run(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
	r.running.Lock()
	defer r.running.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopLife := context.AfterFunc(r.life, cancel)
	defer stopLife()
	if err := r.life.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", errors.New("OpenCode runtime closed")
	}
	r.cancel = cancel
	session := r.session
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.cancel = nil; r.mu.Unlock() }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s, err := r.start(ctx, r.agent, turn)
	if err != nil {
		return "", err
	}
	defer s.stop()
	var native struct {
		ID         string              `json:"id"`
		Directory  string              `json:"directory"`
		Permission []map[string]string `json:"permission"`
	}
	policy := permissions()
	if session == "" {
		err = s.call(ctx, "POST", "/session", map[string]any{"title": "Meshra " + r.agent.ID, "permission": policy}, &native)
	} else {
		err = s.call(ctx, "GET", "/session/"+url.PathEscape(session), nil, &native)
	}
	if err != nil {
		return "", fmt.Errorf("OpenCode session create/resume: %w", err)
	}
	if native.ID == "" || (session != "" && native.ID != session) {
		return "", errors.New("OpenCode returned an unexpected native session")
	}
	if native.Directory != "" && native.Directory != r.agent.Cwd {
		return "", errors.New("OpenCode session belongs to a different working directory")
	}
	session = native.ID
	r.mu.Lock()
	r.session = session
	r.mu.Unlock()
	if emit != nil {
		emit("session", session)
	}
	if !hasPermissionTail(native.Permission, policy) {
		if err = s.call(ctx, "PATCH", "/session/"+url.PathEscape(session), map[string]any{"permission": policy}, nil); err != nil {
			return "", fmt.Errorf("OpenCode session permission policy: %w", err)
		}
	}
	// Subscribe before prompting: permission requests must be explicitly rejected,
	// not left waiting on a terminal that this owned headless server does not have.
	eventsCtx, stopEvents := context.WithCancel(ctx)
	defer stopEvents()
	response, err := s.request(eventsCtx, "GET", "/event", nil)
	if err != nil {
		return "", fmt.Errorf("OpenCode permission event stream: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return "", fmt.Errorf("OpenCode permission event stream returned HTTP %d", response.StatusCode)
	}
	eventDone := make(chan error, 1)
	go func() {
		defer response.Body.Close()
		eventDone <- s.permissions(eventsCtx, response.Body, session, emit)
	}()
	defer func() { stopEvents(); response.Body.Close(); <-eventDone }()
	// Independent cancellation sends the native abort endpoint before teardown.
	abortDone := make(chan struct{})
	stopAbort := context.AfterFunc(ctx, func() {
		defer close(abortDone)
		abortCtx, abortCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer abortCancel()
		_ = s.call(abortCtx, "POST", "/session/"+url.PathEscape(session)+"/abort", nil, nil)
	})
	defer func() {
		if !stopAbort() {
			<-abortDone
		}
	}()
	body := map[string]any{"parts": []map[string]string{{"type": "text", "text": turn.Task.Text}}}
	if r.agent.Config.Model != "" {
		body["model"] = map[string]string{"providerID": r.agent.Config.Provider, "modelID": r.agent.Config.Model}
	}
	var reply struct {
		Info struct {
			Tokens *struct {
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
				Cache  struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
			SessionID string `json:"sessionID"`
			Error     *struct {
				Name string `json:"name"`
			} `json:"error"`
		} `json:"info"`
		Parts []struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Tool  string `json:"tool"`
			State struct {
				Status string `json:"status"`
			} `json:"state"`
		} `json:"parts"`
	}
	// Prompt runs in a goroutine so a failed permission stream aborts the turn.
	promptDone := make(chan error, 1)
	go func() {
		promptDone <- s.call(ctx, "POST", "/session/"+url.PathEscape(session)+"/message", body, &reply)
	}()
	select {
	case err = <-promptDone:
	case eventErr := <-eventDone:
		// Restore the result for the deferred join, then terminate uncertain work.
		eventDone <- eventErr
		cancelled := ctx.Err()
		cancel()
		<-promptDone
		if cancelled != nil {
			return "", cancelled
		}
		if eventErr == nil {
			eventErr = errors.New("event stream ended")
		}
		return "", fmt.Errorf("OpenCode permission supervision failed: %w", eventErr)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("OpenCode prompt: %w", err)
	}
	if reply.Info.SessionID != session {
		return "", errors.New("OpenCode prompt returned another session's result")
	}
	if reply.Info.Error != nil {
		return "", fmt.Errorf("OpenCode prompt failed: %s (check native provider authentication and model configuration)", reply.Info.Error.Name)
	}
	if emit != nil && reply.Info.Tokens != nil {
		v := reply.Info.Tokens
		data, _ := json.Marshal(map[string]int64{"inputTokens": v.Input + v.Cache.Read + v.Cache.Write, "outputTokens": v.Output, "cachedInputTokens": v.Cache.Read + v.Cache.Write})
		emit("usage", string(data))
	}
	var text strings.Builder
	for _, part := range reply.Parts {
		if part.Type == "text" {
			text.WriteString(part.Text)
		}
		if part.Type == "tool" && part.State.Status == "error" && emit != nil {
			emit("progress", "OpenCode tool failed: "+part.Tool)
		}
	}
	result := text.String()
	if turn.Token != "" {
		result = strings.ReplaceAll(result, turn.Token, "[redacted]")
	}
	return result, nil
}
func (s *server) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, data)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("opencode", s.password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-opencode-directory", s.cwd)
	return s.client.Do(req)
}
func (s *server) call(ctx context.Context, method, path string, body, out any) error {
	response, err := s.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s %s", response.StatusCode, method, path)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4*1024*1024))
		return err
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 4*1024*1024)).Decode(out); err != nil {
		return fmt.Errorf("invalid HTTP response: %w", err)
	}
	return nil
}
func (s *server) permissions(ctx context.Context, reader io.Reader, session string, emit model.Emit) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var data strings.Builder
	handle := func() error {
		if data.Len() == 0 {
			return nil
		}
		var event struct {
			Type       string `json:"type"`
			Properties struct {
				ID         string `json:"id"`
				SessionID  string `json:"sessionID"`
				Permission string `json:"permission"`
				Action     string `json:"action"`
			} `json:"properties"`
		}
		raw := data.String()
		data.Reset()
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return errors.New("invalid OpenCode event JSON")
		}
		if event.Properties.SessionID != session || (event.Type != "permission.asked" && event.Type != "permission.v2.asked") {
			return nil
		}
		if event.Properties.ID == "" {
			return errors.New("OpenCode permission request lacks ID")
		}
		name := event.Properties.Permission
		if name == "" {
			name = event.Properties.Action
		}
		if emit != nil {
			emit("approval_required", "Denied OpenCode permission "+name+": no interactive approval channel")
		}
		path := "/permission/" + url.PathEscape(event.Properties.ID) + "/reply"
		if event.Type == "permission.v2.asked" {
			path = "/api/session/" + url.PathEscape(session) + "/permission/" + url.PathEscape(event.Properties.ID) + "/reply"
		}
		return s.call(ctx, "POST", path, map[string]string{"reply": "reject"}, nil)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := handle(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return handle()
}
