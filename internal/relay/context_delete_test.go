package relay

import (
	"meshra/internal/model"
	"testing"
)

func TestContextDelete(t *testing.T) {
	r, _ := openTest(t, nil)
	a := createTest(t, r, "claude", "contextowner")
	b := createTest(t, r, "claude", "contextreader")
	owner := model.Principal{AgentID: a.ID}
	reader := model.Principal{AgentID: b.ID}
	v, err := r.Collaborate(owner, "context.publish", model.CollaborationRequest{Title: "Delete me", Content: "shared content"})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.SharedContext)
	q := model.CollaborationRequest{ContextID: c.ID}
	if _, err = r.Collaborate(owner, "context.grant", model.CollaborationRequest{ContextID: c.ID, AgentID: b.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Collaborate(reader, "context.delete", q); err == nil {
		t.Fatal("reader deleted context")
	} else {
		requireStatus(t, err, 403)
	}
	if _, err = r.Collaborate(owner, "context.delete", q); err != nil {
		t.Fatal(err)
	}
	for _, p := range []model.Principal{owner, reader, admin} {
		_, err = r.Collaborate(p, "context.get", model.CollaborationRequest{ContextID: c.ID, Version: 1})
		requireStatus(t, err, 403)
		v, err = r.Collaborate(p, "context.search", model.CollaborationRequest{})
		if err != nil || len(v.([]model.SharedContext)) != 0 {
			t.Fatalf("deleted context visible: %v %v", v, err)
		}
	}
	_, err = r.Collaborate(owner, "context.publish", model.CollaborationRequest{ContextID: c.ID, Version: 2, Content: "new version"})
	requireStatus(t, err, 403)
	v, err = r.Collaborate(owner, "context.audit", q)
	if err != nil || len(v.([]map[string]any)) == 0 {
		t.Fatalf("audit unavailable: %v %v", v, err)
	}
	var versions, artifacts int
	if err = r.db.QueryRow("SELECT count(*) FROM context_versions WHERE context_id=?", c.ID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err = r.db.QueryRow("SELECT count(*) FROM artifacts WHERE digest=?", c.SHA256).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || artifacts != 1 {
		t.Fatalf("immutable history removed: versions=%d artifacts=%d", versions, artifacts)
	}
}
