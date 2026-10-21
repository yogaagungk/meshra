package api

import (
	"meshra/internal/model"
	"net/http"
	"sort"
	"strings"
	"time"
)

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
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	if path == "sessions" && r.Method == http.MethodGet {
		type entry struct {
			ID      string    `json:"id"`
			Device  string    `json:"device"`
			Created time.Time `json:"createdAt"`
			Seen    time.Time `json:"lastSeen"`
			Expires time.Time `json:"expiresAt"`
			Current bool      `json:"current"`
		}
		entries := []entry{}
		for k, v := range s.sessions {
			if time.Now().After(v.Expires) {
				delete(s.sessions, k)
				continue
			}
			entries = append(entries, entry{v.ID, v.Device, v.Created, v.LastSeen, v.Expires, k == key})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Seen.After(entries[j].Seen) })
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
	for k, v := range s.sessions {
		if (path == "sessions/revoke-others" && k != key) || (path == "sessions/revoke" && v.ID == q.ID) {
			delete(s.sessions, k)
			revoked++
		}
	}
	if path == "sessions/revoke" && revoked == 0 {
		fail(w, model.Err(404, "session not found"))
		return
	}
	respond(w, 200, map[string]int{"revoked": revoked})
}
