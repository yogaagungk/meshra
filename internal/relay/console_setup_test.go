package relay

import (
	"context"
	"fmt"
	"meshra/internal/model"
	"os"
	"path/filepath"
	"testing"
)

func TestHarnessChatCreatesBoundedManagedIdentities(t *testing.T) {
	r, _ := openTest(t, nil)
	for _, harness := range []string{"codex", "claude", "omp", "opencode"} {
		var previous string
		for i := 0; i < 3; i++ {
			v, err := r.Console(admin, "chat-create", model.ConsoleRequest{Harness: harness, Title: "A private chat"})
			if err != nil {
				t.Fatal(err)
			}
			c := v.(model.Conversation)
			if c.Mode != "chat" || len(c.Participants) != 1 {
				t.Fatal(c)
			}
			if previous != "" && previous != c.Participants[0] {
				t.Fatal("a new identity was leaked for each chat")
			}
			previous = c.Participants[0]
			a, err := r.Agent(admin, previous)
			if err != nil || !a.ManagedChat || a.Harness != harness {
				t.Fatal(a, err)
			}
			if a.Cwd == "" || a.Policy.Delegate || len(a.Policy.Operations) != 0 {
				t.Fatal("unsafe managed identity", a)
			}
			if i == 0 {
				startTest(t, r, a.ID)
				reply := awaitTask(t, r, sendChat(t, r, c, "Hello").ID)
				if reply.State != "completed" {
					t.Fatal(reply)
				}
			}
		}
	}
	agents, err := r.Agents(admin)
	if err != nil || len(agents) != 4 {
		t.Fatal("managed agent count grew with chats", len(agents), err)
	}
	_, err = r.Console(model.Principal{AgentID: agents[0].ID}, "chat-create", model.ConsoleRequest{Harness: "codex"})
	requireStatus(t, err, 403)
	_, err = r.Console(admin, "chat-create", model.ConsoleRequest{Harness: "unknown"})
	requireStatus(t, err, 400)
	_, err = r.Console(admin, "chat-create", model.ConsoleRequest{Harness: "codex", Title: "password=private"})
	requireStatus(t, err, 400)
}

func TestDirectoryExplorerBoundedPagination(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 215; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("folder-%03d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "ordinary-file.txt"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	after := ""
	for page := 0; page < 3; page++ {
		v, err := browseDirectories(model.ConsoleRequest{Path: root, AfterName: after})
		if err != nil {
			t.Fatal(err)
		}
		out := v.(map[string]any)
		entries := out["entries"].([]directoryEntry)
		if len(entries) > 100 {
			t.Fatal("unbounded directory page")
		}
		for _, entry := range entries {
			if seen[entry.Name] || filepath.Dir(entry.Path) != root {
				t.Fatal("invalid directory page", entry)
			}
			seen[entry.Name] = true
		}
		after = out["nextAfter"].(string)
	}
	if len(seen) != 215 {
		t.Fatal("missing or unexpected directory entries", len(seen))
	}
	_, err := browseDirectories(model.ConsoleRequest{Path: filepath.Join(root, "ordinary-file.txt")})
	requireStatus(t, err, 400)
	r, _ := openTest(t, nil)
	_, err = r.Console(model.Principal{AgentID: "codex:untrusted"}, "directories", model.ConsoleRequest{Path: root})
	requireStatus(t, err, 403)
}

func TestDirectoryCreateCanonicalChildAndConflicts(t *testing.T) {
	r, _ := openTest(t, nil)
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	value, err := r.Console(admin, "directories/create", model.ConsoleRequest{Path: alias, Name: "New project"})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	path := value.(map[string]string)["path"]
	if path != filepath.Join(canonical, "New project") {
		t.Fatal("created path is not canonical", path)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatal("child directory was not created", info, err)
	}
	for _, name := range []string{"New project", "existing-file", "existing-link"} {
		switch name {
		case "existing-file":
			if err := os.WriteFile(filepath.Join(root, name), []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
		case "existing-link":
			if err := os.Symlink(t.TempDir(), filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
		_, err := r.Console(admin, "directories/create", model.ConsoleRequest{Path: root, Name: name})
		requireStatus(t, err, 409)
	}
	if data, err := os.ReadFile(filepath.Join(root, "existing-file")); err != nil || string(data) != "preserve" {
		t.Fatal("existing file was modified", string(data), err)
	}
}

func TestDirectoryCreateRejectsUnsafeNamesParentsAndNonAdmin(t *testing.T) {
	r, _ := openTest(t, nil)
	root := t.TempDir()
	for _, name := range []string{"", ".", "..", "../escape", "nested/child", `nested\child`, "/absolute", " leading", "trailing ", "trailing.", "bad\nname", "bad\x00name", "bad:name", string([]byte{0xff})} {
		_, err := r.Console(admin, "directories/create", model.ConsoleRequest{Path: root, Name: name})
		requireStatus(t, err, 400)
	}
	for _, parent := range []string{"", ".", filepath.Join(root, "missing")} {
		_, err := r.Console(admin, "directories/create", model.ConsoleRequest{Path: parent, Name: "child"})
		requireStatus(t, err, 400)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := r.Console(admin, "directories/create", model.ConsoleRequest{Path: file, Name: "child"})
	requireStatus(t, err, 400)
	for _, principal := range []model.Principal{{}, {AgentID: "codex:untrusted"}} {
		_, err := r.Console(principal, "directories/create", model.ConsoleRequest{Path: root, Name: "unauthorized"})
		requireStatus(t, err, 403)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "file" {
		t.Fatal("rejected creation changed parent", entries, err)
	}
}

func TestManagedChatRestartStartsFreshWithoutResumingIdleThreads(t *testing.T) {
	calls := make(chan model.Agent, 8)
	adapters := map[string]model.Adapter{"codex": {Factory: func(_ context.Context, a model.Agent, _ model.Emit) (model.Runtime, error) {
		calls <- a
		if a.ManagedChat && (a.SessionID != "" || !a.Ephemeral) {
			t.Error("managed slot tried to resume a durable idle thread")
		}
		sid := a.SessionID
		if sid == "" {
			sid = "native-" + randomID()
		}
		return &fakeRuntime{session: sid}, nil
	}}}
	dir := t.TempDir()
	r, err := Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	v, err := r.Console(admin, "chat-create", model.ConsoleRequest{Harness: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	managed := v.(model.Conversation).Participants[0]
	first := startTest(t, r, managed)
	<-calls
	if first.SessionID == "" {
		t.Fatal("factory did not allocate idle thread")
	}
	if _, err = r.Action(admin, managed, "restart"); err != nil {
		t.Fatal(err)
	}
	<-calls
	ordinary := createTest(t, r, "codex", "ordinary")
	before := startTest(t, r, ordinary.ID)
	<-calls
	if _, err = r.Action(admin, ordinary.ID, "restart"); err != nil {
		t.Fatal(err)
	}
	received := <-calls
	if received.SessionID != before.SessionID || received.Ephemeral {
		t.Fatal("ordinary durable session changed", received)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, adapters)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	startTest(t, reopened, managed)
	input := <-calls
	if input.SessionID != "" || !input.Ephemeral {
		t.Fatal("daemon recovery tried to resume managed idle thread", input)
	}
}
