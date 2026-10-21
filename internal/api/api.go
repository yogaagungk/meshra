// Package api exposes the authenticated local JSON, SSE, and MCP transports.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"meshra/internal/model"
)

const maxBody = 128 * 1024

type server struct {
	service       model.Service
	consoleMu     sync.Mutex
	consoleAuthMu sync.Mutex
	sessions      map[string]consoleSession
	loginWindow   time.Time
	loginAttempts int
}

func New(service model.Service) http.Handler {
	return &server{service: service, sessions: map[string]consoleSession{}}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/" || r.URL.Path == "/console" || strings.HasPrefix(r.URL.Path, "/console/") {
		s.console(w, r)
		return
	}
	if _, present := r.Header["Origin"]; present {
		fail(w, model.Err(403, "browser origins are not permitted"))
		return
	}
	auth := strings.Fields(r.Header.Get("Authorization"))
	if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, model.Err(401, "bearer token required"))
		return
	}
	p, err := s.service.Authenticate(auth[1])
	if err != nil {
		fail(w, err)
		return
	}
	if r.URL.Path == "/mcp" {
		s.mcp(w, r, p)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	if path == r.URL.Path {
		fail(w, model.Err(404, "endpoint not found"))
		return
	}
	parts := strings.Split(path, "/")
	var result any
	status := http.StatusOK
	switch {
	case len(parts) == 2 && parts[0] == "collaboration":
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		var q model.CollaborationRequest
		if !decode(w, r, &q) {
			return
		}
		svc, ok := s.service.(model.CollaborationService)
		if !ok {
			fail(w, model.Err(501, "collaboration unavailable"))
			return
		}
		result, err = svc.Collaborate(p, parts[1], q)
	case path == "identity":
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		result = map[string]any{"admin": p.Admin, "agentId": p.AgentID, "sessionId": p.SessionID, "taskId": p.TaskID}
	case path == "metrics" || path == "doctor":
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		if !p.Admin {
			fail(w, model.Err(403, "administrator required"))
			return
		}
		if path == "metrics" {
			result, err = s.service.Metrics(p)
		} else {
			result, err = s.service.Doctor(p)
		}
	case path == "agents":
		switch r.Method {
		case http.MethodGet:
			result, err = s.service.Agents(p)
		case http.MethodPost:
			var req model.CreateAgent
			if !decode(w, r, &req) {
				return
			}
			result, err = s.service.Create(p, req)
			status = http.StatusCreated
		default:
			method(w, "GET, POST")
			return
		}
	case len(parts) == 2 && parts[0] == "agents":
		switch r.Method {
		case http.MethodGet:
			result, err = s.service.Agent(p, parts[1])
		case http.MethodDelete:
			err = s.service.Remove(p, parts[1])
			result = map[string]bool{"removed": err == nil}
		default:
			method(w, "GET, DELETE")
			return
		}
	case len(parts) == 3 && parts[0] == "agents":
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		switch parts[2] {
		case "start", "stop", "restart":
			result, err = s.service.Action(p, parts[1], parts[2])
		case "token":
			var token string
			token, err = s.service.Token(p, parts[1])
			result = map[string]string{"token": token}
		default:
			fail(w, model.Err(404, "agent action not found"))
			return
		}
	case path == "send" || path == "request":
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		var req model.SendRequest
		if !decode(w, r, &req) {
			return
		}
		req.Type = path
		wait := false
		if path == "request" {
			raw := r.URL.Query().Get("wait")
			if raw != "" && raw != "true" && raw != "false" {
				fail(w, model.Err(400, "wait must be true or false"))
				return
			}
			wait = raw == "true"
			if wait && (req.TimeoutSeconds < 1 || req.TimeoutSeconds > 86400) {
				fail(w, model.Err(400, "wait requires timeoutSeconds between 1 and 86400"))
				return
			}
		}
		if wait && !p.Admin {
			// Polling requires status permission. Check it before durable admission,
			// rather than enqueueing work and only then reporting forbidden.
			if _, err = s.service.Agent(p, p.AgentID); err != nil {
				fail(w, err)
				return
			}
		}
		var task model.Task
		task, err = s.service.Send(p, req)
		if err == nil && wait {
			s.waitTask(w, r, p, task)
			return
		}
		result = task
		status = http.StatusAccepted
	case len(parts) == 2 && parts[0] == "tasks":
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		result, err = s.service.Task(p, parts[1])
	case len(parts) == 3 && parts[0] == "tasks":
		if parts[2] == "trace" {
			if r.Method != http.MethodGet {
				method(w, "GET")
				return
			}
			result, err = s.service.Trace(p, parts[1])
			break
		}
		if parts[2] == "events" {
			if r.Method != http.MethodGet {
				method(w, "GET")
				return
			}
			s.events(w, r, p, parts[1])
			return
		}
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		switch parts[2] {
		case "cancel":
			result, err = s.service.Cancel(p, parts[1])
		case "retry":
			result, err = s.service.Retry(p, parts[1])
		default:
			fail(w, model.Err(404, "task action not found"))
			return
		}
	default:
		fail(w, model.Err(404, "endpoint not found"))
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, status, result)
}

// Waiting affects only this transport: admission remains durable and disconnecting
// never invokes Cancel. A delayed scheduler must not be represented as terminal.
func (s *server) waitTask(w http.ResponseWriter, r *http.Request, p model.Principal, task model.Task) {
	deadline := time.NewTimer(time.Until(task.Deadline))
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		if r.Context().Err() != nil {
			return
		}
		if model.Terminal(task.State) {
			w.Header().Set("X-Meshra-Wait-Status", "terminal")
			respond(w, http.StatusOK, task)
			return
		}
		expired := false
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			expired = true
		case <-poll.C:
		}
		current, err := s.service.Task(p, task.ID)
		if err != nil {
			fail(w, err)
			return
		}
		task = current
		if expired && !model.Terminal(task.State) {
			w.Header().Set("X-Meshra-Wait-Status", "deadline")
			respond(w, http.StatusAccepted, task)
			return
		}
	}
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "internal server error"
	var e *model.Error
	if errors.As(err, &e) && e.Status >= 400 && e.Status <= 599 {
		status = e.Status
		message = e.Message
	}
	respond(w, status, map[string]string{"error": message})
}
func method(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	fail(w, model.Err(405, "method not allowed"))
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, model.Err(415, "Content-Type must be application/json"))
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		decodeError(w, err)
		return false
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		decodeError(w, err)
		return false
	}
	return true
}
func decodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, model.Err(413, "request body exceeds 128 KiB"))
		return
	}
	fail(w, model.Err(400, "invalid JSON: "+err.Error()))
}
func cursor(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("after")
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		raw = last
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, model.Err(400, "event cursor must be a nonnegative integer")
	}
	return n, nil
}

func (s *server) events(w http.ResponseWriter, r *http.Request, p model.Principal, id string) {
	after, err := cursor(r)
	if err != nil {
		fail(w, err)
		return
	}
	follow := r.URL.Query().Get("follow")
	if follow != "" && follow != "true" && follow != "false" {
		fail(w, model.Err(400, "follow must be true or false"))
		return
	}
	// Authorize and fetch before committing streaming headers.
	events, err := s.service.Events(p, id, after)
	if err != nil {
		fail(w, err)
		return
	}
	if follow != "true" {
		respond(w, 200, events)
		return
	}
	task, err := s.service.Task(p, id)
	if err != nil {
		fail(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, model.Err(500, "streaming unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	writeEvents := func(batch []model.Event) bool {
		for _, event := range batch {
			if event.ID <= after {
				continue
			}
			data, e := json.Marshal(event)
			if e != nil {
				return false
			}
			if _, e = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, "message", data); e != nil {
				return false
			}
			after = event.ID
		}
		flusher.Flush()
		return true
	}
	for {
		if !writeEvents(events) {
			return
		}
		if model.Terminal(task.State) {
			// Re-read after observing terminal state, draining every bounded store page.
			// A transition racing the previous read must be delivered before closing.
			for {
				select {
				case <-r.Context().Done():
					return
				default:
				}
				final, e := s.service.Events(p, id, after)
				if e != nil || len(final) == 0 {
					return
				}
				previous := after
				if !writeEvents(final) || after == previous {
					return
				}
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err = io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		case <-poll.C:
		}
		task, err = s.service.Task(p, id)
		if err != nil {
			return
		}
		events, err = s.service.Events(p, id, after)
		if err != nil {
			return
		}
	}
}
