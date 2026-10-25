package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConsoleSessionOriginCSRFAndStatic(t *testing.T) {
	h := New(&fakeService{})
	request := func(method, path, origin, token, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		body := []byte("{}")
		if path == "/console/api/login" {
			body, _ = json.Marshal(map[string]string{"token": token})
		}
		r := httptest.NewRequest(method, "http://127.0.0.1:7331"+path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("X-Meshra-Console", "1")
		if csrf != "" {
			r.Header.Set("X-Meshra-CSRF", csrf)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/console/", "", "", "", nil); w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("embedded console unavailable", w.Code)
	}
	if w := request("POST", "/console/api/login", "https://evil.example", "admin", "", nil); w.Code != 403 {
		t.Fatal("foreign origin admitted", w.Code)
	}
	if w := request("POST", "/console/api/login", "http://127.0.0.1:7331", "agent", "", nil); w.Code != 401 {
		t.Fatal("agent escalated", w.Code)
	}
	w := request("POST", "/console/api/login", "http://127.0.0.1:7331", "admin", "", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/console/api/" {
		t.Fatal("unsafe session cookie", cookie)
	}
	var data map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	csrf := data["csrf"]
	if w = request("POST", "/console/api/logout", "http://127.0.0.1:7331", "", "", cookie); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	if w = request("GET", "/console/api/agents", "http://evil.example", "", "", cookie); w.Code != 403 {
		t.Fatal("origin bypass", w.Code)
	}
	if w = request("GET", "/console/api/agents", "http://127.0.0.1:7331", "", "", cookie); w.Code != 200 {
		t.Fatal("same-origin query failed", w.Code)
	}
	if w = request("POST", "/console/api/logout", "http://127.0.0.1:7331", "", csrf, cookie); w.Code != 200 {
		t.Fatal("logout failed", w.Code)
	}
	if w = request("GET", "/console/api/agents", "", "", "", cookie); w.Code != 401 {
		t.Fatal("revoked session works", w.Code)
	}
	r := httptest.NewRequest("GET", "http://attacker.example/console/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("DNS rebinding host admitted")
	}
	// Legacy bearer API remains closed to browser Origins.
	r = httptest.NewRequest("GET", "http://127.0.0.1:7331/v1/agents", nil)
	r.Header.Set("Origin", "http://127.0.0.1:7331")
	r.Header.Set("Authorization", "Bearer admin")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("legacy browser boundary weakened")
	}
}

func TestConsoleExpiredSessionAndLoginHeaders(t *testing.T) {
	s := New(&fakeService{}).(*server)
	login := func(origin, header, site string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://127.0.0.1:7331/console/api/login", bytes.NewBufferString(`{"token":"admin"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Meshra-Console", header)
		r.Header.Set("Sec-Fetch-Site", site)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := login("http://127.0.0.1:7331", "", "same-origin"); w.Code != 403 {
		t.Fatal("login custom-header boundary bypassed")
	}
	if w := login("http://127.0.0.1:7331", "1", "cross-site"); w.Code != 403 {
		t.Fatal("fetch metadata boundary bypassed")
	}
	w := login("http://127.0.0.1:7331", "1", "same-origin")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	s.consoleMu.Lock()
	session := s.sessions[consoleSessionKey(cookie.Value)]
	session.Expires = time.Now().Add(-time.Second)
	s.sessions[consoleSessionKey(cookie.Value)] = session
	s.consoleMu.Unlock()
	r := httptest.NewRequest("GET", "http://127.0.0.1:7331/console/api/session", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("expired console session retained access")
	}
}

func TestConsoleSessionsRevokeWithoutLeakingCredentials(t *testing.T) {
	s := New(&fakeService{}).(*server)
	request := func(method, path, body, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://127.0.0.1:7331/console/api/"+path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Meshra-Console", "1")
		r.Header.Set("X-Meshra-CSRF", csrf)
		r.Header.Set("User-Agent", "Mozilla/5.0 (Linux) Chrome/123")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	login := func() (*http.Cookie, string) {
		w := request("POST", "login", `{"token":"admin"}`, "", nil)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var v map[string]string
		json.Unmarshal(w.Body.Bytes(), &v)
		return w.Result().Cookies()[0], v["csrf"]
	}
	first, csrf := login()
	second, otherCSRF := login()
	w := request("GET", "sessions", "", "", first)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	text := w.Body.String()
	for _, secret := range []string{first.Value, second.Value, csrf, otherCSRF} {
		if bytes.Contains([]byte(text), []byte(secret)) {
			t.Fatal("session credential leaked")
		}
	}
	var entries []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
		Device  string `json:"device"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatal(entries)
	}
	id := ""
	for _, v := range entries {
		if !v.Current {
			id = v.ID
		}
		if v.Device != "Chrome on Linux" {
			t.Fatal(v.Device)
		}
	}
	body, _ := json.Marshal(map[string]string{"id": id})
	if w = request("POST", "sessions/revoke", string(body), "", first); w.Code != 403 {
		t.Fatal("revoke bypassed CSRF")
	}
	if w = request("POST", "sessions/revoke", string(body), csrf, first); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("GET", "session", "", "", second); w.Code != 401 {
		t.Fatal("revoked browser still authenticated")
	}
	third, _ := login()
	if w = request("POST", "sessions/revoke-others", `{}`, csrf, first); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("GET", "session", "", "", third); w.Code != 401 {
		t.Fatal("bulk revocation failed")
	}
	if w = request("GET", "session", "", "", first); w.Code != 200 {
		t.Fatal("bulk revocation ended own session")
	}
}
