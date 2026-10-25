package externalmcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// MaxTokenResponseBytes bounds one token endpoint response.
const MaxTokenResponseBytes = 256 << 10

// OAuthDiscovery is the resolved authorization configuration of one MCP server.
type OAuthDiscovery struct {
	Resource                   string   `json:"resource"`
	Issuer                     string   `json:"issuer"`
	AuthorizationEndpoint      string   `json:"authorizationEndpoint"`
	TokenEndpoint              string   `json:"tokenEndpoint"`
	RegistrationEndpoint       string   `json:"registrationEndpoint,omitempty"`
	RevocationEndpoint         string   `json:"revocationEndpoint,omitempty"`
	ScopesSupported            []string `json:"scopesSupported,omitempty"`
	CodeChallengeMethods        []string `json:"codeChallengeMethodsSupported,omitempty"`
	ClientID                   string   `json:"clientId,omitempty"`
	ClientSecret               string   `json:"clientSecret,omitempty"`
	RedirectURI                string   `json:"redirectUri"`
}

// Token is an OAuth token set. It is stored encrypted and never returned in a
// DTO, an event or a log.
type Token struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	TokenType    string    `json:"tokenType,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// Expired reports whether the access token is past its expiry.
func (t Token) Expired() bool {
	return !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(time.Now().UTC())
}

// DiscoverOAuth resolves the protected resource metadata, the authorization
// server metadata and, when the server supports it, a dynamic client
// registration for one MCP endpoint.
func DiscoverOAuth(ctx context.Context, client *http.Client, serverURL, redirectURI string, scopes []string) (*OAuthDiscovery, error) {
	if client == nil {
		return nil, errors.New("an http client is required for OAuth discovery")
	}
	origin, err := OriginOf(serverURL)
	if err != nil {
		return nil, err
	}
	discovery := &OAuthDiscovery{Resource: serverURL, Issuer: origin, RedirectURI: redirectURI}
	resourceMetadataURL := origin + "/.well-known/oauth-protected-resource"
	metadata, err := oauthex.GetProtectedResourceMetadata(ctx, resourceMetadataURL, serverURL, client)
	if err != nil {
		metadata = nil
	}
	if metadata != nil && len(metadata.AuthorizationServers) > 0 {
		discovery.Issuer = metadata.AuthorizationServers[0]
		if len(metadata.ScopesSupported) > 0 {
			discovery.ScopesSupported = metadata.ScopesSupported
		}
	}
	authMetadataURL := strings.TrimSuffix(discovery.Issuer, "/") + "/.well-known/oauth-authorization-server"
	server, err := oauthex.GetAuthServerMeta(ctx, authMetadataURL, discovery.Issuer, client)
	if err != nil {
		return nil, fmt.Errorf("authorization server metadata is unavailable: %w", err)
	}
	if server == nil {
		return nil, errors.New("authorization server metadata is unavailable")
	}
	discovery.AuthorizationEndpoint = server.AuthorizationEndpoint
	discovery.TokenEndpoint = server.TokenEndpoint
	discovery.RegistrationEndpoint = server.RegistrationEndpoint
	discovery.RevocationEndpoint = server.RevocationEndpoint
	issuerOrigin, err := OriginOf(discovery.Issuer)
	if err != nil { return nil, errors.New("authorization issuer is invalid") }
	for _, endpoint := range []string{discovery.AuthorizationEndpoint, discovery.TokenEndpoint, discovery.RegistrationEndpoint, discovery.RevocationEndpoint} {
		if endpoint == "" { continue }
		endpointOrigin, err := OriginOf(endpoint)
		if err != nil || endpointOrigin != issuerOrigin { return nil, errors.New("OAuth endpoint is outside the authorization issuer origin") }
	}
	if len(server.ScopesSupported) > 0 {
		discovery.ScopesSupported = server.ScopesSupported
	}
	discovery.CodeChallengeMethods = server.CodeChallengeMethodsSupported
	if len(scopes) > 0 {
		discovery.ScopesSupported = scopes
	}
	if server.RegistrationEndpoint != "" {
		registration, err := oauthex.RegisterClient(ctx, server.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
			RedirectURIs:      []string{redirectURI},
			GrantTypes:        []string{"authorization_code", "refresh_token"},
			ResponseTypes:     []string{"code"},
			TokenEndpointAuthMethod: "none",
			ClientName:        "Meshra",
			Scope:             strings.Join(discovery.ScopesSupported, " "),
		}, client)
		if err != nil {
			return nil, fmt.Errorf("dynamic client registration failed: %w", err)
		}
		discovery.ClientID = registration.ClientID
		discovery.ClientSecret = registration.ClientSecret
	}
	return discovery, nil
}

// NewState returns an unguessable OAuth state value.
func NewState() (string, error) {
	return randomValue(32)
}

// NewVerifier returns an unguessable PKCE code verifier.
func NewVerifier() (string, error) {
	return randomValue(48)
}

// randomValue returns a base64url encoded random string.
func randomValue(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// Challenge returns the S256 PKCE challenge of a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizationURL builds the operator-facing authorization URL of one flow.
func AuthorizationURL(discovery *OAuthDiscovery, state, verifier string, scopes []string) (string, error) {
	if discovery == nil || discovery.AuthorizationEndpoint == "" {
		return "", errors.New("authorization endpoint is unknown")
	}
	if state == "" || verifier == "" {
		return "", errors.New("state and verifier are required")
	}
	parsed, err := url.Parse(discovery.AuthorizationEndpoint)
	if err != nil {
		return "", errors.New("authorization endpoint is not a valid URL")
	}
	query := parsed.Query()
	query.Set("response_type", "code")
	query.Set("client_id", discovery.ClientID)
	query.Set("redirect_uri", discovery.RedirectURI)
	query.Set("state", state)
	query.Set("code_challenge", Challenge(verifier))
	query.Set("code_challenge_method", "S256")
	query.Set("resource", discovery.Resource)
	if len(scopes) > 0 {
		query.Set("scope", strings.Join(scopes, " "))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// tokenResponse is the RFC 6749 token endpoint response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// ErrInvalidGrant reports that the stored grant is no longer usable.
var ErrInvalidGrant = errors.New("the stored authorization grant is no longer valid")

// ExchangeCode exchanges an authorization code for a token set.
func ExchangeCode(ctx context.Context, client *http.Client, discovery *OAuthDiscovery, code, verifier string) (*Token, error) {
	if discovery == nil || discovery.TokenEndpoint == "" {
		return nil, errors.New("token endpoint is unknown")
	}
	if code == "" {
		return nil, errors.New("authorization code is missing")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", discovery.RedirectURI)
	form.Set("client_id", discovery.ClientID)
	form.Set("code_verifier", verifier)
	form.Set("resource", discovery.Resource)
	return tokenRequest(ctx, client, discovery.TokenEndpoint, form, discovery)
}

// RefreshToken exchanges a refresh token for a new token set.
func RefreshToken(ctx context.Context, client *http.Client, discovery *OAuthDiscovery, refreshToken string) (*Token, error) {
	if discovery == nil || discovery.TokenEndpoint == "" {
		return nil, errors.New("token endpoint is unknown")
	}
	if refreshToken == "" {
		return nil, ErrInvalidGrant
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", discovery.ClientID)
	form.Set("resource", discovery.Resource)
	return tokenRequest(ctx, client, discovery.TokenEndpoint, form, discovery)
}

// tokenRequest performs one bounded token endpoint request.
func tokenRequest(ctx context.Context, client *http.Client, endpoint string, form url.Values, discovery *OAuthDiscovery) (*Token, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("token request could not be built")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if discovery != nil && discovery.ClientSecret != "" {
		request.SetBasicAuth(discovery.ClientID, discovery.ClientSecret)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("token endpoint request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxTokenResponseBytes))
	if err != nil {
		return nil, errors.New("token endpoint response could not be read")
	}
	var decoded tokenResponse
	if err = json.Unmarshal(body, &decoded); err != nil {
		return nil, errors.New("token endpoint response is not valid JSON")
	}
	if decoded.Error != "" {
		if decoded.Error == "invalid_grant" {
			return nil, ErrInvalidGrant
		}
		return nil, fmt.Errorf("token endpoint refused the request: %s", decoded.Error)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned status %d", response.StatusCode)
	}
	if decoded.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access token")
	}
	token := &Token{AccessToken: decoded.AccessToken, RefreshToken: decoded.RefreshToken, TokenType: decoded.TokenType}
	if decoded.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().UTC().Add(time.Duration(decoded.ExpiresIn) * time.Second)
	}
	if decoded.Scope != "" {
		token.Scopes = strings.Fields(decoded.Scope)
	}
	return token, nil
}

// RevokeToken attempts a bounded upstream revocation of one token.
func RevokeToken(ctx context.Context, client *http.Client, discovery *OAuthDiscovery, token string) error {
	if discovery == nil || discovery.RevocationEndpoint == "" || token == "" {
		return nil
	}
	form := url.Values{}
	form.Set("token", token)
	form.Set("client_id", discovery.ClientID)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, discovery.RevocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("revocation request could not be built")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if discovery.ClientSecret != "" {
		request.SetBasicAuth(discovery.ClientID, discovery.ClientSecret)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, MaxTokenResponseBytes))
	if response.StatusCode >= 300 {
		return fmt.Errorf("revocation endpoint returned status %d", response.StatusCode)
	}
	return nil
}
