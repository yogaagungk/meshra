package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"meshra/internal/model"
)

type admittedConnectionKey struct{}

// Caller holds relay ownership. Native harness provider names are not connection
// identifiers; endpoint and ACP providers resolve exact saved configuration IDs.
func (r *Relay) profileConnectionLocked(binding model.ProfileProvider, enabled bool) (any, []string, error) {
	if binding.Harness != "openai" && binding.Harness != "acp" {
		if binding.ConnectionID != "" {
			return nil, nil, model.Err(400, "native profile harness does not use a connectionId")
		}
		return nil, nil, nil
	}
	id := binding.Config.Provider
	if !profileIdentifier(id) || binding.ConnectionID != "" && binding.ConnectionID != id {
		return nil, nil, model.Err(400, "connectionId must match the configured provider identifier")
	}
	if binding.Harness == "openai" {
		endpoint, found, err := r.readEndpointLocked(context.Background(), id)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return nil, nil, model.Err(404, "endpoint not found")
		}
		if enabled && !endpoint.Enabled {
			return nil, nil, model.Err(409, "endpoint is disabled")
		}
		refs := []string{}
		if endpoint.SecretRef != "" {
			refs = append(refs, endpoint.SecretRef)
		}
		return endpoint, refs, nil
	}
	config, found, err := r.readACPLocked(context.Background(), id)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, model.Err(404, "ACP configuration not found")
	}
	if enabled && !config.Enabled {
		return nil, nil, model.Err(409, "ACP configuration is disabled")
	}
	refs := make([]string, 0, len(config.SecretEnvRefs))
	for _, reference := range config.SecretEnvRefs {
		_, id, ok := strings.Cut(reference, "=")
		if !ok {
			return nil, nil, model.Err(503, "ACP secret configuration is unavailable")
		}
		refs = append(refs, id)
	}
	return config, refs, nil
}
func (r *Relay) validateProfileConnectionLocked(binding model.ProfileProvider, enabled bool) error {
	_, _, err := r.profileConnectionLocked(binding, enabled)
	return err
}

func (r *Relay) secretRecordVersionLocked(ctx context.Context, id string) (string, error) {
	var nonce, ciphertext []byte
	if err := r.db.QueryRowContext(ctx, "SELECT nonce,ciphertext FROM automation_secrets WHERE id=?", id).Scan(&nonce, &ciphertext); err != nil {
		return "", model.Err(503, "connection credential is unavailable")
	}
	digest := sha256.New()
	_, _ = digest.Write(nonce)
	_, _ = digest.Write(ciphertext)
	return hex.EncodeToString(digest.Sum(nil)), nil
}
func (r *Relay) captureProfileConnectionLocked(snapshot *model.ProfileTaskSnapshot) error {
	config, refs, err := r.profileConnectionLocked(snapshot.Profile.Binding, false)
	if err != nil {
		return err
	}
	if config == nil {
		return nil
	}
	snapshot.Connection, err = json.Marshal(config)
	if err != nil {
		return err
	}
	if len(refs) != 0 {
		snapshot.SecretVersions = make(map[string]string, len(refs))
	}
	for _, id := range refs {
		version, err := r.secretRecordVersionLocked(context.Background(), id)
		if err != nil {
			return err
		}
		snapshot.SecretVersions[id] = version
	}
	return nil
}

// Hash only randomized authenticated ciphertext, not plaintext credentials. This
// records a vault revision without adding a password/API-key dictionary oracle.
func (r *Relay) validateSecretVersions(ctx context.Context, versions map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return model.Err(503, "relay is stopping")
	}
	for id, version := range versions {
		current, err := r.secretRecordVersionLocked(ctx, id)
		if err != nil || current != version {
			return model.Err(403, "admitted connection credential was revoked or replaced")
		}
	}
	return nil
}
