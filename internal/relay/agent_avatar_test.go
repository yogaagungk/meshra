package relay

import (
	"testing"

	"meshra/internal/model"
)

func TestAgentAvatarPersistenceAndValidation(t *testing.T) {
	r, dir := openTest(t, nil)
	for _, icon := range []string{"spark", "orbit", "bot", "star", "hex", "flower", "bolt"} {
		a, err := createWithWorkspaceTest(t, r, admin, model.CreateAgent{Harness: "omp", Name: icon, Cwd: t.TempDir(), AvatarIcon: icon, AvatarColor: "#a1B2c3"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Agent(admin, a.ID)
		if err != nil || got.AvatarIcon != icon || got.AvatarColor != "#a1B2c3" {
			t.Fatalf("avatar not stored: %+v, %v", got, err)
		}
	}
	for _, request := range []model.CreateAgent{
		{Harness: "omp", Name: "invalidicon", Cwd: t.TempDir(), AvatarIcon: "unknown"},
		{Harness: "omp", Name: "invalidcolor", Cwd: t.TempDir(), AvatarColor: "red"},
	} {
		_, err := createWithWorkspaceTest(t, r, admin, request)
		requireStatus(t, err, 400)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Agent(admin, "omp:spark")
	if err != nil || got.AvatarIcon != "spark" || got.AvatarColor != "#a1B2c3" {
		t.Fatalf("avatar lost on restart: %+v, %v", got, err)
	}
}
