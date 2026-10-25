package relay

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"time"

	"meshra/internal/adapters/openai"
	"meshra/internal/model"
	"meshra/internal/netpolicy"
)

// EndpointRuntimeConfig pins inference to one saved endpoint and its exact
// reviewed network origin. The credential never crosses to a different origin.
func (r *Relay) EndpointRuntimeConfig(ctx context.Context, id string) (openai.Config, error) {
	if snapshot, ok := ctx.Value(admittedConnectionKey{}).(*model.ProfileTaskSnapshot); ok {
		if snapshot.Binding.Harness != "openai" || snapshot.Binding.Config.Provider != id || len(snapshot.Connection) == 0 {
			return openai.Config{}, model.Err(503, "admitted endpoint configuration is unavailable")
		}
		var endpoint ProviderEndpoint
		if err := json.Unmarshal(snapshot.Connection, &endpoint); err != nil || endpoint.ID != id {
			return openai.Config{}, model.Err(503, "admitted endpoint configuration is invalid")
		}
		if err := r.validateSecretVersions(ctx, snapshot.SecretVersions); err != nil {
			return openai.Config{}, err
		}
		return r.endpointConfig(ctx, endpoint)
	}
	endpoint, err := r.ProviderEndpoint(ctx, id)
	if err != nil {
		return openai.Config{}, err
	}
	if !endpoint.Enabled {
		return openai.Config{}, model.Err(409, "endpoint is disabled")
	}
	return r.endpointConfig(ctx, endpoint)
}

func (r *Relay) endpointConfig(ctx context.Context, endpoint ProviderEndpoint) (openai.Config, error) {
	config := openai.Config{BaseURL: endpoint.BaseURL, SupportsTools: endpoint.SupportsTools, SupportsImages: endpoint.SupportsImages, ContextWindowTokens: endpoint.ContextWindowTokens}
	var err error
	if endpoint.SecretRef != "" {
		config.APIKey, err = r.readSecret(ctx, endpoint.SecretRef)
		if err != nil {
			return openai.Config{}, err
		}
	}
	origin, err := EndpointOriginOf(endpoint)
	if err != nil {
		return openai.Config{}, err
	}
	config.HTTPClient, err = r.networkClient(netpolicy.Scope{ConnectionID: endpoint.ID}, netpolicy.ClientOptions{
		Origin: origin.String(), CredentialOrigin: origin.String(), Timeout: 5 * time.Minute, MaxResponseBytes: openai.MaxResponseBytes,
		Active: func(requestContext context.Context) error {
			if snapshot, ok := ctx.Value(admittedConnectionKey{}).(*model.ProfileTaskSnapshot); ok {
				if err := r.validateSecretVersions(requestContext, snapshot.SecretVersions); err != nil {
					return err
				}
			}
			currentEndpoint, err := r.ProviderEndpoint(requestContext, endpoint.ID)
			if err != nil || currentEndpoint.SecretRef != endpoint.SecretRef {
				return model.Err(403, "endpoint credential grant was revoked or replaced")
			}
			if err := requestContext.Err(); err != nil {
				return err
			}
			if endpoint.SecretRef == "" {
				return nil
			}
			current, err := r.readSecret(requestContext, endpoint.SecretRef)
			if err != nil || subtle.ConstantTimeCompare([]byte(current), []byte(config.APIKey)) != 1 {
				return model.Err(403, "endpoint credential revoked or replaced")
			}
			return nil
		},
	})
	return config, err
}
