package acp

import (
	"context"
	"errors"
	"time"

	"meshra/internal/model"
)

type AuthenticationMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}
type Inspection struct {
	ProtocolVersion int                    `json:"protocolVersion"`
	SupportsLoad    bool                   `json:"supportsLoad"`
	AuthMethods     []AuthenticationMethod `json:"authMethods"`
	Ready           bool                   `json:"ready"`
}

// Probe initializes a confined agent and creates an empty session. It never
// sends session/prompt. Authentication occurs only for an explicit advertised
// method; normal checks use any existing selected native authentication.
func Probe(ctx context.Context, config Config, launcher Launcher, method string) (Inspection, error) {
	result := Inspection{AuthMethods: []AuthenticationMethod{}}
	if launcher == nil {
		return result, errors.New("ACP probing requires a confined launcher")
	}
	if err := validateConfig(config); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	transport, err := launcher.Launch(ctx, config)
	if err != nil {
		return result, err
	}
	native := &runtime{cfg: config, agent: model.Agent{Cwd: config.CWD}}
	native.conn = newConnection(transport, native)
	defer native.Close()
	initialized, err := native.initialize(ctx)
	if err != nil {
		return result, err
	}
	result.ProtocolVersion = initialized.ProtocolVersion
	result.SupportsLoad = initialized.AgentCapabilities.LoadSession
	for _, advertised := range initialized.AuthMethods {
		result.AuthMethods = append(result.AuthMethods, AuthenticationMethod{ID: advertised.ID, Name: advertised.Name, Description: advertised.Description})
	}
	native.cfg.AuthMethodID = method
	if err = native.authenticate(ctx, initialized.AuthMethods); err != nil {
		return result, err
	}
	if err = native.openSession(ctx); err != nil {
		return result, err
	}
	result.Ready = true
	return result, nil
}
