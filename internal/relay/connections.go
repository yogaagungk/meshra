package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"meshra/internal/adapters/openai"
	"meshra/internal/model"
	"meshra/internal/netpolicy"
)

// ProviderEndpoint is an operator reviewed OpenAI-compatible endpoint. The
// credential is referenced, never embedded: secretRef names a vault record.
type ProviderEndpoint struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	Name     string `json:"name"`
	BaseURL  string `json:"baseUrl"`
	// APIFormat is the wire protocol: openai (default), anthropic or typesafe.
	APIFormat           string `json:"apiFormat,omitempty"`
	Enabled             bool   `json:"enabled"`
	SecretRef           string `json:"secretRef,omitempty"`
	AllowInsecureHTTP   bool   `json:"allowInsecureHttp,omitempty"`
	SupportsTools       bool   `json:"supportsTools"`
	SupportsImages      bool   `json:"supportsImages"`
	ContextWindowTokens int    `json:"contextWindowTokens,omitempty"`
	PromptCacheKey      bool   `json:"promptCacheKey,omitempty"`
	PromptCaching       string `json:"promptCaching,omitempty"`
	CreatedAt           string `json:"createdAt"`
	UpdatedAt           string `json:"updatedAt"`
}

// ProviderEndpointView is the operator summary. It never contains a secret
// value, only whether the referenced record is missing, saved or unreadable.
type ProviderEndpointView struct {
	ProviderEndpoint
	SecretStatus string `json:"secretStatus"`
}

// ConnectionList is the read summary of every configured connection.
type ConnectionList struct {
	Endpoints []ProviderEndpointView `json:"endpoints"`
}

// LocalModelScan reports exactly what one local probe observed. It is not
// authentication and it is not a statement about model capabilities.
type LocalModelScan struct {
	URL    string   `json:"url"`
	Status string   `json:"status"`
	Models []string `json:"models,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// The probe list is exactly two loopback endpoints. It is a variable so that a
// test can bind a fixture server; no configuration can extend it and no
// subnet is ever scanned.
var localModelProbeURLs = []string{"http://127.0.0.1:11434/v1/models", "http://127.0.0.1:1234/v1/models"}

// endpointRequest is the submitted endpoint definition. Absent fields keep
// their stored value, so a partial edit cannot silently clear a reviewed flag.
type endpointRequest struct {
	ID                  string  `json:"id,omitempty"`
	Revision            int     `json:"revision,omitempty"`
	Name                string  `json:"name"`
	BaseURL             string  `json:"baseUrl"`
	APIFormat           string  `json:"apiFormat,omitempty"`
	Enabled             *bool   `json:"enabled,omitempty"`
	SecretRef           string  `json:"secretRef,omitempty"`
	AllowInsecureHTTP   bool    `json:"allowInsecureHttp,omitempty"`
	SupportsTools       *bool   `json:"supportsTools,omitempty"`
	SupportsImages      *bool   `json:"supportsImages,omitempty"`
	ContextWindowTokens *int    `json:"contextWindowTokens,omitempty"`
	PromptCacheKey      *bool   `json:"promptCacheKey,omitempty"`
	PromptCaching       *string `json:"promptCaching,omitempty"`
}

// connectionsOperation serves the operator only connection operations.
func (r *Relay) connectionsOperation(ctx context.Context, principal model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireAdmin(principal); err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) || len(raw) > 64<<10 {
		return nil, model.Err(400, "invalid connection payload")
	}
	switch operation {
	case "connections.list":
		return r.listConnections(ctx, raw)
	case "connections.get":
		return r.getConnection(ctx, raw)
	case "connections.models":
		return r.listEndpointModels(ctx, raw)
	case "connections.saveEndpoint":
		return r.saveEndpoint(ctx, raw)
	case "connections.disable":
		return r.disableEndpoint(ctx, raw)
	case "connections.remove":
		return r.removeEndpoint(ctx, raw)
	case "connections.scanLocal":
		results, err := r.scanLocalConnections(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"results": results}, nil
	}
	return nil, model.Err(404, "unknown connection operation")
}

// localOrLANHost reports whether a host is an explicitly local or LAN address.
// Plain HTTP is accepted only for those hosts and only when the operator
// confirmed it.
func localOrLANHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return false
}

// normalizeEndpointURL validates a base URL and returns its stored form and
// its exact origin. Credentials, query strings and fragments are rejected.
func normalizeEndpointURL(raw string, allowInsecureHTTP bool) (string, netpolicy.Origin, error) {
	if raw == "" || len(raw) > 2048 {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl must be 1..2048 bytes")
	}
	if !utf8.ValidString(raw) || !safeRegistryText(raw) {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl must be valid UTF-8 without control characters")
	}
	if strings.Contains(raw, "#") {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl must not contain a fragment")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl is not a valid URL")
	}
	if parsed.User != nil {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl must not contain a query string")
	}
	if parsed.Fragment != "" {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl must not contain a fragment")
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl must be an absolute http or https URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl must use http or https")
	}
	origin, err := netpolicy.ParseOrigin(scheme + "://" + parsed.Host)
	if err != nil {
		return "", netpolicy.Origin{}, model.Err(400, "baseUrl host is invalid")
	}
	if scheme == "http" {
		if !allowInsecureHTTP {
			return "", netpolicy.Origin{}, model.Err(400, "http requires an explicit local or LAN confirmation")
		}
		if !localOrLANHost(origin.Host) {
			return "", netpolicy.Origin{}, model.Err(400, "http is only allowed for an explicitly local or LAN address")
		}
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	return origin.String() + path, origin, nil
}

// EndpointOriginOf returns the exact origin of a stored endpoint.
func EndpointOriginOf(endpoint ProviderEndpoint) (netpolicy.Origin, error) {
	if endpoint.BaseURL == "" {
		return netpolicy.Origin{}, model.Err(404, "endpoint not found")
	}
	parsed, err := url.Parse(endpoint.BaseURL)
	if err != nil || parsed.Host == "" {
		return netpolicy.Origin{}, model.Err(400, "endpoint base URL is invalid")
	}
	return netpolicy.ParseOrigin(strings.ToLower(parsed.Scheme) + "://" + parsed.Host)
}

// EndpointURL joins a relative path onto the endpoint base URL and refuses a
// path that escapes the reviewed base prefix.
func EndpointURL(base, relative string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return "", model.Err(400, "endpoint base URL is invalid")
	}
	reference, err := url.Parse(relative)
	if err != nil || reference.IsAbs() || reference.Host != "" {
		return "", model.Err(400, "endpoint path must be relative")
	}
	if strings.Contains(relative, "..") {
		return "", model.Err(400, "endpoint path must not traverse")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/" + strings.TrimPrefix(reference.Path, "/")
	parsed.RawQuery = reference.RawQuery
	parsed.Fragment = ""
	return parsed.String(), nil
}

// EndpointModelsURL returns the model listing URL of an endpoint. The
// reviewed base URL is the endpoint root: the standard "/v1" suffix is added
// once and never doubled.
func EndpointModelsURL(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", model.Err(400, "endpoint base URL is invalid")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		return EndpointURL(base, "models")
	}
	return EndpointURL(base, "v1/models")
}

// EndpointChatCompletionsURL returns the chat completion URL of an endpoint.
func EndpointChatCompletionsURL(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", model.Err(400, "endpoint base URL is invalid")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		return EndpointURL(base, "chat/completions")
	}
	return EndpointURL(base, "v1/chat/completions")
}

func validateEndpointDefinition(endpoint *ProviderEndpoint) error {
	name := strings.TrimSpace(endpoint.Name)
	if name == "" || len(name) > 128 || !safeRegistryText(name) || redact(name) != name {
		return model.Err(400, "endpoint name must be 1..128 bytes without credentials or control characters")
	}
	endpoint.Name = name
	base, _, err := normalizeEndpointURL(endpoint.BaseURL, endpoint.AllowInsecureHTTP)
	if err != nil {
		return err
	}
	endpoint.BaseURL = base
	if !openai.ValidFormat(endpoint.APIFormat) {
		return model.Err(400, "apiFormat must be openai, anthropic or typesafe")
	}
	if endpoint.APIFormat == "" {
		endpoint.APIFormat = openai.FormatOpenAI
	}
	if endpoint.APIFormat == openai.FormatTypeSafe {
		// System One answers typed questions; it never generates text or calls
		// tools, so it cannot back a chat turn.
		endpoint.SupportsTools = false
		endpoint.SupportsImages = false
	}
	if endpoint.SecretRef != "" && !profileIdentifier(endpoint.SecretRef) {
		return model.Err(400, "invalid secret reference")
	}
	if endpoint.ContextWindowTokens < 0 || endpoint.ContextWindowTokens > 10_000_000 {
		return model.Err(400, "contextWindowTokens is out of range")
	}
	switch endpoint.PromptCaching {
	case "", "auto", "off", "1h":
	default:
		return model.Err(400, "promptCaching must be auto, off or 1h")
	}
	return nil
}

func (r *Relay) readEndpointLocked(ctx context.Context, id string) (ProviderEndpoint, bool, error) {
	var body string
	err := r.db.QueryRowContext(ctx, "SELECT body FROM provider_endpoints WHERE id=?", id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderEndpoint{}, false, nil
	}
	if err != nil {
		return ProviderEndpoint{}, false, err
	}
	var endpoint ProviderEndpoint
	if err = json.Unmarshal([]byte(body), &endpoint); err != nil {
		return ProviderEndpoint{}, false, err
	}
	return endpoint, true, nil
}

// connectionInUseLocked reports whether a connection is referenced by an agent
// or a profile binding. A referenced connection cannot be removed.
func (r *Relay) connectionInUseLocked(ctx context.Context, id string) (bool, error) {
	queries := []string{
		"SELECT count(*) FROM profile_bindings WHERE json_extract(body,'$.connectionId')=?",
		"SELECT count(*) FROM profile_task_snapshots WHERE json_extract(body,'$.binding.connectionId')=?",
		"SELECT count(*) FROM agents WHERE json_extract(body,'$.config.provider')=?",
	}
	for _, query := range queries {
		var count int
		if err := r.db.QueryRowContext(ctx, query, id).Scan(&count); err != nil {
			return false, err
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (r *Relay) endpointView(ctx context.Context, endpoint ProviderEndpoint) (ProviderEndpointView, error) {
	status, err := r.secretStatus(ctx, endpoint.SecretRef)
	if err != nil {
		return ProviderEndpointView{}, err
	}
	return ProviderEndpointView{ProviderEndpoint: endpoint, SecretStatus: status}, nil
}

func (r *Relay) listConnections(ctx context.Context, raw json.RawMessage) (any, error) {
	var filter struct{}
	if err := decodeAutomation(raw, &filter); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	// Rows are drained before any further query: the relay owns a single
	// database connection and a nested read would wait for itself.
	list := ConnectionList{Endpoints: []ProviderEndpointView{}}
	endpoints := []ProviderEndpoint{}
	rows, err := r.db.QueryContext(ctx, "SELECT body FROM provider_endpoints ORDER BY id")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			rows.Close()
			return nil, err
		}
		var endpoint ProviderEndpoint
		if err = json.Unmarshal([]byte(body), &endpoint); err != nil {
			rows.Close()
			return nil, err
		}
		endpoints = append(endpoints, endpoint)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, endpoint := range endpoints {
		view, err := r.endpointView(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		list.Endpoints = append(list.Endpoints, view)
	}
	return list, nil
}

func (r *Relay) getConnection(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		ID string `json:"id"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) {
		return nil, model.Err(400, "connection id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	endpoint, found, err := r.readEndpointLocked(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	if found {
		return r.endpointView(ctx, endpoint)
	}
	return nil, model.Err(404, "connection not found")
}

func (r *Relay) saveEndpoint(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		ExpectedRevision *int             `json:"expectedRevision,omitempty"`
		ID               string           `json:"id,omitempty"`
		Endpoint         *endpointRequest `json:"endpoint"`
		APIKey           string           `json:"apiKey,omitempty"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.Endpoint == nil {
		return nil, model.Err(400, "endpoint is required")
	}
	if q.APIKey != "" && (len(q.APIKey) > 64<<10 || !utf8.ValidString(q.APIKey) || strings.ContainsRune(q.APIKey, 0)) {
		return nil, model.Err(400, "apiKey must be valid text without NUL bytes")
	}
	incoming := ProviderEndpoint{}
	request := *q.Endpoint
	if request.ID == "" {
		request.ID = q.ID
	}
	if request.ID != "" {
		if !profileIdentifier(request.ID) {
			return nil, model.Err(400, "invalid endpoint identifier")
		}
		incoming.ID = request.ID
	}
	incoming.Name = request.Name
	incoming.BaseURL = request.BaseURL
	incoming.APIFormat = request.APIFormat
	incoming.AllowInsecureHTTP = request.AllowInsecureHTTP
	if request.SecretRef != "" {
		incoming.SecretRef = request.SecretRef
	}
	if request.Enabled != nil {
		incoming.Enabled = *request.Enabled
	}
	if request.SupportsTools != nil {
		incoming.SupportsTools = *request.SupportsTools
	}
	if request.SupportsImages != nil {
		incoming.SupportsImages = *request.SupportsImages
	}
	if request.ContextWindowTokens != nil {
		incoming.ContextWindowTokens = *request.ContextWindowTokens
	}
	if request.PromptCacheKey != nil {
		incoming.PromptCacheKey = *request.PromptCacheKey
	}
	if request.PromptCaching != nil {
		incoming.PromptCaching = *request.PromptCaching
	}
	// The credential is prepared before the transaction so the vault key is
	// never taken while the single database connection is busy. The definition
	// is validated after the stored values are merged, so a partial edit that
	// omits the base URL keeps the reviewed one.
	secretID := ""
	var nonce, ciphertext []byte
	if q.APIKey != "" {
		var err error
		secretID, nonce, ciphertext, err = r.prepareSecret(secretKindAPIKey, q.APIKey)
		if err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	previous, found, err := r.readEndpointLocked(ctx, incoming.ID)
	if err != nil {
		return nil, err
	}
	if found {
		if q.ExpectedRevision == nil || *q.ExpectedRevision != previous.Revision {
			return nil, model.Err(409, "endpoint revision changed")
		}
		if request.Revision != 0 && request.Revision != previous.Revision {
			return nil, model.Err(409, "endpoint revision changed")
		}
		incoming.ID = previous.ID
		incoming.Revision = previous.Revision + 1
		incoming.CreatedAt = previous.CreatedAt
		if request.Name == "" {
			incoming.Name = previous.Name
		}
		if request.BaseURL == "" {
			incoming.BaseURL = previous.BaseURL
			incoming.AllowInsecureHTTP = previous.AllowInsecureHTTP
		}
		if request.APIFormat == "" {
			incoming.APIFormat = previous.APIFormat
		}
		if request.Enabled == nil {
			incoming.Enabled = previous.Enabled
		}
		if request.SupportsTools == nil {
			incoming.SupportsTools = previous.SupportsTools
		}
		if request.SupportsImages == nil {
			incoming.SupportsImages = previous.SupportsImages
		}
		if request.ContextWindowTokens == nil {
			incoming.ContextWindowTokens = previous.ContextWindowTokens
		}
		if request.PromptCacheKey == nil {
			incoming.PromptCacheKey = previous.PromptCacheKey
		}
		if request.PromptCaching == nil {
			incoming.PromptCaching = previous.PromptCaching
		}
		if request.SecretRef == "" {
			incoming.SecretRef = previous.SecretRef
		}
	} else {
		if q.ExpectedRevision != nil {
			return nil, model.Err(409, "endpoint revision changed")
		}
		if incoming.ID == "" {
			incoming.ID = randomID()
		}
		incoming.Revision = 1
	}
	if secretID == "" && incoming.SecretRef != "" {
		exists, err := r.secretExistsLocked(ctx, incoming.SecretRef)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, model.Err(400, "secret reference does not exist")
		}
	}
	if err := validateEndpointDefinition(&incoming); err != nil {
		return nil, err
	}
	if secretID != "" {
		incoming.SecretRef = secretID
	}
	incoming.UpdatedAt = now().Format(time.RFC3339Nano)
	if incoming.CreatedAt == "" {
		incoming.CreatedAt = incoming.UpdatedAt
	}
	body, err := json.Marshal(incoming)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if secretID != "" {
		if err = r.insertSecret(ctx, tx, secretID, secretKindAPIKey, nonce, ciphertext); err != nil {
			return nil, err
		}
	}
	if found {
		_, err = tx.ExecContext(ctx, "UPDATE provider_endpoints SET revision=?,body=? WHERE id=? AND revision=?", incoming.Revision, string(body), incoming.ID, previous.Revision)
	} else {
		_, err = tx.ExecContext(ctx, "INSERT INTO provider_endpoints(id,revision,body) VALUES(?,?,?)", incoming.ID, incoming.Revision, string(body))
	}
	if err != nil {
		return nil, err
	}
	if err = insertEndpointOriginGrantTx(ctx, tx, incoming, previous, found); err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, "connection.endpoint.saved", incoming.ID, "", map[string]any{"revision": incoming.Revision, "name": incoming.Name, "enabled": incoming.Enabled, "hasSecret": incoming.SecretRef != ""}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.cancelConnectionProbesLocked(incoming.ID)
	return r.endpointView(ctx, incoming)
}

func (r *Relay) disableEndpoint(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		ID               string `json:"id"`
		ExpectedRevision int    `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) || q.ExpectedRevision < 1 {
		return nil, model.Err(400, "endpoint id and expectedRevision are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	endpoint, found, err := r.readEndpointLocked(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, model.Err(404, "endpoint not found")
	}
	if endpoint.Revision != q.ExpectedRevision {
		return nil, model.Err(409, "endpoint revision changed")
	}
	endpoint.Revision++
	endpoint.Enabled = false
	endpoint.UpdatedAt = now().Format(time.RFC3339Nano)
	body, err := json.Marshal(endpoint)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE provider_endpoints SET revision=?,body=? WHERE id=? AND revision=?", endpoint.Revision, string(body), endpoint.ID, q.ExpectedRevision); err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, "connection.endpoint.disabled", endpoint.ID, "", map[string]any{"revision": endpoint.Revision}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.cancelConnectionProbesLocked(endpoint.ID)
	return r.endpointView(ctx, endpoint)
}

func (r *Relay) removeEndpoint(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		ID               string `json:"id"`
		ExpectedRevision int    `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) || q.ExpectedRevision < 1 {
		return nil, model.Err(400, "endpoint id and expectedRevision are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	endpoint, found, err := r.readEndpointLocked(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, model.Err(404, "endpoint not found")
	}
	if endpoint.Revision != q.ExpectedRevision {
		return nil, model.Err(409, "endpoint revision changed")
	}
	inUse, err := r.connectionInUseLocked(ctx, endpoint.ID)
	if err != nil {
		return nil, err
	}
	if inUse {
		return nil, model.Err(409, "connection is referenced by an agent or profile binding")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM provider_endpoints WHERE id=? AND revision=?", endpoint.ID, q.ExpectedRevision); err != nil {
		return nil, err
	}
	if endpoint.SecretRef != "" {
		if _, err = tx.ExecContext(ctx, "DELETE FROM automation_secrets WHERE id=?", endpoint.SecretRef); err != nil {
			return nil, err
		}
	}
	if err = insertAutomationEvent(tx, "connection.endpoint.removed", endpoint.ID, "", map[string]any{"revision": endpoint.Revision}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.cancelConnectionProbesLocked(endpoint.ID)
	return map[string]any{"id": endpoint.ID, "removed": true}, nil
}

// listEndpointModels lists the models an endpoint advertises. The listing is
// a provider statement, not a capability guarantee, and no secret value is
// returned: only model identifiers and names.
func (r *Relay) listEndpointModels(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		ID string `json:"id"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) {
		return nil, model.Err(400, "endpoint id is required")
	}
	ctx, release, err := r.beginConnectionProbe(ctx, q.ID, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer release()
	endpoint, err := r.ProviderEndpoint(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	if !endpoint.Enabled {
		return nil, model.Err(409, "endpoint is disabled")
	}
	modelsURL, err := EndpointModelsURL(endpoint.BaseURL)
	if err != nil {
		return nil, err
	}
	config, err := r.endpointConfig(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, model.Err(400, "model listing URL is invalid")
	}
	request.Header.Set("Accept", "application/json")
	// A local endpoint may be listed without a credential. The secret is used
	// for the request only and is never returned or logged.
	if config.Format == openai.FormatAnthropic {
		request.Header.Set("anthropic-version", openai.AnthropicVersion)
		if config.APIKey != "" {
			request.Header.Set("x-api-key", config.APIKey)
		}
		// The Messages API lists 20 models by default.
		request.URL.RawQuery = "limit=1000"
	} else if config.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+config.APIKey)
	}
	response, err := config.HTTPClient.Do(request)
	if err != nil {
		if errors.Is(err, netpolicy.ErrDenied) || errors.Is(err, netpolicy.ErrRevoked) {
			return nil, model.Err(403, "review an exact network origin grant for this connection before discovery")
		}
		return nil, model.Err(503, "the endpoint did not answer the model listing")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	response.Body.Close()
	if readErr != nil {
		return nil, model.Err(503, "the model listing could not be read")
	}
	if len(body) > 2<<20 {
		return nil, model.Err(413, "the model listing exceeded the response bound")
	}
	if response.StatusCode != http.StatusOK {
		return nil, model.Err(503, fmt.Sprintf("the endpoint returned HTTP %d for the model listing", response.StatusCode))
	}
	options, err := parseModelOptions(body, endpoint.APIFormat)
	if err != nil {
		return nil, model.Err(503, "the endpoint did not return a model listing")
	}
	return options, nil
}

// protocolKey is the per-model protocol name that gateways such as OpenCode
// publish under "api". It is empty for a format that has no such key.
func protocolKey(format string) string {
	switch format {
	case "", openai.FormatOpenAI:
		return "chat-completions"
	case openai.FormatAnthropic:
		return "messages"
	}
	return ""
}

// parseModelOptions accepts the common listing shapes. Unknown entries are
// skipped, never guessed.
func parseModelOptions(body []byte, format string) ([]model.ModelOption, error) {
	var payload struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	entries := payload.Data
	if len(entries) == 0 {
		entries = payload.Models
	}
	options := []model.ModelOption{}
	seen := map[string]bool{}
	appendOption := func(id, name string) bool {
		if id == "" || len(id) > 200 || !safeRegistryText(id) || seen[id] {
			return false
		}
		if name == "" {
			name = id
		}
		if len(name) > 200 || !safeRegistryText(name) {
			name = id
		}
		seen[id] = true
		options = append(options, model.ModelOption{ID: id, Name: name})
		return len(options) >= 500
	}
	for _, raw := range entries {
		var entry struct {
			ID          string                     `json:"id"`
			Model       string                     `json:"model"`
			Name        string                     `json:"name"`
			DisplayName string                     `json:"display_name"`
			Type        string                     `json:"type"`
			API         map[string]json.RawMessage `json:"api"`
		}
		if err := json.Unmarshal(raw, &entry); err == nil {
			// A gateway can list non-chat models and models that speak another
			// protocol. Offer only what this connection's format can run.
			if entry.Type != "" && entry.Type != "chat" {
				continue
			}
			if want := protocolKey(format); want != "" && len(entry.API) > 0 {
				if _, ok := entry.API[want]; !ok {
					continue
				}
			}
			id := entry.ID
			if id == "" {
				id = entry.Model
			}
			if id == "" {
				// TypeSafe lists {"models":[{"name":...}]}.
				id = entry.Name
			}
			if entry.Name == "" {
				entry.Name = entry.DisplayName
			}
			if appendOption(id, entry.Name) {
				break
			}
			continue
		}
		var name string
		if err := json.Unmarshal(raw, &name); err == nil {
			if appendOption(name, name) {
				break
			}
		}
	}
	sort.SliceStable(options, func(i, j int) bool { return options[i].ID < options[j].ID })
	return options, nil
}

// scanLocalConnections probes exactly the two reviewed loopback model
// endpoints. It sends no credentials, follows no redirect and reads at most
// two mebibytes per response. Discovery is not authentication.
func (r *Relay) scanLocalConnections(ctx context.Context) ([]LocalModelScan, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("local model probe refuses redirects")
		},
	}
	results := []LocalModelScan{}
	for _, raw := range localModelProbeURLs {
		result := LocalModelScan{URL: raw, Status: "unreachable"}
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, raw, nil)
		if err != nil {
			cancel()
			result.Status = "error"
			result.Detail = "invalid probe URL"
			results = append(results, result)
			continue
		}
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			cancel()
			result.Detail = "no response"
			results = append(results, result)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
		response.Body.Close()
		cancel()
		if readErr != nil {
			result.Status = "error"
			result.Detail = "response could not be read"
			results = append(results, result)
			continue
		}
		if len(body) > 2<<20 {
			result.Status = "error"
			result.Detail = "response exceeded the two mebibyte bound"
			results = append(results, result)
			continue
		}
		if response.StatusCode != http.StatusOK {
			result.Status = "error"
			result.Detail = fmt.Sprintf("status %d", response.StatusCode)
			results = append(results, result)
			continue
		}
		models, err := parseLocalModelList(body)
		if err != nil {
			result.Status = "error"
			result.Detail = "response was not a model list"
			results = append(results, result)
			continue
		}
		result.Status = "available"
		result.Models = models
		results = append(results, result)
	}
	return results, nil
}

// parseLocalModelList extracts the model identifiers of an OpenAI-compatible
// model listing. Unknown shapes are reported, never guessed.
func parseLocalModelList(body []byte) ([]string, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	models := []string{}
	for _, entry := range payload.Data {
		if entry.ID == "" || len(entry.ID) > 200 || !safeRegistryText(entry.ID) {
			continue
		}
		models = append(models, entry.ID)
		if len(models) >= 64 {
			break
		}
	}
	sort.Strings(models)
	return models, nil
}

// ProviderEndpoint reads one stored endpoint for process-internal use. The
// returned definition never contains a secret value.
func (r *Relay) ProviderEndpoint(ctx context.Context, id string) (ProviderEndpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ProviderEndpoint{}, model.Err(503, "relay is stopping")
	}
	endpoint, found, err := r.readEndpointLocked(ctx, id)
	if err != nil {
		return ProviderEndpoint{}, err
	}
	if !found {
		return ProviderEndpoint{}, model.Err(404, "endpoint not found")
	}
	return endpoint, nil
}

// EndpointCredential returns the endpoint API key for process-internal use.
// The caller must not persist, log or return the value.
func (r *Relay) EndpointCredential(ctx context.Context, id string) (string, error) {
	endpoint, err := r.ProviderEndpoint(ctx, id)
	if err != nil {
		return "", err
	}
	if endpoint.SecretRef == "" {
		return "", model.Err(404, "endpoint has no stored credential")
	}
	return r.readSecret(ctx, endpoint.SecretRef)
}
