package relay

import (
	"context"
	"os"
	"path/filepath"

	"meshra/internal/adapters/acp"
)

type acpPresetDiscovery struct {
	Preset     string   `json:"preset"`
	Executable string   `json:"executable,omitempty"`
	Args       []string `json:"args"`
	Installed  bool     `json:"installed"`
}

// Inspect installed path candidates only. No CLI process, login, updater or
// model turn is started; readiness still requires the confined protocol check.
func scanInstalledACPPresets(ctx context.Context) ([]acpPresetDiscovery, error) {
	directories := []string{}
	seen := map[string]bool{}
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(directory) && !seen[directory] {
			directories = append(directories, directory)
			seen[directory] = true
		}
	}
	presets := acp.BuiltinPresets()
	results := make([]acpPresetDiscovery, 0, len(presets))
	for _, preset := range presets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result := acpPresetDiscovery{Preset: preset.Name, Args: preset.Args}
		path, err := preset.Locate(directories)
		if err == nil && redact(path) == path {
			result.Installed = true
			result.Executable = path
		}
		results = append(results, result)
	}
	return results, nil
}
