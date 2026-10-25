package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"meshra/internal/model"
)

func attachmentHTTPRequest(f *automationFixture, method, path, mediaType, filename string, data []byte, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:7331/console/api/"+path, bytes.NewReader(data))
	r.Header.Set("Content-Type", mediaType)
	r.Header.Set("Origin", "http://127.0.0.1:7331")
	r.Header.Set("X-Meshra-CSRF", csrf)
	r.Header.Set("X-Meshra-Filename", url.QueryEscape(filename))
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}

func TestConsoleAttachmentBinaryAndActiveContentDownloads(t *testing.T) {
	f := newAutomationFixture(t)
	cookie, csrf := f.login()
	for _, tc := range []struct {
		name, mediaType string
		data            []byte
	}{
		{`..\../unsafe\file.bin`, "application/octet-stream", []byte{0, 0xff, 0xfe, 'x', 0, 0x80}},
		{"../../page.html", "text/html", []byte(`<script>fetch('/console/api/logout',{method:'POST'})</script>`)},
		{"../../vector.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)},
	} {
		t.Run(tc.mediaType, func(t *testing.T) {
			w := attachmentHTTPRequest(f, "POST", "attachments/upload", tc.mediaType, tc.name, tc.data, cookie, csrf)
			if w.Code != 201 {
				t.Fatal(w.Code, w.Body.String())
			}
			var a model.Attachment
			if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
				t.Fatal(err)
			}
			if a.ID == "" || strings.ContainsAny(a.Name, "/\\") {
				t.Fatalf("unsafe metadata: %+v", a)
			}
			w = attachmentHTTPRequest(f, "GET", "attachments/"+a.ID, "", "", nil, cookie, "")
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), tc.data) {
				t.Fatalf("binary download changed: %d %x", w.Code, w.Body.Bytes())
			}
			disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			if err != nil || disposition != "attachment" || params["filename"] != a.Name {
				t.Fatalf("unsafe download disposition: %q %v", w.Header().Get("Content-Disposition"), err)
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != tc.mediaType {
				t.Fatal("missing safe binary response headers", w.Header())
			}
			w = attachmentHTTPRequest(f, "GET", "attachments/"+a.ID, "", "", nil, nil, "")
			if w.Code != 401 {
				t.Fatal("download accepted no session", w.Code)
			}
		})
	}
}

func TestConsoleAttachmentUploadAuthenticationCSRFAndBounds(t *testing.T) {
	f := newAutomationFixture(t)
	cookie, csrf := f.login()
	for _, tc := range []struct {
		cookie *http.Cookie
		csrf   string
		status int
	}{
		{nil, "", 401}, {cookie, "", 403}, {cookie, "bad", 403},
	} {
		w := attachmentHTTPRequest(f, "POST", "attachments/upload", "application/octet-stream", "file.bin", []byte("bytes"), tc.cookie, tc.csrf)
		if w.Code != tc.status {
			t.Fatal("upload gate bypassed", w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		method, path, mediaType string
		status                  int
	}{
		{"GET", "attachments/upload", "application/octet-stream", 405},
		{"POST", "attachments/upload?unknown=value", "application/octet-stream", 400},
		{"POST", "attachments/upload?conversationId=a&conversationId=b", "application/octet-stream", 400},
		{"POST", "attachments/upload?conversationId=missing", "application/octet-stream", 404},
		{"POST", "attachments/upload", "invalid content type", 400},
	} {
		w := attachmentHTTPRequest(f, tc.method, tc.path, tc.mediaType, "file.bin", []byte("bytes"), cookie, csrf)
		if w.Code != tc.status {
			t.Fatal(tc.path, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:7331/console/api/attachments/upload", strings.NewReader("x"))
	r.Header.Set("X-Meshra-CSRF", csrf)
	r.Header.Set("Content-Type", "application/octet-stream")
	r.AddCookie(cookie)
	r.ContentLength = model.MaxAttachmentBytes + 1
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal("declared upload size unbounded", w.Code, w.Body.String())
	}
	// No large allocation: a bounded synthetic byte stream exercises the real
	// unknown-length HTTP upload limit and its storage cleanup path.
	r = httptest.NewRequest("POST", "http://127.0.0.1:7331/console/api/attachments/upload", io.LimitReader(attachmentZeroReader{}, model.MaxAttachmentBytes+1))
	r.Header.Set("X-Meshra-CSRF", csrf)
	r.Header.Set("Content-Type", "application/octet-stream")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal("streamed upload size unbounded", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("POST", "http://127.0.0.1:7331/console/api/attachments/upload", strings.NewReader("x"))
	r.Header.Set("X-Meshra-CSRF", csrf)
	r.Header.Set("X-Meshra-Filename", "%not-encoded")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("invalid filename accepted", w.Code)
	}
	f.expire(cookie)
	w = attachmentHTTPRequest(f, "POST", "attachments/upload", "application/octet-stream", "file.bin", nil, cookie, csrf)
	if w.Code != 401 {
		t.Fatal("expired session uploaded", w.Code)
	}
}

type attachmentZeroReader struct{}

func (attachmentZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestConsoleAttachmentRemovalPreservesUnavailableReference(t *testing.T) {
	f := newAutomationFixture(t)
	cookie, csrf := f.login()
	w := attachmentHTTPRequest(f, "POST", "attachments/upload", "text/plain", "draft.txt", []byte("draft"), cookie, csrf)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var a model.Attachment
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	payload := `{"id":"` + a.ID + `","confirm":true}`
	w = httpCall(t, f.server, "POST", "/v1/automation/attachments.remove", f.token, payload, nil)
	if w.Code != 403 {
		t.Fatal("bearer removed attachment", w.Code, w.Body.String())
	}
	for _, raw := range []string{`{"id":"` + a.ID + `"}`, `{"id":"` + a.ID + `","confirm":true,"path":"outside"}`} {
		w = f.request("POST", "automation/attachments.remove", raw, cookie, csrf)
		if w.Code != 400 {
			t.Fatal("unsafe remove accepted", w.Code, w.Body.String())
		}
	}
	w = f.request("POST", "automation/attachments.remove", payload, cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = attachmentHTTPRequest(f, "GET", "attachments/"+a.ID, "", "", nil, cookie, "")
	if w.Code != 404 {
		t.Fatal("removed bytes still downloadable", w.Code)
	}
	w = f.request("POST", "automation/attachments.list", `{}`, cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var listed []model.Attachment
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].ID != a.ID || !listed[0].Unavailable {
		t.Fatalf("missing unavailable reference: %s %v", w.Body.String(), err)
	}
}
