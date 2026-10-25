package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"meshra/internal/model"
)

func TestAutomationClientSelectedIDCreatesThenRevisionUpdates(t *testing.T) {
	fixture := newAutomationFixture(t)
	cookie, csrf := fixture.login()
	workspaceValue, err := fixture.service.Relay.Console(model.Principal{Admin: true}, "workspaces/add", model.ConsoleRequest{Path: t.TempDir(), Name: "Create-status workspace"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := workspaceValue.(model.Workspace)
	executable := filepath.Join(t.TempDir(), "reviewed-server")
	if err = os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	server := map[string]any{"id": "explicit-mcp-fixture", "name": "Client-selected fixture", "transport": "stdio", "executable": executable, "workspaceId": workspace.ID}
	payload, _ := json.Marshal(map[string]any{"server": server})
	created := fixture.request(http.MethodPost, "automation/mcp.save", string(payload), cookie, csrf)
	if created.Code != http.StatusCreated {
		t.Fatalf("new client-selected ID returned%d: %s", created.Code, created.Body.String())
	}
	var resource struct {
		ID       string `json:"id"`
		Revision int    `json:"revision"`
		Enabled  bool   `json:"enabled"`
	}
	if err = json.Unmarshal(created.Body.Bytes(), &resource); err != nil {
		t.Fatal(err)
	}
	if resource.Enabled {
		t.Fatal("saving a connection launched or enabled it")
	}
	server["name"] = "Reviewed metadata update"
	payload, _ = json.Marshal(map[string]any{"server": server, "expectedRevision": resource.Revision})
	updated := fixture.request(http.MethodPost, "automation/mcp.save", string(payload), cookie, csrf)
	if updated.Code != http.StatusOK {
		t.Fatalf("revision update returned%d: %s", updated.Code, updated.Body.String())
	}
	stale := fixture.request(http.MethodPost, "automation/mcp.save", string(payload), cookie, csrf)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale update returned%d", stale.Code)
	}
}
