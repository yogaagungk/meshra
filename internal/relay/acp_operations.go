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

func acpStoredConfiguration(stored ACPAgentConfig) (acp.Config, error) {
	body, err := json.Marshal(stored)
	if err != nil {
		return acp.Config{}, err
	}
	config, err := acp.DecodeStored(body)
	if err != nil {
		return config, err
	}
	config.CWD = "/workspace"
	if config.Preset == "cline" {
		config.Env = map[string]string{"CLINE_SESSION_BACKEND_MODE": "local", "CLINE_NO_AUTO_UPDATE": "1"}
	}
	return config, nil
}

func (r *Relay) acpReadinessOperation(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	var q struct {
		ID               string `json:"id"`
		ExpectedRevision *int   `json:"expectedRevision,omitempty"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	stored, err := r.ACPAgent(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	if q.ExpectedRevision != nil && *q.ExpectedRevision != stored.Revision {
		return nil, model.Err(409, "ACP configuration revision changed")
	}
	runtime, _, err := r.runtimeConfig()
	if err != nil {
		return nil, err
	}
	status := sandbox.Check(ctx, runtime)
	artifactErr := acpRuntimeArtifacts(runtime, stored)
	if artifactErr != nil {
		status.State = "missing"
		status.Prerequisites = append(status.Prerequisites, artifactErr.Error())
	}
	if operation == "connections.acpStatus" {
		view, err := r.acpView(ctx, stored)
		if err != nil {
			return nil, err
		}
		return map[string]any{"configuration": view, "runtime": status, "verified": false}, nil
	}
	if status.State != "ready" {
		return nil, model.Err(503, "ACP check requires confined execution: "+strings.Join(status.Prerequisites, "; "))
	}
	config, err := acpStoredConfiguration(stored)
	if err != nil {
		return nil, err
	}
	// A check does not enable the saved configuration or make a model request.
	config.Enabled = true
	checkContext, release, err := r.beginConnectionProbe(ctx, stored.ID, 60*time.Second)
	if err != nil {
		return nil, err
	}
	defer release()
	active := func(requestContext context.Context) error {
		current, err := r.ACPAgent(requestContext, stored.ID)
		if err != nil {
			return err
		}
		if current.Revision != stored.Revision {
			return model.Err(409, "ACP configuration changed during check")
		}
		return nil
	}
	launcher := acpWorkerLauncher{relay: r, agent: model.Agent{ID: "acp-check-" + randomID()}, stored: stored, active: active, disposableState: true}
	inspection, probeErr := acp.Probe(checkContext, config, launcher, "")
	inspection = redactACPInspection(inspection)
	if probeErr != nil {
		// Native errors can contain browser/device authorization URLs or codes.
		// Return negotiation facts, never untrusted native diagnostics.
		return map[string]any{"id": stored.ID, "revision": stored.Revision, "status": "failed", "inspection": inspection, "error": "ACP initialize/session check failed; use a supported ACP v1 CLI and explicitly selected native authentication"}, nil
	}
	return map[string]any{"id": stored.ID, "revision": stored.Revision, "status": "ready", "inspection": inspection}, nil
}
