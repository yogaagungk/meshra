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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
	"meshra/internal/netpolicy"
)

// ProviderEndpoint is an operator reviewed OpenAI-compatible endpoint. The
// credential is referenced, never embedded: secretRef names a vault record.
type ProviderEndpoint struct {
	ID                  string `json:"id"`
	Revision            int    `json:"revision"`
	Name                string `json:"name"`
	BaseURL             string `json:"baseUrl"`
	Enabled             bool   `json:"enabled"`
	SecretRef           string `json:"secretRef,omitempty"`
	AllowInsecureHTTP   bool   `json:"allowInsecureHttp,omitempty"`
	SupportsTools       bool   `json:"supportsTools"`
	SupportsImages      bool   `json:"supportsImages"`
	ContextWindowTokens int    `json:"contextWindowTokens,omitempty"`
	CreatedAt           string `json:"createdAt"`
	UpdatedAt           string `json:"updatedAt"`
}

// ProviderEndpointView is the operator summary. It never contains a secret
// value, only whether the referenced record is missing, saved or unreadable.
type ProviderEndpointView struct {
	ProviderEndpoint
	SecretStatus string `json:"secretStatus"`
}

// ACPAgentConfig describes one installed ACP command line agent.
type ACPAgentConfig struct {
	ID                string   `json:"id"`
	Revision          int      `json:"revision"`
	Name              string   `json:"name"`
	Preset            string   `json:"preset"`
	Executable        string   `json:"executable"`
	Args              []string `json:"args"`
	RuntimePaths      []string `json:"runtimePaths"`
	SecretEnvRefs     []string `json:"secretEnvRefs"`
	ReadOnlyAuthFiles []string `json:"readOnlyAuthFiles"`
	AllowedOrigins    []string `json:"allowedOrigins"`
	Enabled           bool     `json:"enabled"`
	CreatedAt         string   `json:"createdAt"`
	UpdatedAt         string   `json:"updatedAt"`
}

// ACPAgentView is the operator summary of one ACP configuration.
type ACPAgentView struct {
	ACPAgentConfig
	SecretStatus string `json:"secretStatus"`
}

// ConnectionList is the read summary of every configured connection.
type ConnectionList struct {
	Endpoints []ProviderEndpointView `json:"endpoints"`
	ACP       []ACPAgentView         `json:"acp"`
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
	ID                  string `json:"id,omitempty"`
	Revision            int    `json:"revision,omitempty"`
	Name                string `json:"name"`
	BaseURL             string `json:"baseUrl"`
	Enabled             *bool  `json:"enabled,omitempty"`
	SecretRef           string `json:"secretRef,omitempty"`
	AllowInsecureHTTP   bool   `json:"allowInsecureHttp,omitempty"`
	SupportsTools       *bool  `json:"supportsTools,omitempty"`
	SupportsImages      *bool  `json:"supportsImages,omitempty"`
	ContextWindowTokens *int   `json:"contextWindowTokens,omitempty"`
}

// acpRequest is the submitted ACP configuration definition.
type acpRequest struct {
	ID                string   `json:"id,omitempty"`
	Revision          int      `json:"revision,omitempty"`
	Name              string   `json:"name"`
	Preset            string   `json:"preset"`
	Executable        string   `json:"executable"`
	Args              []string `json:"args,omitempty"`
	RuntimePaths      []string `json:"runtimePaths,omitempty"`
	SecretEnvRefs     []string `json:"secretEnvRefs,omitempty"`
	ReadOnlyAuthFiles []string `json:"readOnlyAuthFiles,omitempty"`
	AllowedOrigins    []string `json:"allowedOrigins,omitempty"`
	Enabled           *bool    `json:"enabled,omitempty"`
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
	case "connections.checkAcp", "connections.acpStatus":
		return r.acpReadinessOperation(ctx, principal, operation, raw)
	case "connections.authenticateAcp":
		return r.authenticateACP(ctx, principal, raw)
	case "connections.models":
		return r.listEndpointModels(ctx, raw)
	case "connections.saveEndpoint":
		return r.saveEndpoint(ctx, raw)
	case "connections.disable":
		return r.disableEndpoint(ctx, raw)
	case "connections.remove":
		return r.removeEndpoint(ctx, raw)
	case "connections.saveAcp":
		return r.saveACPAgent(ctx, raw)
	case "connections.disableAcp":
		return r.disableACPAgent(ctx, raw)
	case "connections.removeAcp":
		return r.removeACPAgent(ctx, raw)
	case "connections.scanLocal":
		return r.scanLocalOperation(ctx)
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
	if endpoint.SecretRef != "" && !profileIdentifier(endpoint.SecretRef) {
		return model.Err(400, "invalid secret reference")
	}
	if endpoint.ContextWindowTokens < 0 || endpoint.ContextWindowTokens > 10_000_000 {
		return model.Err(400, "contextWindowTokens is out of range")
	}
	return nil
}

func validateACPExecutable(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 || !safeRegistryText(raw) {
		return "", model.Err(400, "ACP executable must be an absolute path")
	}
	if !filepath.IsAbs(raw) {
		return "", model.Err(400, "ACP executable must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", model.Err(400, "ACP executable does not exist")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", model.Err(400, "ACP executable must be an existing regular file")
	}
	return resolved, nil
}

// validateACPArgs rejects shell text. Arguments are passed to the process as a
// vector, so a combined command line is always a configuration error.
func validateACPArgs(args []string, preset string) ([]string, error) {
	if args == nil {
		switch preset {
		case "gemini", "cline":
			args = []string{"--acp"}
		case "cursor":
			args = []string{"acp"}
		}
	}
	if len(args) > 32 {
		return nil, model.Err(400, "ACP argument list is too long")
	}
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "" || len(arg) > 1024 || !safeRegistryText(arg) {
			return nil, model.Err(400, "ACP arguments must be nonempty safe strings")
		}
		if strings.ContainsAny(arg, ";|&$`<>(){}\\'\"\\") {
			return nil, model.Err(400, "ACP arguments must not contain shell syntax")
		}
		if redact(arg) != arg {
			return nil, model.Err(400, "ACP arguments must not contain credentials")
		}
		if maskSecretValues(arg) != arg {
			return nil, model.Err(400, "ACP arguments must not contain secret values")
		}
		out = append(out, arg)
	}
	return out, nil
}

func validateACPRuntimePaths(paths []string) ([]string, error) {
	if len(paths) > 32 {
		return nil, model.Err(400, "too many ACP runtime paths")
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" || len(path) > 4096 || !safeRegistryText(path) {
			return nil, model.Err(400, "invalid ACP runtime path")
		}
		if !filepath.IsAbs(path) {
			return nil, model.Err(400, "ACP runtime paths must be absolute")
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return nil, model.Err(400, "ACP runtime path must be an existing directory")
		}
		out = append(out, filepath.Clean(path))
	}
	return out, nil
}

func validateACPAuthFiles(files []string) ([]string, error) {
	if len(files) > 32 {
		return nil, model.Err(400, "too many ACP authentication files")
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		if file == "" || len(file) > 4096 || !safeRegistryText(file) {
			return nil, model.Err(400, "invalid ACP authentication file")
		}
		if !filepath.IsAbs(file) {
			return nil, model.Err(400, "ACP authentication files must be absolute")
		}
		resolved, err := filepath.EvalSymlinks(file)
		if err != nil {
			return nil, model.Err(400, "ACP authentication file does not exist")
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return nil, model.Err(400, "ACP authentication file must be an existing regular file")
		}
		out = append(out, resolved)
	}
	return out, nil
}

// validateACPSecretEnvRefs accepts ENV_NAME=secret_id entries. The value never
// appears in the configuration; only the opaque reference does.
func validateACPSecretEnvRefs(refs []string) ([]string, error) {
	if len(refs) > 32 {
		return nil, model.Err(400, "too many ACP secret references")
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		name, id, ok := strings.Cut(ref, "=")
		if !ok || name == "" || id == "" || len(ref) > 512 || !safeRegistryText(ref) {
			return nil, model.Err(400, "ACP secret references must use ENV_NAME=secret_id")
		}
		for _, ch := range name {
			if !(ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9') {
				return nil, model.Err(400, "invalid ACP environment variable name")
			}
		}
		if !profileIdentifier(id) {
			return nil, model.Err(400, "invalid ACP secret reference")
		}
		out = append(out, name+"="+id)
	}
	return out, nil
}

func validateACPOrigins(origins []string) ([]string, error) {
	if len(origins) > 32 {
		return nil, model.Err(400, "too many ACP origins")
	}
	out := make([]string, 0, len(origins))
	for _, raw := range origins {
		if raw == "" || len(raw) > 2048 || !safeRegistryText(raw) {
			return nil, model.Err(400, "invalid ACP origin")
		}
		origin, err := netpolicy.ParseOrigin(raw)
		if err != nil {
			return nil, model.Err(400, "ACP origins must be exact scheme, host and port without path, query or credentials")
		}
		out = append(out, origin.String())
	}
	return out, nil
}

func validateACPDefinition(config *ACPAgentConfig) error {
	name := strings.TrimSpace(config.Name)
	if name == "" || len(name) > 128 || !safeRegistryText(name) || redact(name) != name {
		return model.Err(400, "ACP configuration name must be 1..128 bytes without credentials or control characters")
	}
	config.Name = name
	preset := strings.ToLower(strings.TrimSpace(config.Preset))
	switch preset {
	case "gemini", "cursor", "cline", "custom":
	default:
		return model.Err(400, "ACP preset must be gemini, cursor, cline or custom")
	}
	config.Preset = preset
	executable, err := validateACPExecutable(config.Executable)
	if err != nil {
		return err
	}
	config.Executable = executable
	args, err := validateACPArgs(config.Args, preset)
	if err != nil {
		return err
	}
	config.Args = args
	paths, err := validateACPRuntimePaths(config.RuntimePaths)
	if err != nil {
		return err
	}
	config.RuntimePaths = paths
	files, err := validateACPAuthFiles(config.ReadOnlyAuthFiles)
	if err != nil {
		return err
	}
	config.ReadOnlyAuthFiles = files
	refs, err := validateACPSecretEnvRefs(config.SecretEnvRefs)
	if err != nil {
		return err
	}
	config.SecretEnvRefs = refs
	origins, err := validateACPOrigins(config.AllowedOrigins)
	if err != nil {
		return err
	}
	config.AllowedOrigins = origins
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

func (r *Relay) readACPLocked(ctx context.Context, id string) (ACPAgentConfig, bool, error) {
	var body string
	err := r.db.QueryRowContext(ctx, "SELECT body FROM acp_agents WHERE id=?", id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return ACPAgentConfig{}, false, nil
	}
	if err != nil {
		return ACPAgentConfig{}, false, err
	}
	var config ACPAgentConfig
	if err = json.Unmarshal([]byte(body), &config); err != nil {
		return ACPAgentConfig{}, false, err
	}
	return config, true, nil
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

func (r *Relay) acpView(ctx context.Context, config ACPAgentConfig) (ACPAgentView, error) {
	status := "missing"
	for _, ref := range config.SecretEnvRefs {
		_, id, ok := strings.Cut(ref, "=")
		if !ok {
			continue
		}
		state, err := r.secretStatus(ctx, id)
		if err != nil {
			return ACPAgentView{}, err
		}
		if state == "unreadable" {
			status = "unreadable"
			break
		}
		if state == "saved" && status == "missing" {
			status = "saved"
		}
	}
	return ACPAgentView{ACPAgentConfig: config, SecretStatus: status}, nil
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
	list := ConnectionList{Endpoints: []ProviderEndpointView{}, ACP: []ACPAgentView{}}
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
	configs := []ACPAgentConfig{}
	acpRows, err := r.db.QueryContext(ctx, "SELECT body FROM acp_agents ORDER BY id")
	if err != nil {
		return nil, err
	}
	for acpRows.Next() {
		var body string
		if err = acpRows.Scan(&body); err != nil {
			acpRows.Close()
			return nil, err
		}
		var config ACPAgentConfig
		if err = json.Unmarshal([]byte(body), &config); err != nil {
			acpRows.Close()
			return nil, err
		}
		configs = append(configs, config)
	}
	if err = acpRows.Err(); err != nil {
		acpRows.Close()
		return nil, err
	}
	acpRows.Close()
	for _, endpoint := range endpoints {
		view, err := r.endpointView(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		list.Endpoints = append(list.Endpoints, view)
	}
	for _, config := range configs {
		view, err := r.acpView(ctx, config)
		if err != nil {
			return nil, err
		}
		list.ACP = append(list.ACP, view)
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
	config, found, err := r.readACPLocked(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	if found {
		return r.acpView(ctx, config)
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
	// The shared-secret lookup runs before the transaction: the relay owns a
	// single database connection and a nested read would wait for itself.
	shared := false
	if endpoint.SecretRef != "" {
		shared, err = r.acpSecretSharedLocked(ctx, endpoint.SecretRef)
		if err != nil {
			return nil, err
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM provider_endpoints WHERE id=? AND revision=?", endpoint.ID, q.ExpectedRevision); err != nil {
		return nil, err
	}
	if endpoint.SecretRef != "" && !shared {
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

// acpSecretSharedLocked reports whether an ACP configuration references the
// same secret record.
func (r *Relay) acpSecretSharedLocked(ctx context.Context, id string) (bool, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT body FROM acp_agents")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			return false, err
		}
		var config ACPAgentConfig
		if err = json.Unmarshal([]byte(body), &config); err != nil {
			return false, err
		}
		for _, ref := range config.SecretEnvRefs {
			if _, secretID, ok := strings.Cut(ref, "="); ok && secretID == id {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func (r *Relay) saveACPAgent(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		ExpectedRevision *int        `json:"expectedRevision,omitempty"`
		ID               string      `json:"id,omitempty"`
		ACP              *acpRequest `json:"acp"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ACP == nil {
		return nil, model.Err(400, "acp configuration is required")
	}
	request := *q.ACP
	if request.ID == "" {
		request.ID = q.ID
	}
	incoming := ACPAgentConfig{}
	if request.ID != "" {
		if !profileIdentifier(request.ID) {
			return nil, model.Err(400, "invalid ACP configuration identifier")
		}
		incoming.ID = request.ID
	}
	incoming.Name = request.Name
	incoming.Preset = request.Preset
	incoming.Executable = request.Executable
	incoming.Args = request.Args
	incoming.RuntimePaths = request.RuntimePaths
	incoming.SecretEnvRefs = request.SecretEnvRefs
	incoming.ReadOnlyAuthFiles = request.ReadOnlyAuthFiles
	incoming.AllowedOrigins = request.AllowedOrigins
	if request.Enabled != nil {
		incoming.Enabled = *request.Enabled
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	previous, found, err := r.readACPLocked(ctx, incoming.ID)
	if err != nil {
		return nil, err
	}
	if found {
		if q.ExpectedRevision == nil || *q.ExpectedRevision != previous.Revision {
			return nil, model.Err(409, "ACP configuration revision changed")
		}
		if request.Revision != 0 && request.Revision != previous.Revision {
			return nil, model.Err(409, "ACP configuration revision changed")
		}
		incoming.ID = previous.ID
		incoming.Revision = previous.Revision + 1
		incoming.CreatedAt = previous.CreatedAt
		if request.Name == "" {
			incoming.Name = previous.Name
		}
		if request.Preset == "" {
			incoming.Preset = previous.Preset
		}
		if request.Executable == "" {
			incoming.Executable = previous.Executable
		}
		if request.Args == nil {
			incoming.Args = previous.Args
		}
		if request.RuntimePaths == nil {
			incoming.RuntimePaths = previous.RuntimePaths
		}
		if request.SecretEnvRefs == nil {
			incoming.SecretEnvRefs = previous.SecretEnvRefs
		}
		if request.ReadOnlyAuthFiles == nil {
			incoming.ReadOnlyAuthFiles = previous.ReadOnlyAuthFiles
		}
		if request.AllowedOrigins == nil {
			incoming.AllowedOrigins = previous.AllowedOrigins
		}
		if request.Enabled == nil {
			incoming.Enabled = previous.Enabled
		}
	} else {
		if q.ExpectedRevision != nil {
			return nil, model.Err(409, "ACP configuration revision changed")
		}
		if incoming.ID == "" {
			incoming.ID = randomID()
		}
		incoming.Revision = 1
	}
	if err := validateACPDefinition(&incoming); err != nil {
		return nil, err
	}
	for _, ref := range incoming.SecretEnvRefs {
		_, id, _ := strings.Cut(ref, "=")
		exists, err := r.secretExistsLocked(ctx, id)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, model.Err(400, "secret reference does not exist")
		}
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
	if found {
		_, err = tx.ExecContext(ctx, "UPDATE acp_agents SET revision=?,body=? WHERE id=? AND revision=?", incoming.Revision, string(body), incoming.ID, previous.Revision)
	} else {
		_, err = tx.ExecContext(ctx, "INSERT INTO acp_agents(id,revision,body) VALUES(?,?,?)", incoming.ID, incoming.Revision, string(body))
	}
	if err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, "connection.acp.saved", incoming.ID, "", map[string]any{"revision": incoming.Revision, "name": incoming.Name, "preset": incoming.Preset, "enabled": incoming.Enabled}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.cancelConnectionProbesLocked(incoming.ID)
	return r.acpView(ctx, incoming)
}

func (r *Relay) disableACPAgent(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		ID               string `json:"id"`
		ExpectedRevision int    `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) || q.ExpectedRevision < 1 {
		return nil, model.Err(400, "ACP configuration id and expectedRevision are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	config, found, err := r.readACPLocked(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, model.Err(404, "ACP configuration not found")
	}
	if config.Revision != q.ExpectedRevision {
		return nil, model.Err(409, "ACP configuration revision changed")
	}
	config.Revision++
	config.Enabled = false
	config.UpdatedAt = now().Format(time.RFC3339Nano)
	body, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE acp_agents SET revision=?,body=? WHERE id=? AND revision=?", config.Revision, string(body), config.ID, q.ExpectedRevision); err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, "connection.acp.disabled", config.ID, "", map[string]any{"revision": config.Revision}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.cancelConnectionProbesLocked(config.ID)
	return r.acpView(ctx, config)
}

func (r *Relay) removeACPAgent(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		ID               string `json:"id"`
		ExpectedRevision int    `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) || q.ExpectedRevision < 1 {
		return nil, model.Err(400, "ACP configuration id and expectedRevision are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	config, found, err := r.readACPLocked(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, model.Err(404, "ACP configuration not found")
	}
	if config.Revision != q.ExpectedRevision {
		return nil, model.Err(409, "ACP configuration revision changed")
	}
	inUse, err := r.connectionInUseLocked(ctx, config.ID)
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
	if _, err = tx.ExecContext(ctx, "DELETE FROM acp_agents WHERE id=? AND revision=?", config.ID, q.ExpectedRevision); err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, "connection.acp.removed", config.ID, "", map[string]any{"revision": config.Revision}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.cancelConnectionProbesLocked(config.ID)
	return map[string]any{"id": config.ID, "removed": true}, nil
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
	if config.APIKey != "" {
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
		return nil, model.Err(503, "the endpoint did not return a model listing")
	}
	options, err := parseModelOptions(body)
	if err != nil {
		return nil, model.Err(503, "the endpoint did not return a model listing")
	}
	return options, nil
}

// parseModelOptions accepts the common listing shapes. Unknown entries are
// skipped, never guessed.
func parseModelOptions(body []byte) ([]model.ModelOption, error) {
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
			ID    string `json:"id"`
			Model string `json:"model"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(raw, &entry); err == nil {
			id := entry.ID
			if id == "" {
				id = entry.Model
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

// scanLocalOperation probes fixed model endpoints and inspects installed ACP paths.
func (r *Relay) scanLocalOperation(ctx context.Context) (any, error) {
	results, err := r.scanLocalConnections(ctx)
	if err != nil {
		return nil, err
	}
	presets, err := scanInstalledACPPresets(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"results": results, "acpPresets": presets}, nil
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

// ACPAgent reads one stored ACP configuration for process-internal use.
func (r *Relay) ACPAgent(ctx context.Context, id string) (ACPAgentConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ACPAgentConfig{}, model.Err(503, "relay is stopping")
	}
	config, found, err := r.readACPLocked(ctx, id)
	if err != nil {
		return ACPAgentConfig{}, err
	}
	if !found {
		return ACPAgentConfig{}, model.Err(404, "ACP configuration not found")
	}
	return config, nil
}

// ACPSecretEnvironment resolves the referenced secret records into an
// environment for a confined ACP launch. The values are process-internal.
func (r *Relay) ACPSecretEnvironment(ctx context.Context, config ACPAgentConfig) (map[string]string, error) {
	environment := map[string]string{}
	for _, ref := range config.SecretEnvRefs {
		name, id, ok := strings.Cut(ref, "=")
		if !ok {
			continue
		}
		value, err := r.readSecret(ctx, id)
		if err != nil {
			return nil, err
		}
		environment[name] = value
	}
	return environment, nil
}
