package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
	"meshra/internal/relay"
)

// Observe security context around the real relay, without supplying synthetic
// operation results or replacing its authorization/account implementation.
type sessionAuditService struct {
	*relay.Relay
	validate func(string) bool
	revoked  map[string]int
	callers  []model.Principal
	onRevoke func(string)
}

func (s *sessionAuditService) SetConsoleSessionValidator(validate func(string) bool) {
	s.validate = validate
	s.Relay.SetConsoleSessionValidator(validate)
}

func (s *sessionAuditService) RevokeConsoleSession(id string) {
	s.revoked[id]++
	if s.onRevoke != nil {
		s.onRevoke(id)
	}
	s.Relay.RevokeConsoleSession(id)
}

func (s *sessionAuditService) Automation(ctx context.Context, p model.Principal, operation string, payload json.RawMessage) (any, error) {
	s.callers = append(s.callers, p)
	return s.Relay.Automation(ctx, p, operation, payload)
}

type automationFixture struct {
	t       *testing.T
	server  *server
	service *sessionAuditService
	token   string
}

func newAutomationFixture(t *testing.T) *automationFixture {
	t.Helper()
	dir := t.TempDir()
	core, err := relay.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.Close() })
	token, err := os.ReadFile(filepath.Join(dir, "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	svc := &sessionAuditService{Relay: core, revoked: make(map[string]int)}
	s := New(svc).(*server)
	if svc.validate == nil {
		t.Fatal("live session validator was not installed")
	}
	svc.onRevoke = func(id string) {
		// A hook may immediately consult the installed validator. TryLock makes
		// an accidental lock inversion fail rather than hanging the test.
		if !s.consoleMu.TryLock() {
			t.Error("revocation hook invoked while consoleMu was held")
			return
		}
		s.consoleMu.Unlock()
		if svc.validate(id) {
			t.Error("revoked session still passes the live validator")
		}
	}
	return &automationFixture{t: t, server: s, service: svc, token: string(token)}
}

func (f *automationFixture) request(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:7331/console/api/"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:7331")
	r.Header.Set("X-Meshra-Console", "1")
	r.Header.Set("X-Meshra-CSRF", csrf)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}

func (f *automationFixture) login() (*http.Cookie, string) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"token": f.token})
	w := f.request("POST", "login", string(body), nil, "")
	if w.Code != 200 {
		f.t.Fatal(w.Code, w.Body.String())
	}
	var response struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		f.t.Fatal(err)
	}
	return w.Result().Cookies()[0], response.CSRF
}

func (f *automationFixture) expire(cookie *http.Cookie) {
	f.t.Helper()
	key := consoleSessionKey(cookie.Value)
	f.server.consoleMu.Lock()
	session := f.server.sessions[key]
	session.Expires = time.Now().Add(-time.Second)
	f.server.sessions[key] = session
	f.server.consoleMu.Unlock()
}

func TestAutomationAuthenticationAndConsoleProvenance(t *testing.T) {
	f := newAutomationFixture(t)
	cookie, csrf := f.login()
	key := consoleSessionKey(cookie.Value)
	if key == cookie.Value || !f.service.validate(key) || f.service.validate(cookie.Value) || f.service.validate("") {
		t.Fatal("validator does not require a live session digest")
	}
	for _, credential := range []string{"", "invalid"} {
		w := httpCall(t, f.server, "POST", "/v1/automation/runtime.status", credential, `{}`, nil)
		if w.Code != 401 {
			t.Fatal("automation bypassed bearer authentication", w.Code)
		}
	}
	if w := httpCall(t, f.server, "POST", "/v1/automation/runtime.status", f.token, `{}`, map[string]string{"Origin": "http://127.0.0.1:7331"}); w.Code != 403 {
		t.Fatal("bearer automation accepted a browser origin", w.Code)
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		csrf   string
		status int
	}{
		{nil, csrf, 401}, {cookie, "", 403}, {cookie, "invalid", 403},
	} {
		w := f.request("POST", "automation/runtime.status", `{}`, tc.cookie, tc.csrf)
		if w.Code != tc.status {
			t.Fatal("console automation gate bypassed", w.Code, w.Body.String())
		}
	}
	if len(f.service.callers) != 0 {
		t.Fatal("rejected request reached the optional service")
	}
	// Unsupported operations retain the real relay's error through the transport.
	if w := f.request("POST", "automation/not.a.real.operation", `{}`, cookie, csrf); w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(f.service.callers) != 1 || !f.service.callers[0].Admin || f.service.callers[0].ConsoleSessionID != key {
		t.Fatal("console provenance is not the validated credential digest")
	}
	w := httpCall(t, f.server, "POST", "/v1/automation/not.a.real.operation", f.token, `{"consoleSessionId":"`+key+`"}`, map[string]string{"X-Meshra-CSRF": csrf, "X-Meshra-Console-Session": key, "Cookie": "meshra_console=" + cookie.Value})
	if w.Code != 404 || len(f.service.callers) != 2 || f.service.callers[1].ConsoleSessionID != "" {
		t.Fatal("bearer request obtained console provenance", w.Code, w.Body.String())
	}
	encoded, err := json.Marshal(f.service.callers[0])
	if err != nil || strings.Contains(string(encoded), key) || strings.Contains(string(encoded), cookie.Value) {
		t.Fatal("console provenance was serialized", string(encoded), err)
	}
}

func TestAutomationBoundedObjectAndOptionalBoundary(t *testing.T) {
	f := newAutomationFixture(t)
	cookie, csrf := f.login()
	for _, tc := range []struct {
		body   string
		status int
	}{
		{"", 400}, {`null`, 400}, {`[]`, 400}, {`"object"`, 400}, {`42`, 400},
		{`{} {}`, 400}, {`{} trailing`, 400}, {`{"x":`, 400},
		{`{"x":"` + strings.Repeat("x", maxBody) + `"}`, 413},
	} {
		w := httpCall(t, f.server, "POST", "/v1/automation/runtime.status", f.token, tc.body, nil)
		if w.Code != tc.status {
			t.Errorf("bearer payload: got %d want %d", w.Code, tc.status)
		}
		w = f.request("POST", "automation/runtime.status", tc.body, cookie, csrf)
		if w.Code != tc.status {
			t.Errorf("console payload: got %d want %d", w.Code, tc.status)
		}
	}
	if len(f.service.callers) != 0 {
		t.Fatal("invalid JSON reached automation service")
	}
	if w := httpCall(t, f.server, "POST", "/v1/automation/runtime.status", f.token, `{}`, map[string]string{"Content-Type": "text/plain"}); w.Code != 415 {
		t.Fatal("automation accepted non-JSON content type", w.Code)
	}
	if w := httpCall(t, f.server, "GET", "/v1/automation/runtime.status", f.token, "", nil); w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Fatal("automation method gate missing", w.Code)
	}
	// Hiding optional interfaces must not break any legacy Service endpoint.
	legacy := New(struct{ model.Service }{f.service.Relay})
	if w := httpCall(t, legacy, "POST", "/v1/automation/runtime.status", f.token, `{}`, nil); w.Code != 503 {
		t.Fatal("missing optional boundary must be unavailable", w.Code, w.Body.String())
	}
	if w := httpCall(t, legacy, "GET", "/v1/agents", f.token, "", nil); w.Code != 200 {
		t.Fatal("optional boundary changed legacy API", w.Code, w.Body.String())
	}
	legacyConsole := &automationFixture{t: t, server: legacy.(*server), token: f.token}
	legacyCookie, legacyCSRF := legacyConsole.login()
	if w := legacyConsole.request("POST", "automation/runtime.status", `{}`, legacyCookie, legacyCSRF); w.Code != 503 {
		t.Fatal("console missing optional boundary must be unavailable", w.Code, w.Body.String())
	}
}

func TestConsoleSessionRevocationHooksAndExpiry(t *testing.T) {
	f := newAutomationFixture(t)
	current, csrf := f.login()
	assertRevoked := func(cookie *http.Cookie) {
		t.Helper()
		key := consoleSessionKey(cookie.Value)
		if f.service.validate(key) || f.service.revoked[key] != 1 {
			t.Fatal("session invalidation not propagated exactly once", f.service.revoked[key])
		}
		if w := f.request("GET", "session", "", cookie, ""); w.Code != 401 {
			t.Fatal("revoked session retained cookie authentication", w.Code)
		}
	}
	other, _ := f.login()
	key := consoleSessionKey(other.Value)
	f.server.consoleMu.Lock()
	id := f.server.sessions[key].ID
	f.server.consoleMu.Unlock()
	body, _ := json.Marshal(map[string]string{"id": id})
	if w := f.request("POST", "sessions/revoke", string(body), current, csrf); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertRevoked(other)
	other, _ = f.login()
	if w := f.request("POST", "sessions/revoke-others", `{}`, current, csrf); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertRevoked(other)
	for _, cleanup := range []string{"validator", "request", "login", "sessions"} {
		t.Run(cleanup, func(t *testing.T) {
			expired, _ := f.login()
			f.expire(expired)
			switch cleanup {
			case "validator":
				if f.service.validate(consoleSessionKey(expired.Value)) {
					t.Fatal("expired digest remains valid")
				}
			case "request":
				f.request("GET", "session", "", expired, "")
			case "login":
				f.login()
			case "sessions":
				f.request("GET", "sessions", "", current, "")
			}
			assertRevoked(expired)
		})
	}
	other, _ = f.login()
	if w := f.request("POST", "account/save", `{"username":"operator","password":"a long test passphrase"}`, current, csrf); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertRevoked(other)
	if !f.service.validate(consoleSessionKey(current.Value)) {
		t.Fatal("account change revoked the retained current session")
	}
	if w := f.request("POST", "logout", `{}`, current, csrf); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertRevoked(current)
}
