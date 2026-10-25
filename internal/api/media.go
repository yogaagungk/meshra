package api

import (
	"context"
	"net/http"
	"strconv"

	"meshra/internal/model"
)

type browserFrameService interface {
	BrowserFrame(context.Context, model.Principal, string) ([]byte, error)
}
type voiceAudioService interface {
	VoiceAudio(context.Context, model.Principal, string) ([]byte, error)
}

// Only the authenticated console router calls this handler. No bearer route,
// worker path, query-selected file, cached response or remote HTML is exposed.
func (s *server) consoleMedia(w http.ResponseWriter, r *http.Request, p model.Principal, kind, id string) {
	if r.Method != http.MethodGet {
		method(w, "GET")
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, model.Err(400, "media reads do not accept query parameters"))
		return
	}
	if id == "" || len(id) > 128 {
		fail(w, model.Err(404, "media not found"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var data []byte
	var err error
	var contentType string
	if kind == "browser" {
		svc, ok := s.service.(browserFrameService)
		if !ok {
			fail(w, model.Err(503, "browser frames unavailable"))
			return
		}
		data, err = svc.BrowserFrame(r.Context(), p, id)
		contentType = "image/png"
	} else {
		svc, ok := s.service.(voiceAudioService)
		if !ok {
			fail(w, model.Err(503, "local voice audio unavailable"))
			return
		}
		data, err = svc.VoiceAudio(r.Context(), p, id)
		contentType = "audio/wav"
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
