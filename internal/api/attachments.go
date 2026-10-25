package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"meshra/internal/model"
)

// Called only after console cookie, same-origin and CSRF checks. There is no
// bearer upload route and no JSON/base64 transformation of the uploaded bytes.
func (s *server) consoleAttachment(w http.ResponseWriter, r *http.Request, p model.Principal, id string) {
	svc, ok := s.service.(model.AttachmentService)
	if !ok {
		fail(w, model.Err(503, "attachments unavailable"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if id == "upload" {
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			fail(w, model.Err(400, "invalid upload query"))
			return
		}
		for key, values := range query {
			if key != "conversationId" || len(values) != 1 {
				fail(w, model.Err(400, "invalid upload query"))
				return
			}
		}
		name := r.Header.Get("X-Meshra-Filename")
		if len(name) > 4096 {
			fail(w, model.Err(400, "filename metadata too long"))
			return
		}
		name, err = url.QueryUnescape(name)
		if err != nil {
			fail(w, model.Err(400, "invalid encoded filename"))
			return
		}
		if r.ContentLength > model.MaxAttachmentBytes {
			fail(w, model.Err(413, "attachment exceeds 25 MiB"))
			return
		}
		body := http.MaxBytesReader(w, r.Body, model.MaxAttachmentBytes+1)
		defer body.Close()
		a, err := svc.UploadAttachment(r.Context(), p, query.Get("conversationId"), name, r.Header.Get("Content-Type"), body, r.ContentLength)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				err = model.Err(413, "attachment exceeds 25 MiB")
			}
			fail(w, err)
			return
		}
		respond(w, http.StatusCreated, a)
		return
	}
	if id == "" || strings.ContainsAny(id, "/\\") {
		fail(w, model.Err(404, "attachment not found"))
		return
	}
	if r.Method != http.MethodGet {
		method(w, "GET")
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, model.Err(400, "attachment download does not accept query parameters"))
		return
	}
	a, body, err := svc.ReadAttachment(r.Context(), p, id)
	if err != nil {
		fail(w, err)
		return
	}
	defer body.Close()
	// Always download, including HTML, SVG and unknown binary content. Uploaded
	// bytes are never mounted as console-origin pages or guessed host paths.
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}))
	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}
