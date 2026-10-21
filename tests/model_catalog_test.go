package tests

import (
	"meshra/internal/adapters/claude"
	"meshra/internal/adapters/codex"
	"meshra/internal/adapters/omp"
	"meshra/internal/adapters/opencode"
	"meshra/internal/model"
	"meshra/internal/relay"
	"os"
	"testing"
)

// Catalog discovery starts no provider turn. Opt in because installed native
// tooling and its existing authentication are environment-specific.
func TestNativeModelCatalog(t *testing.T) {
	if os.Getenv("MESHRA_NATIVE_MODEL_CATALOG") != "1" {
		t.Skip("native model catalog opt-in")
	}
	r, err := relay.Open(t.TempDir(), map[string]model.Adapter{"codex": codex.New(), "claude": claude.New(), "omp": omp.New(), "opencode": opencode.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, h := range []string{"codex", "omp", "claude", "opencode"} {
		t.Run(h, func(t *testing.T) {
			v, err := r.Console(model.Principal{Admin: true}, "models", model.ConsoleRequest{Harness: h})
			if err != nil {
				t.Log("unavailable:", err)
				return
			}
			items := v.(map[string]any)["items"].([]model.ModelOption)
			t.Logf("discovered %d models", len(items))
			if len(items) == 0 {
				t.Error("native catalog returned no models")
			}
		})
	}
}

// Native account telemetry only; this never starts a generation or consumes resets.
func TestNativeAIStatus(t *testing.T) {
	if os.Getenv("MESHRA_NATIVE_AI_STATUS") != "1" {
		t.Skip("native AI status opt-in")
	}
	r, err := relay.Open(t.TempDir(), map[string]model.Adapter{"codex": codex.New(), "omp": omp.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, h := range []string{"codex", "omp"} {
		t.Run(h, func(t *testing.T) {
			value, err := r.Console(model.Principal{Admin: true}, "quota", model.ConsoleRequest{Harness: h})
			if err != nil {
				t.Fatal(err)
			}
			s := value.(model.QuotaSnapshot)
			t.Logf("state=%s windows=%d stale=%t", s.State, len(s.Windows), s.Stale)
			if s.State != "reported" {
				t.Error("native account did not report quota")
			}
		})
	}
}
