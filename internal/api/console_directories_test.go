package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestConsoleDirectoryCreateSessionCSRFAndResult(t *testing.T) {
	f := newAutomationFixture(t)
	cookie, csrf := f.login()
	parent := t.TempDir()
	payload, err := json.Marshal(map[string]string{"path": parent, "name": "New project"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		csrf   string
		status int
	}{
		{nil, "", 401}, {cookie, "", 403}, {cookie, "invalid", 403},
	} {
		w := f.request("POST", "directories/create", string(payload), tc.cookie, tc.csrf)
		if w.Code != tc.status {
			t.Fatal("directory creation gate bypassed", w.Code, w.Body.String())
		}
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatal("unauthorized request created a directory", entries, err)
	}
	if w := f.request("GET", "directories/create", "", cookie, csrf); w.Code != 405 {
		t.Fatal("creation accepted wrong method", w.Code, w.Body.String())
	}
	w := f.request("POST", "directories/create", string(payload), cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != filepath.Join(canonical, "New project") {
		t.Fatal("unexpected created path", result.Path)
	}
	if info, err := os.Stat(result.Path); err != nil || !info.IsDir() {
		t.Fatal("created directory missing", info, err)
	}
	if w := f.request("POST", "directories/create", string(payload), cookie, csrf); w.Code != 409 {
		t.Fatal("existing directory was not a conflict", w.Code, w.Body.String())
	}
	payload, err = json.Marshal(map[string]string{"path": parent, "name": "../escape"})
	if err != nil {
		t.Fatal(err)
	}
	if w := f.request("POST", "directories/create", string(payload), cookie, csrf); w.Code != 400 {
		t.Fatal("traversal was accepted", w.Code, w.Body.String())
	}
}
