package relay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"meshra/internal/model"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func (r *Relay) workspace(id string) (model.Workspace, error) {
	var w model.Workspace
	var body string
	if err := r.db.QueryRow("SELECT body FROM workspaces WHERE id=?", id).Scan(&body); err != nil {
		if err == sql.ErrNoRows {
			return w, model.Err(404, "workspace not found")
		}
		return w, err
	}
	err := json.Unmarshal([]byte(body), &w)
	return w, err
}
func (r *Relay) workspaceOperation(op string, q model.ConsoleRequest) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	switch op {
	case "workspaces":
		rows, err := r.db.Query("SELECT body FROM workspaces ORDER BY json_extract(body,'$.name') LIMIT 32")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []model.Workspace{}
		for rows.Next() {
			var body string
			var w model.Workspace
			if err = rows.Scan(&body); err != nil {
				return nil, err
			}
			if err = json.Unmarshal([]byte(body), &w); err != nil {
				return nil, err
			}
			out = append(out, w)
		}
		return out, rows.Err()
	case "workspaces/add":
		path, err := canonicalWorkspace(q.Path)
		if err != nil {
			return nil, err
		}
		var count int
		if err = r.db.QueryRow("SELECT count(*) FROM workspaces").Scan(&count); err != nil {
			return nil, err
		}
		if count >= 32 {
			return nil, model.Err(409, "workspace limit reached (32)")
		}
		name := strings.TrimSpace(q.Name)
		if name == "" {
			name = filepath.Base(path)
		}
		if len(name) > 80 || !safeRegistryText(name) || redact(name) != name {
			return nil, model.Err(400, "invalid workspace name")
		}
		w := model.Workspace{ID: randomID(), Name: name, Path: path, CreatedAt: now()}
		b, _ := json.Marshal(w)
		if _, err = r.db.Exec("INSERT INTO workspaces VALUES(?,?,?)", w.ID, string(b), path); err != nil {
			return nil, model.Err(409, "this directory is already a workspace")
		}
		return w, nil
	case "workspaces/remove":
		// Saved conversations retain their binding. Refuse removal until no chats use it.
		var count int
		if err := r.db.QueryRow("SELECT count(*) FROM chat_settings WHERE json_extract(body,'$.workspaceId')=?", q.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, model.Err(409, "workspace is used by a conversation")
		}
		_, err := r.db.Exec("DELETE FROM workspaces WHERE id=?", q.ID)
		return map[string]bool{"removed": err == nil}, err
	}
	return nil, model.Err(404, "unknown workspace operation")
}
func (r *Relay) chatSettings(id string) (model.ChatSettings, error) {
	var v model.ChatSettings
	var b string
	err := r.db.QueryRow("SELECT body FROM chat_settings WHERE conversation_id=?", id).Scan(&b)
	if err == sql.ErrNoRows {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	err = json.Unmarshal([]byte(b), &v)
	return v, err
}
func (r *Relay) chatAgent(t model.Task, a model.Agent) (model.Agent, error) {
	if !t.Chat {
		return a, nil
	}
	var stored int
	if err := r.db.QueryRow("SELECT count(*) FROM chat_settings WHERE conversation_id=?", t.ConversationID).Scan(&stored); err != nil {
		return a, err
	}
	if stored == 0 {
		return a, validateWorkspace(a)
	}
	v, err := r.chatSettings(t.ConversationID)
	if err != nil {
		return a, err
	}
	a.Config = v.Config
	if v.WorkspaceID != "" {
		w, e := r.workspace(v.WorkspaceID)
		if e != nil {
			return a, e
		}
		a.Cwd = w.Path
	}
	if err = validateWorkspace(a); err != nil {
		return a, err
	}
	return a, nil
}
func safeWorkspacePath(path string) bool {
	if filepath.IsAbs(path) || len(path) > 4096 || !safeRegistryText(path) || strings.Contains(path, "\\") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// Snapshot through os.Root: symlinks cannot escape the registered directory.
// File reads, directory pages and Git output are bounded, with no shell execution.
func (r *Relay) inspectWorkspace(op string, q model.ConsoleRequest) (any, error) {
	r.mu.Lock()
	w, err := r.workspace(q.WorkspaceID)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	path := q.Path
	if path == "" {
		path = "."
	}
	if !safeWorkspacePath(path) {
		return nil, model.Err(400, "path must stay inside the workspace")
	}
	root, err := os.OpenRoot(w.Path)
	if err != nil {
		return nil, model.Err(403, "workspace cannot be opened")
	}
	defer root.Close()
	switch op {
	case "workspace/tree":
		dir, err := root.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, model.Err(403, "directory cannot be opened")
		}
		defer dir.Close()
		type entry struct {
			Name      string `json:"name"`
			Path      string `json:"path"`
			Directory bool   `json:"directory"`
		}
		entries := []entry{}
		if len(q.AfterName) > 255 || strings.ContainsAny(q.AfterName, "/\\") {
			return nil, model.Err(400, "invalid directory cursor")
		}
		for {
			batch, e := dir.ReadDir(128)
			for _, v := range batch {
				if v.Name() == ".git" || !safeRegistryText(v.Name()) || v.Type()&os.ModeSymlink != 0 || v.Name() <= q.AfterName {
					continue
				}
				item := entry{v.Name(), filepath.Join(path, v.Name()), v.IsDir()}
				i := sort.Search(len(entries), func(i int) bool { return entries[i].Name >= item.Name })
				if i >= 101 {
					continue
				}
				entries = append(entries, entry{})
				copy(entries[i+1:], entries[i:])
				entries[i] = item
				if len(entries) > 101 {
					entries = entries[:101]
				}
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, model.Err(403, "directory listing unavailable")
			}
		}
		more := len(entries) > 100
		if more {
			entries = entries[:100]
		}
		after := ""
		if more {
			after = entries[len(entries)-1].Name
		}
		return map[string]any{"path": path, "entries": entries, "hasMore": more, "nextAfter": after}, nil
	case "workspace/file":
		file, err := root.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, model.Err(403, "file cannot be opened")
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return nil, model.Err(400, "select a regular text file")
		}
		b, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
		if err != nil {
			return nil, err
		}
		truncated := len(b) > 64*1024
		if truncated {
			b = b[:64*1024]
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil, model.Err(415, "binary files are not previewed")
		}
		text := strings.ToValidUTF8(string(b), "�")
		return map[string]any{"path": path, "content": redact(text), "truncated": truncated, "size": info.Size()}, nil
	case "workspace/git":
		branch, branchErr := gitRead(w.Path, "branch", "--show-current")
		if branchErr != nil {
			return map[string]any{"isRepository": false}, nil
		}
		status, err := gitRead(w.Path, "status", "--porcelain=v1", "-z", "--untracked-files=normal")
		if err != nil {
			return nil, model.Err(503, "Git status unavailable or exceeds 256 KiB")
		}
		return map[string]any{"isRepository": true, "branch": strings.TrimSpace(branch), "status": redact(status)}, nil
	case "workspace/diff":
		if path != "." {
			f, e := root.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
			if e == nil {
				f.Close()
			} else if !errors.Is(e, os.ErrNotExist) {
				return nil, model.Err(403, "file cannot be opened")
			}
		}
		args := []string{"diff", "--no-ext-diff", "--no-textconv", "--"}
		if q.Status == "staged" {
			args = []string{"diff", "--cached", "--no-ext-diff", "--no-textconv", "--"}
		}
		if path != "." {
			args = append(args, path)
		}
		output, err := gitRead(w.Path, args...)
		if err != nil {
			return nil, model.Err(503, "diff unavailable or exceeds 256 KiB; select a smaller file")
		}
		return map[string]any{"content": redact(output), "staged": q.Status == "staged"}, nil
	}
	return nil, model.Err(404, "unknown workspace inspection")
}

type limitedGitOutput struct{ bytes.Buffer }

func (b *limitedGitOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 256*1024 {
		return 0, errors.New("Git output limit")
	}
	return b.Buffer.Write(p)
}
func gitRead(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base := []string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "diff.external=", "-c", "core.quotePath=true", "--literal-pathspecs", "-C", dir}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	var out limitedGitOutput
	cmd.Stdout = &out
	err := cmd.Run()
	text := out.String()
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	return text, err
}
