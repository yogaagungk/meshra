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
