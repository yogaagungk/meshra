package api

import (
	"bytes"
	"encoding/json"
	"net/http"

	"meshra/internal/model"
)

// Operation-specific decoding and authorization belong to the optional service.
func (s *server) automation(w http.ResponseWriter, r *http.Request, p model.Principal, operation string) {
	if r.Method != http.MethodPost {
		method(w, "POST")
		return
	}
	var payload json.RawMessage
	if !decode(w, r, &payload) {
		return
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		fail(w, model.Err(400, "automation payload must be a JSON object"))
		return
	}
	svc, ok := s.service.(model.AutomationService)
	if !ok {
		fail(w, model.Err(503, "automation unavailable"))
		return
	}
	result, err := svc.Automation(r.Context(), p, operation, payload)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, automationStatus(operation, payload), result)
}

func automationStatus(operation string, payload json.RawMessage) int {
	switch operation {
	case "profiles.send", "profiles.generate", "routines.run", "voice.transcribe", "voice.synthesize":
		return http.StatusAccepted
	case "profiles.createConversation", "routineTokens.create", "templates.import", "browser.open":
		return http.StatusCreated
	case "profiles.save", "memory.save", "flows.save", "skills.save", "network.grant", "connections.saveAcp", "connections.saveEndpoint", "mcp.save", "routines.save", "connectors.save":
		// Successful updates require a positive optimistic revision. A caller
		// may choose a new ID, so ID presence cannot distinguish creation.
		var revision struct {
			ExpectedRevision int `json:"expectedRevision"`
		}
		_ = json.Unmarshal(payload, &revision)
		if revision.ExpectedRevision > 0 {
			return http.StatusOK
		}
		return http.StatusCreated
	default:
		return http.StatusOK
	}
}
