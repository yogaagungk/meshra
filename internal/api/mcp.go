package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"meshra/internal/model"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func schema(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func tools() []toolDefinition {
	send := map[string]any{"to": str("Canonical destination agent ID"), "text": str("Message text, at most 64 KiB"), "idempotencyKey": str("Deduplication key"), "timeoutSeconds": map[string]any{"type": "integer", "minimum": 1}, "parentTaskId": str("Parent task, if delegating"), "maxHops": map[string]any{"type": "integer", "minimum": 1}}
	policy := schema(map[string]any{
		"destinations":      map[string]any{"type": "array", "items": str("Allowed destination ID")},
		"operations":        map[string]any{"type": "array", "items": str("Allowed outbound operation")},
		"delegate":          map[string]any{"type": "boolean"},
		"senders":           map[string]any{"type": "array", "items": str("Allowed sender ID")},
		"inboundOperations": map[string]any{"type": "array", "items": str("Allowed inbound operation")},
		"worktreeWrites":    map[string]any{"type": "boolean", "description": "Rejected unless adapter can enforce approvals"},
	})
	return append(collaborationTools(), []toolDefinition{
		{"meshra_agents_list", "List agents visible to this authenticated identity", schema(map[string]any{})},
		{"meshra_agent_status", "Inspect a named agent", schema(map[string]any{"id": str("Canonical agent ID")}, "id")},
		{"meshra_agent_register", "Administrator: register an agent without starting its runtime; omitted inbound lists preserve legacy access, empty lists deny", schema(map[string]any{"harness": str("Harness name"), "name": str("Instance name"), "displayName": str("Display name"), "cwd": str("Workspace directory"), "config": schema(map[string]any{"model": str("Model"), "provider": str("Provider")}), "policy": policy, "concurrency": map[string]any{"type": "integer", "minimum": 1}}, "harness", "name", "cwd")},
		{"meshra_send", "Queue a message; returns a persistent task immediately", schema(send, "to", "text")},
		{"meshra_request", "Queue a request; poll its task for the reply (never blocks a turn)", schema(send, "to", "text")},
		{"meshra_task_status", "Inspect a task visible to this identity", schema(map[string]any{"taskId": str("Task ID")}, "taskId")},
		{"meshra_task_events", "Read persistent events strictly after a cursor", schema(map[string]any{"taskId": str("Task ID"), "after": map[string]any{"type": "integer", "minimum": 0}}, "taskId")},
		{"meshra_task_cancel", "Cancel an authorized task", schema(map[string]any{"taskId": str("Task ID")}, "taskId")},
		{"meshra_task_trace", "Read an authorized task and its persistent lifecycle events", schema(map[string]any{"taskId": str("Task ID")}, "taskId")},
		{"meshra_task_retry", "Administrator: manually retry a terminal task; ambiguous failures may repeat effects", schema(map[string]any{"taskId": str("Task ID")}, "taskId")},
		{"meshra_metrics", "Administrator: inspect retained task metrics", schema(map[string]any{})},
		{"meshra_doctor", "Administrator: inspect local relay diagnostics", schema(map[string]any{})},
	}...)
}
func protocolSupported(version string) bool {
	return version == "2025-03-26" || version == "2025-06-18" || version == "2025-11-25"
}
func (s *server) mcp(w http.ResponseWriter, r *http.Request, p model.Principal) {
	if r.Method != http.MethodPost {
		method(w, "POST")
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !protocolSupported(version) {
		fail(w, model.Err(400, "unsupported MCP protocol version"))
		return
	}
	var req rpcRequest
	if !decode(w, r, &req) {
		return
	}
	rpcError := func(code int, message string) {
		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		respond(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		rpcError(-32600, "invalid JSON-RPC request")
		return
	}
	if len(req.ID) > 0 {
		var id any
		if json.Unmarshal(req.ID, &id) != nil {
			rpcError(-32600, "invalid request ID")
			return
		}
		switch id.(type) {
		case string, float64:
		default:
			rpcError(-32600, "request ID must be a string or number")
			return
		}
	} else {
		// Notifications carry no response. No tool is executed without a request ID.
		if req.Method != "notifications/initialized" && req.Method != "notifications/cancelled" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
			ClientInfo      json.RawMessage `json:"clientInfo"`
		}
		if err := arguments(req.Params, &params); err != nil {
			rpcError(-32602, err.Error())
			return
		}
		version := params.ProtocolVersion
		if !protocolSupported(version) {
			version = "2025-11-25"
		}
		result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}, "resources": map[string]any{}}, "serverInfo": map[string]string{"name": "meshra", "version": "0.2.0"}, "instructions": "Requests are asynchronous. Use task status/events/trace to read results. Caller identity comes only from bearer authentication. Registration, retry, metrics and doctor require administrator credentials."}
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := arguments(req.Params, &params); err != nil {
			rpcError(-32602, err.Error())
			return
		}
		u, err := url.Parse(params.URI)
		if err != nil || u.Scheme != "meshra" || u.Host != "contexts" || u.RawQuery != "" || u.Fragment != "" {
			rpcError(-32602, "expected immutable Meshra context URI")
			return
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) != 3 || parts[1] != "versions" {
			rpcError(-32602, "expected context/version URI")
			return
		}
		version, err := strconv.Atoi(parts[2])
		if err != nil || version < 1 {
			rpcError(-32602, "positive version required")
			return
		}
		svc, ok := s.service.(model.CollaborationService)
		if !ok {
			rpcError(-32601, "resources unavailable")
			return
		}
		value, err := svc.Collaborate(p, "context.get", model.CollaborationRequest{ContextID: parts[0], Version: version})
		if err != nil {
			rpcError(-32000, err.Error())
			return
		}
		b, err := json.Marshal(value)
		if err != nil {
			rpcError(-32603, "encoding failed")
			return
		}
		result = map[string]any{"contents": []any{map[string]any{"uri": params.URI, "mimeType": "application/json", "text": string(b)}}}
	case "resources/list":
		// Context search is the metadata discovery surface; no unbounded enumeration.
		result = map[string]any{"resources": []any{}}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = map[string]any{"tools": tools()}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      json.RawMessage `json:"_meta,omitempty"`
		}
		if err := arguments(req.Params, &params); err != nil {
			rpcError(-32602, err.Error())
			return
		}
		value, err := s.callTool(p, params.Name, params.Arguments)
		if err != nil {
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}
		} else {
			text, e := json.Marshal(value)
			if e != nil {
				rpcError(-32603, "cannot encode tool result")
				return
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}, "structuredContent": value, "isError": false}
		}
	default:
		rpcError(-32601, "method not found")
		return
	}
	respond(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}
func arguments(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return fmt.Errorf("arguments must be an object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid trailing arguments")
	}
	return nil
}
func (s *server) callTool(p model.Principal, name string, raw json.RawMessage) (any, error) {
	if strings.HasPrefix(name, "meshra_collaboration_") {
		var q model.CollaborationRequest
		if err := arguments(raw, &q); err != nil {
			return nil, err
		}
		svc, ok := s.service.(model.CollaborationService)
		if !ok {
			return nil, model.Err(501, "collaboration unavailable")
		}
		return svc.Collaborate(p, strings.Replace(strings.TrimPrefix(name, "meshra_collaboration_"), "_", ".", 1), q)
	}
	switch name {
	case "meshra_agent_register":
		var args model.CreateAgent
		if err := arguments(raw, &args); err != nil {
			return nil, err
		}
		if !p.Admin {
			return nil, model.Err(403, "administrator required")
		}
		return s.service.Create(p, args)
	case "meshra_metrics", "meshra_doctor":
		var args struct{}
		if err := arguments(raw, &args); err != nil {
			return nil, err
		}
		if !p.Admin {
			return nil, model.Err(403, "administrator required")
		}
		if name == "meshra_metrics" {
			return s.service.Metrics(p)
		}
		return s.service.Doctor(p)
	case "meshra_agents_list":
		var args struct{}
		if err := arguments(raw, &args); err != nil {
			return nil, err
		}
		agents, err := s.service.Agents(p)
		return map[string]any{"agents": agents}, err
	case "meshra_agent_status":
		var args struct {
			ID string `json:"id"`
		}
		if err := arguments(raw, &args); err != nil {
			return nil, err
		}
		if args.ID == "" {
			return nil, fmt.Errorf("id is required")
		}
		return s.service.Agent(p, args.ID)
	case "meshra_send", "meshra_request":
		// Deliberately omit from: identity is derived only from the authenticated token.
		var args struct {
			To             string `json:"to"`
			Text           string `json:"text"`
			IdempotencyKey string `json:"idempotencyKey"`
			TimeoutSeconds int    `json:"timeoutSeconds"`
			ParentTaskID   string `json:"parentTaskId"`
			MaxHops        int    `json:"maxHops"`
		}
		if err := arguments(raw, &args); err != nil {
			return nil, err
		}
		if args.To == "" || args.Text == "" {
			return nil, fmt.Errorf("to and text are required")
		}
		kind := "send"
		if name == "meshra_request" {
			kind = "request"
		}
		return s.service.Send(p, model.SendRequest{To: args.To, Text: args.Text, Type: kind, IdempotencyKey: args.IdempotencyKey, TimeoutSeconds: args.TimeoutSeconds, ParentTaskID: args.ParentTaskID, MaxHops: args.MaxHops})
	case "meshra_task_status", "meshra_task_events", "meshra_task_cancel", "meshra_task_trace", "meshra_task_retry":
		var args struct {
			TaskID string `json:"taskId"`
			After  int64  `json:"after,omitempty"`
		}
		if name == "meshra_task_events" {
			if err := arguments(raw, &args); err != nil {
				return nil, err
			}
		} else {
			var only struct {
				TaskID string `json:"taskId"`
			}
			if err := arguments(raw, &only); err != nil {
				return nil, err
			}
			args.TaskID = only.TaskID
		}
		if args.TaskID == "" || args.After < 0 {
			return nil, fmt.Errorf("taskId is required and after must be nonnegative")
		}
		switch name {
		case "meshra_task_status":
			return s.service.Task(p, args.TaskID)
		case "meshra_task_cancel":
			return s.service.Cancel(p, args.TaskID)
		case "meshra_task_trace":
			return s.service.Trace(p, args.TaskID)
		case "meshra_task_retry":
			if !p.Admin {
				return nil, model.Err(403, "administrator required")
			}
			return s.service.Retry(p, args.TaskID)
		default:
			events, err := s.service.Events(p, args.TaskID, args.After)
			return map[string]any{"events": events}, err
		}
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func collaborationTools() []toolDefinition {
	out := []toolDefinition{}
	for _, op := range []string{"conversation.create", "conversation.get", "conversation.list", "conversation.addParticipant", "conversation.events", "context.publish", "context.get", "context.search", "context.attach", "context.grant", "context.revoke", "context.audit", "task.delegate", "task.list"} {
		out = append(out, toolDefinition{"meshra_collaboration_" + strings.Replace(op, ".", "_", 1), "Authorized durable collaboration operation", schema(map[string]any{"targetTaskId": str("Queued task binding target; mutually exclusive with conversationId"), "contextId": str("Context ID"), "version": map[string]any{"type": "integer"}, "content": str("UTF-8 content, at most 64 KiB; no secrets"), "type": str("Context type"), "scope": str("Metadata scope; not authorization"), "idempotencyKey": str("Deduplication key"), "conversationId": str("Conversation ID"), "keyword": str("Title keyword"), "from": str("Admin only sender; agent identity is authenticated"), "to": str("Recipient"), "instruction": str("Task instruction"), "contextRefs": map[string]any{"type": "array", "items": schema(map[string]any{"contextId": str("ID"), "version": map[string]any{"type": "integer"}}, "contextId", "version")}, "dependsOn": map[string]any{"type": "array", "items": str("Existing task ID")}, "timeoutSeconds": map[string]any{"type": "integer"}, "parentTaskId": str("Parent task"), "id": str("Resource ID"), "title": str("Title"), "agentId": str("Exact agent ID"), "participants": map[string]any{"type": "array", "items": str("Exact ID")}, "after": str("Cursor"), "limit": map[string]any{"type": "integer"}})})
	}
	return out
}
