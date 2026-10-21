package relay

import (
	"database/sql"
	"encoding/json"
	"io"
	"meshra/internal/model"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Reuse at most one managed identity per harness. Its private workspace and
// transient native turns are independent of user-registered project agents.
func (r *Relay) createHarnessChat(p model.Principal, q model.ConsoleRequest) (any, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, model.Err(503, "relay is stopping")
	}
	adapter, ok := r.adapters[q.Harness]
	if !ok || adapter.Factory == nil || (q.Harness != "codex" && q.Harness != "claude" && q.Harness != "omp" && q.Harness != "opencode") {
		r.mu.Unlock()
		return nil, model.Err(400, "unsupported AI harness")
	}
	if err := validConfig(q.Config, q.Harness); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if q.WorkspaceID != "" {
		if _, err := r.workspace(q.WorkspaceID); err != nil {
			r.mu.Unlock()
			return nil, err
		}
	}
	title := strings.TrimSpace(q.Title)
	if title == "" {
		title = "New chat"
	}
	if len(title) > 512 || redact(title) != title {
		r.mu.Unlock()
		return nil, model.Err(400, "invalid title")
	}
	var id string
	err := r.db.QueryRow(`SELECT id FROM agents WHERE json_extract(body,'$.harness')=? AND json_extract(body,'$.managedChat')=1 LIMIT 1`, q.Harness).Scan(&id)
	if err == sql.ErrNoRows {
		cwd := filepath.Join(filepath.Dir(r.lock.Name()), "chat-workspaces", q.Harness)
		if err = os.MkdirAll(cwd, 0700); err == nil {
			var a model.Agent
			a, err = r.createLocked(p, model.CreateAgent{Harness: q.Harness, Name: "chat-" + randomID()[:12], DisplayName: q.Harness, Cwd: cwd, ManagedChat: true, Policy: model.Policy{Destinations: []string{}, Operations: []string{}}})
			id = a.ID
		}
	}
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	value, err := r.Collaborate(p, "conversation.create", model.CollaborationRequest{Title: title, Mode: "chat", Participants: []string{id}})
	if err != nil {
		return nil, err
	}
	c := value.(model.Conversation)
	settings := model.ChatSettings{WorkspaceID: q.WorkspaceID, Config: q.Config}
	body, _ := json.Marshal(settings)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err = r.db.Exec("INSERT INTO chat_settings VALUES(?,?)", c.ID, string(body)); err != nil {
		return nil, err
	}
	c.ChatSettings = &settings
	return c, nil
}

type directoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Browse only directory metadata. Retain at most 101 names while scanning, even
// for a folder containing millions of entries. Run outside the relay mutex.
func browseDirectories(q model.ConsoleRequest) (any, error) {
	path := q.Path
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	if len(path) > 4096 || !safeRegistryText(path) || len(q.AfterName) > 255 || strings.ContainsAny(q.AfterName, "/\\") {
		return nil, model.Err(400, "invalid directory path")
	}
	path, err := canonicalWorkspace(path)
	if err != nil {
		return nil, err
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, model.Err(403, "this directory cannot be opened")
	}
	defer dir.Close()
	names := make([]string, 0, 101)
	for {
		batch, readErr := dir.ReadDir(128)
		for _, entry := range batch {
			name := entry.Name()
			if !entry.IsDir() || name <= q.AfterName || !safeRegistryText(name) {
				continue
			}
			i := sort.SearchStrings(names, name)
			if i >= 101 {
				continue
			}
			names = append(names, "")
			copy(names[i+1:], names[i:])
			names[i] = name
			if len(names) > 101 {
				names = names[:101]
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, model.Err(403, "directory listing is unavailable")
		}
	}
	more := len(names) > 100
	if more {
		names = names[:100]
	}
	entries := []directoryEntry{}
	for _, name := range names {
		entries = append(entries, directoryEntry{Name: name, Path: filepath.Join(path, name)})
	}
	after := ""
	if more {
		after = names[len(names)-1]
	}
	return map[string]any{"path": path, "parent": filepath.Dir(path), "entries": entries, "nextAfter": after, "hasMore": more}, nil
}
