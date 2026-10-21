package api

import (
	"bytes"
	"encoding/json"
	"meshra/internal/relay"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestConsolePasswordLoginAndSessionRevocation(t *testing.T) {
	dir := t.TempDir()
	core, err := relay.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	token, err := os.ReadFile(filepath.Join(dir, "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(core)
	request := func(path string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		var raw []byte
		method := "GET"
		if body != nil {
			raw, _ = json.Marshal(body)
			method = "POST"
		}
		r := httptest.NewRequest(method, "http://127.0.0.1:7331/console/api/"+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://127.0.0.1:7331")
		r.Header.Set("X-Meshra-Console", "1")
		r.Header.Set("X-Meshra-CSRF", csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	login := func(body any) (*http.Cookie, string) {
		w := request("login", body, nil, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var v struct {
			CSRF string `json:"csrf"`
		}
		json.Unmarshal(w.Body.Bytes(), &v)
		return w.Result().Cookies()[0], v.CSRF
	}
	if w := request("account", nil, nil, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	cookie, csrf := login(map[string]string{"token": string(token)})
	other, _ := login(map[string]string{"token": string(token)})
	details := map[string]string{"username": "operator", "password": "a long test passphrase"}
	if w := request("account/save", details, cookie, ""); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	if w := request("account/save", details, cookie, csrf); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("session", nil, other, ""); w.Code != 401 {
		t.Fatal("other session survived change", w.Code)
	}
	if w := request("auth", nil, nil, ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"configured":true`)) {
		t.Fatal(w.Body.String())
	}
	if w := request("login", map[string]string{"username": "operator", "password": "wrong"}, nil, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	passwordCookie, passwordCSRF := login(details)
	if w := request("session", nil, passwordCookie, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	details["password"] = "a replacement passphrase"
	if w := request("account/save", details, passwordCookie, passwordCSRF); w.Code != 403 {
		t.Fatal("missing current password accepted", w.Code)
	}
	details["currentPassword"] = "a long test passphrase"
	if w := request("account/save", details, passwordCookie, passwordCSRF); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("session", nil, cookie, ""); w.Code != 401 {
		t.Fatal("original session survived rotation", w.Code)
	}
	for i := 0; i < 31; i++ {
		request("login", map[string]string{"username": "operator", "password": "wrong"}, nil, "")
	}
	if w := request("login", details, nil, ""); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("login not rate limited", w.Code)
	}
}
