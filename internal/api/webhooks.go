package api

import (
	"context"
	"io"
	"net/http"
	"strings"

	"meshra/internal/model"
)

// RoutineWebhookService is the optional signed ingress of the routine
// scheduler. It replaces no bearer route: only this path reaches it.
type RoutineWebhookService interface {
	RoutineWebhook(ctx context.Context, routineID, timestamp, eventID, signature string, body []byte) (any, error)
}

// routineWebhook accepts exactly one signed ingress path. It requires a
// loopback Host, forbids a browser Origin, and bounds the raw body before any
// signature is computed.
func (s *server) routineWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		method(w, "POST")
		return
	}
	svc, ok := s.service.(RoutineWebhookService)
	if !ok {
		fail(w, model.Err(404, "endpoint not found"))
		return
	}
	if _, present := r.Header["Origin"]; present {
		fail(w, model.Err(403, "browser origins are not permitted"))
		return
	}
	if !loopbackHost(r.Host) {
		fail(w, model.Err(403, "routine webhooks require a loopback host"))
		return
	}
	routineID := strings.TrimPrefix(r.URL.Path, "/v1/automation-webhooks/")
	if routineID == "" || strings.Contains(routineID, "/") {
		fail(w, model.Err(404, "endpoint not found"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		fail(w, model.Err(413, "webhook body exceeds 65536 bytes"))
		return
	}
	result, err := svc.RoutineWebhook(r.Context(), routineID, r.Header.Get("X-Meshra-Timestamp"), r.Header.Get("X-Meshra-Event-ID"), r.Header.Get("X-Meshra-Signature"), body)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusAccepted, result)
}
