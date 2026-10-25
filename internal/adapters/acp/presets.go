package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Preset describes one reviewed installed-CLI preset for an ACP agent. The
// preset never downloads a proprietary CLI and never changes user
// configuration: it only names the executable candidates, the fixed
// arguments and the fixed environment of an already installed agent.
type Preset struct {
	// Name identifies the preset.
	Name string
	// Executables lists the accepted executable names in preference order.
	Executables []string
	// Args are the fixed arguments of the preset.
	Args []string
	// Env holds the fixed environment values of the preset.
	Env map[string]string
}

// BuiltinPresets returns the reviewed installed-CLI presets.
func BuiltinPresets() []Preset {
	return []Preset{
		{Name: "gemini", Executables: []string{"gemini"}, Args: []string{"--acp"}},
		{Name: "cursor", Executables: []string{"cursor-agent", "agent"}, Args: []string{"acp"}},
		{Name: "cline", Executables: []string{"cline"}, Args: []string{"--acp"}, Env: map[string]string{"CLINE_SESSION_BACKEND_MODE": "local", "CLINE_NO_AUTO_UPDATE": "1"}},
	}
}

// PresetByName returns one reviewed preset.
func PresetByName(name string) (Preset, bool) {
	for _, preset := range BuiltinPresets() {
		if preset.Name == name {
			return preset, true
		}
	}
	return Preset{}, false
}

// ResolvePreset returns one reviewed preset or an actionable error.
func ResolvePreset(name string) (Preset, error) {
	if preset, ok := PresetByName(name); ok {
		return preset, nil
	}
	return Preset{}, fmt.Errorf("unknown ACP preset %q; the reviewed presets are %s", name, PresetNamesText())
}

// Locate returns the absolute path of the first installed candidate. Only
// regular executable files are accepted, and only inside the given
// directories.
func (p Preset) Locate(directories []string) (string, error) {
	if len(p.Executables) == 0 {
		return "", fmt.Errorf("the %q preset declares no executable", p.Name)
	}
	for _, directory := range directories {
		if directory == "" || !filepath.IsAbs(directory) {
			return "", fmt.Errorf("the %q preset requires an absolute search directory", p.Name)
		}
		for _, name := range p.Executables {
			candidate := filepath.Join(directory, name)
			canonical, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				continue
			}
			info, err := os.Stat(canonical)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				continue
			}
			return canonical, nil
		}
	}
	return "", fmt.Errorf("the %q preset is not installed in the reviewed directories", p.Name)
}

// Apply merges the fixed preset values into a configuration. The preset never
// overwrites an explicitly reviewed value.
func (p Preset) Apply(config Config) Config {
	if len(config.Args) == 0 {
		config.Args = append([]string(nil), p.Args...)
	}
	if len(config.Env) == 0 {
		config.Env = map[string]string{}
		for name, value := range p.Env {
			config.Env[name] = value
		}
	}
	return config
}

// PresetNames lists the reviewed preset names.
func PresetNames() []string {
	names := make([]string, 0, 3)
	for _, preset := range BuiltinPresets() {
		names = append(names, preset.Name)
	}
	return names
}

// PresetNamesText lists the reviewed preset names for an error message.
func PresetNamesText() string {
	return strings.Join(PresetNames(), ", ")
}
