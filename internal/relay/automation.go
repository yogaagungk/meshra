package relay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"

	"meshra/internal/model"
)

// admissionMeta is constructed only by trusted application operations. Reads and
// authorization run before Begin; persist may use only the supplied transaction.
type admissionMeta struct {
	profile           *model.ProfileTaskSnapshot
	sourceRequestHash string
	generationAgentID string
	ingress           *ingressGrant
	validate          func(model.Principal, model.SendRequest) error
	persist           func(*sql.Tx, model.Task) error
}

func decodeAutomation(raw json.RawMessage, dst any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return model.Err(400, "payload must be an object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return model.Err(400, "invalid automation payload: "+err.Error())
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return model.Err(400, "payload must contain one JSON object")
	}
	return nil
}

func (r *Relay) Automation(ctx context.Context, p model.Principal, operation string, payload json.RawMessage) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.RoutineTokenID != "" && operation != "routines.run" {
		return nil, model.Err(403, "routine credentials may only run their granted routines")
	}
	switch operation {
	case "profiles.list", "profiles.get", "profiles.save", "profiles.archive":
		return r.profileStoreOperation(p, operation, payload)
	case "profiles.createConversation", "profiles.conversations", "profiles.bindingAction", "profiles.switch", "profiles.switchPreview", "profiles.generate", "profiles.generationDraft":
		return r.profileContinuityOperation(ctx, p, operation, payload)
	case "profiles.binding", "skills.bindings":
		return r.automationBindingQuery(ctx, p, operation, payload)
	case "profiles.send":
		return r.profileSend(ctx, p, payload)
	case "queue.list", "queue.pause", "queue.resume", "queue.reorder":
		return r.queueOperation(p, operation, payload)
	case "history.search", "history.read":
		return r.profileHistoryOperation(ctx, p, operation, payload)
	case "memory.list", "memory.save", "memory.remove":
		return r.profileMemoryOperation(ctx, p, operation, payload)
	case "attachments.list", "attachments.remove":
		return r.attachmentOperation(p, operation, payload)
	case "profiles.resetContext":
		return r.profileResetContext(p, payload)
	case "usage.list":
		return r.automationUsage(p, payload)
	case "runtime.status", "runtime.configure":
		return r.runtimeOperation(ctx, p, operation, payload)
	case "network.list", "network.grant", "network.revoke":
		return r.networkOperation(ctx, p, operation, payload)
	case "connections.list", "connections.get", "connections.models", "connections.saveEndpoint", "connections.disable", "connections.remove", "connections.saveAcp", "connections.disableAcp", "connections.removeAcp", "connections.scanLocal":
		return r.connectionsOperation(ctx, p, operation, payload)
	case "connections.checkAcp", "connections.acpStatus", "connections.authenticateAcp":
		return r.connectionsOperation(ctx, p, operation, payload)
	case "changesets.apply", "changesets.get", "changesets.list":
		return r.changesetOperation(ctx, p, operation, payload)
	case "mcp.list", "mcp.save", "mcp.test", "mcp.setEnabled", "mcp.bind", "mcp.remove", "mcp.authStart", "mcp.authStatus", "mcp.signOut":
		return r.mcpClientOperation(ctx, p, operation, payload)
	case "routines.list", "routines.save", "routines.preview", "routines.setEnabled", "routines.run", "routines.runs", "routines.cancelRun", "flows.get", "flows.save", "routineTokens.create", "routineTokens.list", "routineTokens.revoke":
		return r.routineOperation(ctx, p, operation, payload)
	case "skills.list", "skills.read", "skills.save", "skills.bind", "skills.remove":
		return r.skillOperation(ctx, p, operation, payload)
	case "templates.list", "templates.export", "templates.importPreview", "templates.import":
		return r.templateOperation(ctx, p, operation, payload)
	case "connectors.list", "connectors.save", "connectors.test", "connectors.setEnabled", "connectors.routesSave", "connectors.remove", "connectors.deliveries", "connectors.retryDelivery":
		return r.connectorOperation(ctx, p, operation, payload)
	case "browser.open", "browser.state", "browser.sessions", "browser.takeover", "browser.release", "browser.input", "browser.close", "browser.clearData":
		return r.browserOperation(ctx, p, operation, payload)
	case "voice.configure", "voice.status", "voice.transcribe", "voice.synthesize", "voice.job", "voice.cancel":
		return r.voiceOperation(ctx, p, operation, payload)
	case "approvals.list", "approvals.decide", "questions.answer":
		return r.approvalOperation(ctx, p, operation, payload)
	}
	return nil, model.Err(404, "unknown automation operation")
}

// Session callbacks are deliberately separate from relay ownership: validating a
// console session must never invert the API session mutex and r.mu.
func (r *Relay) SetConsoleSessionValidator(validate func(string) bool) {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	r.consoleSessionValidator = validate
}

func (r *Relay) RevokeConsoleSession(id string) {
	r.sessionMu.Lock()
	// Release session ownership before touching relay state or canceling jobs.
	if r.revokedConsoleSessions == nil {
		r.revokedConsoleSessions = make(map[string]bool)
	}
	r.revokedConsoleSessions[id] = true
	jobs := r.consoleJobs[id]
	delete(r.consoleJobs, id)
	r.consoleJobCount -= len(jobs)
	r.sessionMu.Unlock()
	for _, cancel := range jobs {
		cancel()
	}
	r.mu.Lock()
	r.revokeToolSessionLocked(id)
	if err := r.revokeMCPAuthFlowsLocked(id); err != nil {
		r.logger.Warn("OAuth flow cleanup failed after session revocation")
	}
	r.mu.Unlock()
	if err := r.revokeVoiceSession(id); err != nil && r.ctx.Err() == nil {
		r.logger.Warn("local voice cleanup failed after session revocation")
	}
	if err := r.revokeBrowserSession(id); err != nil {
		r.logger.Warn("browser control cleanup failed after session revocation")
	}
}

func (r *Relay) requireConsole(p model.Principal) error {
	if !p.Admin || p.ConsoleSessionID == "" {
		return model.Err(403, "live console session required")
	}
	r.sessionMu.Lock()
	validate := r.consoleSessionValidator
	revoked := r.revokedConsoleSessions[p.ConsoleSessionID]
	r.sessionMu.Unlock()
	if revoked || validate == nil || !validate(p.ConsoleSessionID) {
		return model.Err(403, "console session is no longer active")
	}
	r.sessionMu.Lock()
	revoked = r.revokedConsoleSessions[p.ConsoleSessionID]
	r.sessionMu.Unlock()
	if revoked {
		return model.Err(403, "console session is no longer active")
	}
	return nil
}
