package relay

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"meshra/internal/adapters/acp"
	"meshra/internal/model"
	"meshra/internal/sandbox"
)

// Authentication state belongs to an exact profile/conversation binding, never
// a shared native HOME that could disclose another conversation's history.
// This operation initializes/authenticates only; it never sends a prompt.
func (r *Relay) authenticateACP(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	var q struct {
		ID               string `json:"id"`
		ExpectedRevision int    `json:"expectedRevision"`
		ProfileID        string `json:"profileId"`
		ConversationID   string `json:"conversationId"`
		MethodID         string `json:"methodId"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) || !profileIdentifier(q.ProfileID) || !profileIdentifier(q.ConversationID) || q.ExpectedRevision < 1 || q.MethodID == "" || len(q.MethodID) > 128 || !safeRegistryText(q.MethodID) {
		return nil, model.Err(400, "exact ACP configuration, binding and advertised method are required")
	}
	r.mu.Lock()
	stored, found, err := r.readACPLocked(ctx, q.ID)
	if err == nil && !found {
		err = model.Err(404, "ACP configuration not found")
	}
	if err == nil && stored.Revision != q.ExpectedRevision {
		err = model.Err(409, "ACP configuration revision changed")
	}
	if err == nil {
		profile, profileErr := r.profile(q.ProfileID)
		if profileErr != nil {
			err = profileErr
		} else if profile.Archived {
			err = model.Err(409, "profile is archived")
		}
	}
	var binding model.ProfileBinding
	if err == nil {
		binding, err = r.profileBinding(q.ProfileID, q.ConversationID)
	}
	if err == nil && (binding.Harness != "acp" || binding.Config.Provider != q.ID) {
		err = model.Err(403, "binding does not use this ACP configuration")
	}
	var agent model.Agent
	if err == nil {
		agent, err = r.agent(binding.AgentID)
	}
	if err == nil {
		var busy bool
		err = r.db.QueryRow("SELECT EXISTS(SELECT 1 FROM tasks WHERE recipient=? AND state NOT IN ('completed','failed','cancelled','timed_out'))", binding.AgentID).Scan(&busy)
		if err == nil && busy {
			err = model.Err(409, "ACP binding has outstanding work")
		}
	}
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	runtime, _, err := r.runtimeConfig()
	if err != nil {
		return nil, err
	}
	if err = acpRuntimeArtifacts(runtime, stored); err != nil {
		return nil, err
	}
	status := sandbox.Check(ctx, runtime)
	if status.State != "ready" {
		return nil, model.Err(503, "ACP authentication requires confined execution: "+strings.Join(status.Prerequisites, "; "))
	}
	jobContext, releaseOwner, err := r.beginConsoleJob(ctx, p.ConsoleSessionID)
	if err != nil {
		return nil, err
	}
	defer releaseOwner()
	probeContext, releaseProbe, err := r.beginConnectionProbe(jobContext, stored.ID, 60*time.Second)
	if err != nil {
		return nil, err
	}
	defer releaseProbe()
	config, err := acpStoredConfiguration(stored)
	if err != nil {
		return nil, err
	}
	config.Enabled = true
	active := func(requestContext context.Context) error {
		if err := r.requireConsole(p); err != nil {
			return err
		}
		current, err := r.ACPAgent(requestContext, stored.ID)
		if err != nil {
			return err
		}
		if current.Revision != stored.Revision {
			return model.Err(409, "ACP configuration changed during authentication")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		currentBinding, err := r.profileBinding(q.ProfileID, q.ConversationID)
		if err != nil {
			return err
		}
		if currentBinding.ID != binding.ID {
			return model.Err(409, "ACP binding changed during authentication")
		}
		return nil
	}
	launcher := acpWorkerLauncher{relay: r, agent: agent, stored: stored, active: active}
	inspection, probeErr := acp.Probe(probeContext, config, launcher, q.MethodID)
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	if probeErr == nil {
		if err := active(probeContext); err != nil {
			return nil, err
		}
	}
	result := map[string]any{"id": stored.ID, "revision": stored.Revision, "bindingId": binding.ID, "inspection": redactACPInspection(inspection), "status": "ready"}
	if probeErr != nil {
		result["status"] = "failed"
		result["error"] = "ACP authentication did not complete; use an advertised method supported by this confined CLI or explicitly selected native sign-in files"
	}
	// Do not return native diagnostics: they can contain device/browser codes.
	return result, nil
}

func redactACPInspection(value acp.Inspection) acp.Inspection {
	clean := value.AuthMethods[:0]
	for _, method := range value.AuthMethods {
		if redact(method.ID) != method.ID {
			continue
		}
		method.Name = redact(method.Name)
		method.Description = redact(method.Description)
		clean = append(clean, method)
	}
	value.AuthMethods = clean
	return value
}
