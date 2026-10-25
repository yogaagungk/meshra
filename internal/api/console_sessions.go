package api

import (
	"crypto/sha256"
	"encoding/hex"
	"meshra/internal/model"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Session keys are digests of browser credentials, never credentials themselves.
func consoleSessionKey(cookie string) string {
	digest := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(digest[:])
}

func (s *server) revokeConsoleSessions(keys []string) {
	if svc, ok := s.service.(model.ConsoleSessionService); ok {
		for _, key := range keys {
			svc.RevokeConsoleSession(key)
		}
	}
}

// Callers must release consoleMu before notifying the optional service.
func (s *server) expireConsoleSessionsLocked(now time.Time) []string {
	var expired []string
	for key, session := range s.sessions {
		if !now.Before(session.Expires) {
			delete(s.sessions, key)
			expired = append(expired, key)
		}
	}
	return expired
}

func (s *server) lookupConsoleSession(key string, touch bool) (consoleSession, bool) {
	s.consoleMu.Lock()
	session, ok := s.sessions[key]
	expired := ok && !time.Now().Before(session.Expires)
	if expired {
		delete(s.sessions, key)
		ok = false
	} else if ok && touch {
		session.LastSeen = time.Now()
		s.sessions[key] = session
	}
	s.consoleMu.Unlock()
	if expired {
		s.revokeConsoleSessions([]string{key})
	}
	return session, ok
}

func (s *server) validConsoleSession(key string) bool {
	session, ok := s.lookupConsoleSession(key, false)
	return ok && session.Principal.Admin
}

func sessionDevice(agent string) string {
	browser := "Browser"
	for _, b := range []struct{ marker, name string }{{"Edg/", "Edge"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}} {
		if strings.Contains(agent, b.marker) {
			browser = b.name
			break
		}
	}
	system := ""
	for _, s := range []struct{ marker, name string }{{"Android", "Android"}, {"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Windows", "Windows"}, {"Macintosh", "macOS"}, {"Linux", "Linux"}} {
		if strings.Contains(agent, s.marker) {
			system = s.name
			break
		}
	}
	if system != "" {
		return browser + " on " + system
	}
	return browser
}

// Public IDs are independent of cookie credentials. Listing sessions never reveals
// authentication secrets or CSRF tokens; all mutations use the current session's CSRF.
func (s *server) consoleSessions(w http.ResponseWriter, r *http.Request, key, path string) {
	if path == "sessions" && r.Method == http.MethodGet {
		type entry struct {
			ID      string    `json:"id"`
			Device  string    `json:"device"`
			Created time.Time `json:"createdAt"`
			Seen    time.Time `json:"lastSeen"`
			Expires time.Time `json:"expiresAt"`
			Current bool      `json:"current"`
		}
		s.consoleMu.Lock()
		expired := s.expireConsoleSessionsLocked(time.Now())
		entries := []entry{}
		for k, v := range s.sessions {
			entries = append(entries, entry{v.ID, v.Device, v.Created, v.LastSeen, v.Expires, k == key})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Seen.After(entries[j].Seen) })
		s.consoleMu.Unlock()
		s.revokeConsoleSessions(expired)
		respond(w, 200, entries)
		return
	}
	if path == "sessions" {
		method(w, "GET")
		return
	}
	if r.Method != http.MethodPost {
		method(w, "POST")
		return
	}
	var q struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &q) {
		return
	}
	revoked := 0
	var keys []string
	s.consoleMu.Lock()
	for k, v := range s.sessions {
		if (path == "sessions/revoke-others" && k != key) || (path == "sessions/revoke" && v.ID == q.ID) {
			delete(s.sessions, k)
			keys = append(keys, k)
			revoked++
		}
	}
	s.consoleMu.Unlock()
	s.revokeConsoleSessions(keys)
	if path == "sessions/revoke" && revoked == 0 {
		fail(w, model.Err(404, "session not found"))
		return
	}
	respond(w, 200, map[string]int{"revoked": revoked})
}
