package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"meshra/internal/externalmcp"
	"meshra/internal/model"
)

func mcpDigest(value any) string {
	body, _ := json.Marshal(value)
	return tokenHash(string(body))
}

// Enabled is a dispatch gate, not an OAuth credential audience.
func mcpCredentialDigest(config externalmcp.Config) string {
	config.Enabled = false
	return mcpDigest(config)
}

func (r *Relay) mcpAuthorizationID(serverID string) (string, error) {
	var id string
	err := r.db.QueryRow("SELECT COALESCE((SELECT json_extract(body,'$.authorizationId') FROM mcp_auth_tokens WHERE server_id=?),'')", serverID).Scan(&id)
	return id, err
}

// externalMCPReferences runs under relay ownership at admission, never from model input.
func (r *Relay) externalMCPReferences(profileID string) ([]model.ExternalMCPReference, error) {
	rows, err := r.db.Query("SELECT server_id FROM profile_mcp_bindings WHERE profile_id=? AND enabled=1 ORDER BY server_id", profileID)
	if err != nil { return nil, err }
	var ids []string
	for rows.Next() { var id string; if err = rows.Scan(&id); err != nil { break }; ids = append(ids, id) }
	if err == nil { err = rows.Err() }; rows.Close()
	if err != nil { return nil, err }
	var refs []model.ExternalMCPReference
	for _, id := range ids {
		config, err := r.mcpServer(id); if err != nil { return nil, err }
		binding, err := r.mcpBinding(profileID, id); if err != nil { return nil, err }
		if !config.Enabled || binding.Revision != config.Revision { continue }
		authorizationID, err := r.mcpAuthorizationID(id)
		if err != nil { return nil, err }
		versions := make(map[string]string)
		for _, ref := range append(append([]string{}, config.SecretHeaderRefs...), config.SecretEnvRefs...) {
			_, secretID, ok := strings.Cut(ref, ":")
			if !ok || secretID == "" { return nil, model.Err(400, "invalid external MCP secret reference") }
			version, err := r.secretRecordVersionLocked(context.Background(), secretID)
			if err != nil { return nil, err }
			versions[secretID] = version
		}
		for _, tool := range binding.Tools {
			refs = append(refs, model.ExternalMCPReference{ServerID: id, ServerRevision: config.Revision, ServerDigest: mcpDigest(config), BindingDigest: mcpDigest(binding), Tool: tool.Name, ToolDigest: tool.Digest, AuthorizationID: authorizationID, SecretVersions: versions})
		}
	}
	return refs, nil
}

func validateExternalMCPRequest(request *externalMCPRequest) error {
	if !profileIdentifier(request.ServerID) || request.Tool == "" || !externalmcp.SafeText(request.Tool) || len(request.Tool) > 200 { return model.Err(400, "invalid external MCP call") }
	if len(request.Arguments) > mcpMaxArguments { return model.Err(413, "external MCP arguments exceed 64 KiB") }
	if len(bytes.TrimSpace(request.Arguments)) == 0 { return model.Err(400, "external MCP arguments must be an object") }
	arguments, err := canonicalToolArguments(request.Arguments)
	if err != nil { return err }; request.Arguments = arguments
	return nil
}

// externalMCPCallLocked requires the live in-memory broker and its claimed action.
// Database rows alone, including fabricated approved rows, confer no authority.
func (r *Relay) externalMCPCallLocked(principal model.Principal, profileID, invocationID string, request externalMCPRequest) (*toolBroker, *brokerCall, externalmcp.Config, model.ExternalMCPReference, error) {
	var config externalmcp.Config
	var pin model.ExternalMCPReference
	b := r.toolBrokers[principal.TaskID]
	if b == nil || b.snapshot.Profile.ID != profileID { return nil, nil, config, pin, model.Err(403, "active external MCP broker required") }
	_, active, epoch, err := b.activeLocked()
	if err != nil { return nil, nil, config, pin, err }
	if active != principal { return nil, nil, config, pin, model.Err(403, "external MCP principal belongs to another attempt") }
	var call *brokerCall
	for _, candidate := range b.calls { if candidate.invocation.ID == invocationID { call = candidate; break } }
	if call == nil || call.done || call.invocation.Name != "meshra_external_mcp" || call.invocation.State != "claimed" || call.invocation.Epoch != epoch || call.approvalID == "" || call.owner == "" || !call.action.approval { return nil, nil, config, pin, model.Err(403, "external MCP requires a live claimed approval") }
	input, ok := call.action.args.(*externalMCPRequest)
	if !ok || input.ServerID != request.ServerID || input.Tool != request.Tool || !bytes.Equal(input.Arguments, request.Arguments) { return nil, nil, config, pin, model.Err(403, "external MCP call differs from its approved action") }
	profile, err := r.profile(profileID)
	if err != nil { return nil, nil, config, pin, err }
	if profile.Archived || !profile.Capabilities.ExternalMCP || !b.snapshot.Profile.Capabilities.ExternalMCP { return nil, nil, config, pin, model.Err(403, "external MCP capability is disabled") }
	for _, ref := range b.snapshot.ExternalMCP { if ref.ServerID == request.ServerID && ref.Tool == request.Tool { pin = ref; break } }
	if pin.ToolDigest == "" { return nil, nil, config, pin, model.Err(403, "external MCP tool was not pinned at admission") }
	config, err = r.mcpServer(request.ServerID)
	if err != nil { return nil, nil, config, pin, err }
	binding, err := r.mcpBinding(profileID, request.ServerID)
	if err != nil { return nil, nil, config, pin, err }
	if !config.Enabled || !binding.Enabled { return nil, nil, config, pin, model.Err(403, "external MCP server or binding is disabled") }
	if config.Revision != pin.ServerRevision || binding.Revision != config.Revision || mcpDigest(config) != pin.ServerDigest || mcpDigest(binding) != pin.BindingDigest { return nil, nil, config, pin, model.Err(409, "external MCP server or binding changed after admission") }
	authorizationID, err := r.mcpAuthorizationID(request.ServerID)
	if err != nil { return nil, nil, config, pin, err }
	if authorizationID != pin.AuthorizationID { return nil, nil, config, pin, model.Err(403, "admitted MCP authorization was revoked or replaced") }
	for id, version := range pin.SecretVersions {
		current, err := r.secretRecordVersionLocked(context.Background(), id)
		if err != nil || current != version { return nil, nil, config, pin, model.Err(403, "admitted MCP credential was revoked or replaced") }
	}
	return b, call, config, pin, nil
}

// cancelExternalMCPLocked invalidates pending approvals and in-flight calls, including stdio.
func (r *Relay) cancelExternalMCPLocked(serverID, profileID string) {
	for _, b := range r.toolBrokers {
		if profileID != "" && b.snapshot.Profile.ID != profileID { continue }
		for _, call := range b.calls {
			input, ok := call.action.args.(*externalMCPRequest)
			if ok && input.ServerID == serverID && !call.done { call.cancel() }
		}
	}
}

func (r *Relay) recheckExternalMCP(ctx context.Context, principal model.Principal, profileID, invocationID string, request externalMCPRequest) error {
	if err := ctx.Err(); err != nil { return err }
	r.mu.Lock()
	b, call, _, _, err := r.externalMCPCallLocked(principal, profileID, invocationID, request)
	r.mu.Unlock()
	if err != nil { return err }
	return b.claim(call)
}
