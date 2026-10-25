package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"meshra/internal/consoleui"
	"meshra/internal/model"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type consoleSession struct {
	ID        string
	Device    string
	Created   time.Time
	LastSeen  time.Time
	Principal model.Principal
	CSRF      string
	Expires   time.Time
}

func randomConsoleToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localConsoleRequest(r *http.Request) bool {
	if !loopbackHost(r.Host) {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	origin := r.Header.Get("Origin")
	return origin == "" || origin == scheme+"://"+r.Host
}
func (s *server) console(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if r.URL.Path == "/console/api/automation/mcp-oauth-callback" {
		s.mcpOAuthCallback(w, r)
		return
	}
	if !localConsoleRequest(r) {
		fail(w, model.Err(403, "console requires a loopback host and same-origin request"))
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/console/api/") {
		if r.Method != "GET" && r.Method != "HEAD" {
			method(w, "GET, HEAD")
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/console" {
			http.Redirect(w, r, "/console/", http.StatusTemporaryRedirect)
			return
		}
		assets, _ := fs.Sub(consoleui.Assets, "dist")
		http.StripPrefix("/console/", http.FileServer(http.FS(assets))).ServeHTTP(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/console/api/")
	if path == "auth" && r.Method == http.MethodGet {
		configured := false
		if svc, ok := s.service.(model.ConsoleAccountService); ok {
			account, err := svc.ConsoleAccount(model.Principal{Admin: true})
			if err != nil {
				fail(w, err)
				return
			}
			configured = account.Configured
		}
		respond(w, 200, map[string]bool{"configured": configured})
		return
	}
	if path == "login" {
		s.consoleAuthMu.Lock()
		defer s.consoleAuthMu.Unlock()
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		if r.Header.Get("X-Meshra-Console") != "1" {
			fail(w, model.Err(403, "console login header required"))
			return
		}
		s.consoleMu.Lock()
		if time.Since(s.loginWindow) > time.Minute {
			s.loginWindow = time.Now()
			s.loginAttempts = 0
		}
		s.loginAttempts++
		attempts := s.loginAttempts
		s.consoleMu.Unlock()
		if attempts > 30 {
			w.Header().Set("Retry-After", "60")
			fail(w, model.Err(429, "too many login attempts; wait one minute"))
			return
		}
		var q struct {
			Token    string `json:"token"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decode(w, r, &q) {
			return
		}
		var p model.Principal
		var err error
		if q.Token != "" && q.Username == "" && q.Password == "" {
			p, err = s.service.Authenticate(q.Token)
		} else if svc, ok := s.service.(model.ConsoleAccountService); ok && q.Token == "" {
			p, err = svc.AuthenticateConsole(q.Username, q.Password)
		} else {
			err = model.Err(401, "invalid sign-in credentials")
		}
		if err != nil || !p.Admin {
			fail(w, model.Err(401, "invalid sign-in credentials"))
			return
		}
		p.ConsoleSessionID = ""
		key := randomConsoleToken()
		session := consoleSession{ID: randomConsoleToken(), Device: sessionDevice(r.UserAgent()), Created: time.Now(), LastSeen: time.Now(), Principal: p, CSRF: randomConsoleToken(), Expires: time.Now().Add(8 * time.Hour)}
		s.consoleMu.Lock()
		expired := s.expireConsoleSessionsLocked(time.Now())
		if len(s.sessions) >= 128 {
			s.consoleMu.Unlock()
			s.revokeConsoleSessions(expired)
			fail(w, model.Err(429, "too many console sessions"))
			return
		}
		s.sessions[consoleSessionKey(key)] = session
		s.consoleMu.Unlock()
		s.revokeConsoleSessions(expired)
		http.SetCookie(w, &http.Cookie{Name: "meshra_console", Value: key, Path: "/console/api/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
		respond(w, 200, map[string]any{"csrf": session.CSRF, "operator": "Local operator", "expiresAt": session.Expires, "version": "0.4.0"})
		return
	}
	cookie, err := r.Cookie("meshra_console")
	if err != nil {
		fail(w, model.Err(401, "console session required"))
		return
	}
	key := consoleSessionKey(cookie.Value)
	session, ok := s.lookupConsoleSession(key, true)
	if !ok {
		fail(w, model.Err(401, "console session expired; connect again"))
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Meshra-CSRF")), []byte(session.CSRF)) != 1 {
		fail(w, model.Err(403, "CSRF token required"))
		return
	}
	if path == "sessions" || path == "sessions/revoke" || path == "sessions/revoke-others" {
		s.consoleSessions(w, r, key, path)
		return
	}
	p := session.Principal
	p.ConsoleSessionID = key
	if path == "account" || path == "account/save" {
		svc, ok := s.service.(model.ConsoleAccountService)
		if !ok {
			fail(w, model.Err(501, "account sign-in is unavailable"))
			return
		}
		if path == "account" {
			if r.Method != http.MethodGet {
				method(w, "GET")
				return
			}
			value, err := svc.ConsoleAccount(p)
			if err != nil {
				fail(w, err)
				return
			}
			respond(w, 200, value)
			return
		}
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		var q struct {
			Username        string `json:"username"`
			Password        string `json:"password"`
			CurrentPassword string `json:"currentPassword"`
		}
		if !decode(w, r, &q) {
			return
		}
		s.consoleAuthMu.Lock()
		defer s.consoleAuthMu.Unlock()
		// Reauthentication failures share the login limiter.
		s.consoleMu.Lock()
		if time.Since(s.loginWindow) > time.Minute {
			s.loginWindow = time.Now()
			s.loginAttempts = 0
		}
		s.loginAttempts++
		limited := s.loginAttempts > 30
		s.consoleMu.Unlock()
		if limited {
			w.Header().Set("Retry-After", "60")
			fail(w, model.Err(429, "too many authentication attempts; wait one minute"))
			return
		}
		value, err := svc.SaveConsoleAccount(p, q.Username, q.Password, q.CurrentPassword)
		if err != nil {
			fail(w, err)
			return
		}
		s.consoleMu.Lock()
		var revoked []string
		for key := range s.sessions {
			if key != p.ConsoleSessionID {
				delete(s.sessions, key)
				revoked = append(revoked, key)
			}
		}
		s.consoleMu.Unlock()
		s.revokeConsoleSessions(revoked)
		respond(w, 200, value)
		return
	}
	if path == "session" && r.Method == "GET" {
		respond(w, 200, map[string]any{"csrf": session.CSRF, "operator": "Local operator", "version": "0.4.0", "expiresAt": session.Expires})
		return
	}
	if path == "logout" && r.Method == "POST" {
		s.consoleMu.Lock()
		delete(s.sessions, key)
		s.consoleMu.Unlock()
		s.revokeConsoleSessions([]string{key})
		http.SetCookie(w, &http.Cookie{Name: "meshra_console", Path: "/console/api/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		respond(w, 200, map[string]bool{"loggedOut": true})
		return
	}
	var result any
	status := 200
	parts := strings.Split(path, "/")
	switch {
	case len(parts) == 3 && ((parts[0] == "browser" && parts[2] == "frame") || (parts[0] == "voice" && parts[2] == "audio")):
		s.consoleMedia(w, r, p, parts[0], parts[1])
		return
	case len(parts) == 2 && parts[0] == "attachments":
		s.consoleAttachment(w, r, p, parts[1])
		return
	case len(parts) == 2 && parts[0] == "automation" && parts[1] == "events":
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		s.consoleAutomationEvents(w, r, p, key)
		return
	case len(parts) == 2 && parts[0] == "automation" && parts[1] != "":
		s.automation(w, r, p, parts[1])
		return
	case path == "ai-status" || path == "usage-history" || path == "preferences" || path == "resources" || path == "usage" || path == "workspaces" || path == "conversations" || path == "tasks" || path == "timeline" || path == "chat-state" || path == "harnesses":
		if r.Method != "GET" {
			method(w, "GET")
			return
		}
		svc, ok := s.service.(model.ConsoleService)
		if !ok {
			fail(w, model.Err(501, "console unavailable"))
			return
		}
		q, e := consoleQuery(r)
		if e != nil {
			fail(w, e)
			return
		}
		result, err = svc.Console(p, path, q)
	case path == "quota" || path == "preferences/save" || path == "workspaces/add" || path == "workspaces/remove" || path == "workspace/tree" || path == "workspace/file" || path == "workspace/git" || path == "workspace/diff" || path == "models" || path == "send" || path == "context-check" || path == "regenerate" || path == "chat-create" || path == "directories":
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		svc, ok := s.service.(model.ConsoleService)
		if !ok {
			fail(w, model.Err(501, "console unavailable"))
			return
		}
		var q model.ConsoleRequest
		if !decode(w, r, &q) {
			return
		}
		result, err = svc.Console(p, path, q)
		status = 202
		if path == "directories" || path == "preferences/save" || strings.HasPrefix(path, "workspace") || path == "models" || path == "quota" {
			status = 200
		}
	case path == "events":
		if r.Method != "GET" {
			method(w, "GET")
			return
		}
		s.consoleEvents(w, r, p, key)
		return
	case path == "agents":
		if r.Method == "GET" {
			result, err = s.service.Agents(p)
		} else if r.Method == "POST" {
			var q model.CreateAgent
			if !decode(w, r, &q) {
				return
			}
			result, err = s.service.Create(p, q)
			status = 201
		} else {
			method(w, "GET, POST")
			return
		}
	case len(parts) == 3 && parts[0] == "agents" && (parts[2] == "start" || parts[2] == "stop" || parts[2] == "restart"):
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		result, err = s.service.Action(p, parts[1], parts[2])
	case len(parts) == 2 && parts[0] == "tasks":
		if r.Method != "GET" {
			method(w, "GET")
			return
		}
		result, err = s.service.Task(p, parts[1])
	case len(parts) == 3 && parts[0] == "tasks":
		switch parts[2] {
		case "events":
			if r.Method != "GET" {
				method(w, "GET")
				return
			}
			after, e := cursor(r)
			if e != nil {
				fail(w, e)
				return
			}
			result, err = s.service.Events(p, parts[1], after)
		case "cancel", "retry":
			if r.Method != "POST" {
				method(w, "POST")
				return
			}
			if parts[2] == "cancel" {
				result, err = s.service.Cancel(p, parts[1])
			} else {
				if r.Header.Get("X-Meshra-Confirm-Retry") != "inspect-effects" {
					fail(w, model.Err(409, "inspect external effects and explicitly confirm retry"))
					return
				}
				result, err = s.service.Retry(p, parts[1])
			}
		default:
			err = model.Err(404, "task operation unavailable")
		}
	case len(parts) == 2 && parts[0] == "collaboration":
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		svc, ok := s.service.(model.CollaborationService)
		if !ok {
			fail(w, model.Err(501, "collaboration unavailable"))
			return
		}
		var q model.CollaborationRequest
		if !decode(w, r, &q) {
			return
		}
		result, err = svc.Collaborate(p, parts[1], q)
	case path == "metrics":
		if r.Method != "GET" {
			method(w, "GET")
			return
		}
		result, err = s.service.Metrics(p)
	default:
		err = model.Err(404, "console endpoint not found")
	}
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, status, result)
}
func consoleQuery(r *http.Request) (model.ConsoleRequest, error) {
	q := model.ConsoleRequest{Harness: r.URL.Query().Get("harness"), ConversationID: r.URL.Query().Get("conversationId"), Keyword: r.URL.Query().Get("keyword"), Status: r.URL.Query().Get("status")}
	for _, v := range []struct {
		name   string
		target *int64
	}{{"after", &q.After}, {"before", &q.Before}} {
		raw := r.URL.Query().Get(v.name)
		if raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				return q, model.Err(400, "invalid cursor")
			}
			*v.target = n
		}
	}
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return q, model.Err(400, "invalid event cursor")
		}
		q.After = n
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return q, model.Err(400, "limit must be 1..100")
		}
		q.Limit = n
	}
	return q, nil
}

// consoleAutomationEvents streams feature resource-change metadata. It never
// carries task text, tool arguments or secret material.
func (s *server) consoleAutomationEvents(w http.ResponseWriter, r *http.Request, p model.Principal, key string) {
	svc, ok := s.service.(model.ConsoleService)
	if !ok {
		fail(w, model.Err(501, "feed unavailable"))
		return
	}
	q, err := consoleQuery(r)
	if err != nil {
		fail(w, err)
		return
	}
	if q.After < 0 {
		fail(w, model.Err(400, "after must not be negative"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, model.Err(500, "stream unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprint(w, "event: resync\ndata: {}\n\n")
	flusher.Flush()
	poll := time.NewTicker(700 * time.Millisecond)
	defer poll.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		batch, err := svc.Console(p, "automation-events", q)
		if err != nil {
			return
		}
		events, _ := batch.([]map[string]any)
		deadline := http.NewResponseController(w)
		_ = deadline.SetWriteDeadline(time.Now().Add(10 * time.Second))
		for _, event := range events {
			b, _ := json.Marshal(event)
			id, _ := event["id"].(int64)
			if _, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, b); err != nil {
				return
			}
			q.After = id
		}
		flusher.Flush()
		_ = deadline.SetWriteDeadline(time.Time{})
		if len(events) < 100 {
			select {
			case <-r.Context().Done():
				return
			case <-poll.C:
			case <-heartbeat.C:
				if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
		if !s.validConsoleSession(key) {
			fmt.Fprint(w, "event: expired\ndata: {}\n\n")
			flusher.Flush()
			return
		}
	}
}

func (s *server) consoleEvents(w http.ResponseWriter, r *http.Request, p model.Principal, key string) {
	svc, ok := s.service.(model.ConsoleService)
	if !ok {
		fail(w, model.Err(501, "feed unavailable"))
		return
	}
	q, err := consoleQuery(r)
	if err != nil {
		fail(w, err)
		return
	}
	if watermark, e := svc.Console(p, "watermark", model.ConsoleRequest{}); e == nil {
		if seq, ok := watermark.(int64); ok && q.After > seq {
			q.After = 0
		}
	}
	batch, err := svc.Console(p, "feed", q)
	if err != nil {
		fail(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, model.Err(500, "stream unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprint(w, "event: resync\ndata: {}\n\n")
	flusher.Flush()
	poll := time.NewTicker(700 * time.Millisecond)
	defer poll.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		events := batch.([]map[string]any)
		deadline := http.NewResponseController(w)
		_ = deadline.SetWriteDeadline(time.Now().Add(10 * time.Second))
		for _, event := range events {
			b, _ := json.Marshal(event)
			id := event["id"].(int64)
			if _, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, b); err != nil {
				return
			}
			q.After = id
		}
		flusher.Flush()
		_ = deadline.SetWriteDeadline(time.Time{})
		if len(events) < 256 {
			select {
			case <-r.Context().Done():
				return
			case <-poll.C:
			case <-heartbeat.C:
				if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
		if !s.validConsoleSession(key) {
			fmt.Fprint(w, "event: expired\ndata: {}\n\n")
			flusher.Flush()
			return
		}
		batch, err = svc.Console(p, "feed", q)
		if err != nil {
			return
		}
	}
}
