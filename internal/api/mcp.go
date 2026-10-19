package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

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
	return []toolDefinition{
		{"meshra_agents_list", "List agents visible to this authenticated identity", schema(map[string]any{})},
		{"meshra_agent_status", "Inspect a named agent", schema(map[string]any{"id": str("Canonical agent ID")}, "id")},
		{"meshra_send", "Queue a message; returns a persistent task immediately", schema(send, "to", "text")},
		{"meshra_request", "Queue a request; poll its task for the reply (never blocks a turn)", schema(send, "to", "text")},
		{"meshra_task_status", "Inspect a task visible to this identity", schema(map[string]any{"taskId": str("Task ID")}, "taskId")},
		{"meshra_task_events", "Read persistent events strictly after a cursor", schema(map[string]any{"taskId": str("Task ID"), "after": map[string]any{"type": "integer", "minimum": 0}}, "taskId")},
		{"meshra_task_cancel", "Cancel an authorized task", schema(map[string]any{"taskId": str("Task ID")}, "taskId")},
	}
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
		result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]string{"name": "meshra", "version": "0.1.0"}, "instructions": "Requests are asynchronous. Use task status/events to read results. Caller identity comes only from bearer authentication."}
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
	switch name {
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
	case "meshra_task_status", "meshra_task_events", "meshra_task_cancel":
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
		default:
			events, err := s.service.Events(p, args.TaskID, args.After)
			return map[string]any{"events": events}, err
		}
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}
