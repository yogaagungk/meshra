package relay

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"meshra/internal/model"
)

// Restricted OMP runs the native omp binary inside the same bubblewrap worker as
// restricted Claude. OMP's own builtins are removed (--no-tools) and the only
// model-facing tools are the admitted broker catalog, registered over the
// stdio RPC the daemon already owns, so no tool socket is mounted. The daemon
// verifies the exact roster before any prompt (see omp.setHostTools).
//
// Operator prerequisites (runtime.configure):
//   - executables.omp            the omp binary (reviewed version)
//   - executables.node + executables.omp-confined (bridges/confined-exec/confined-exec.mjs)
//   - runtimePaths covering the omp binary directory, bridges/confined-exec and /etc/ssl/certs
//   - the provider credential in the daemon environment (one of ompProviders)
//   - a reviewed network grant for the provider origin on the profile
type ompProvider struct{ origin, env string }

// ompProviders is deliberately a reviewed allowlist; an unlisted provider is
// unavailable rather than guessed.
var ompProviders = map[string]ompProvider{
	"anthropic": {"https://api.anthropic.com", "ANTHROPIC_API_KEY"},
	"openai":    {"https://api.openai.com", "OPENAI_API_KEY"},
}

const ompSafetyOverlay = "disabledProviders: [native, claude, claude-plugins, codex, gemini, github, opencode, cursor, windsurf, agents-md]\nmcp:\n  enableProjectConfig: false\n"

func ompProviderNames() string {
	names := make([]string, 0, len(ompProviders))
	for n := range ompProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// confinedOMPReadyLocked reports why restricted OMP cannot run, or nil.
func (r *Relay) confinedOMPReadyLocked() error {
	config, _, err := r.runtimeConfigLocked()
	if err != nil {
		return model.Err(503, "restricted OMP is unavailable: runtime settings unreadable")
	}
	fail := func(reason string) error {
		return model.Err(503, "restricted execution is unavailable for omp: "+reason+" (no host fallback)")
	}
	if config.Executables["bwrap"] == "" {
		return fail("configure the confined runtime (executables.bwrap)")
	}
	omp, node, launcher := config.Executables["omp"], config.Executables["node"], config.Executables["omp-confined"]
	if omp == "" || node == "" || launcher == "" {
		return fail("runtime.configure needs executables.omp, executables.node and executables.omp-confined (bridges/confined-exec/confined-exec.mjs)")
	}
	if !covered(config.RuntimePaths, filepath.Dir(omp)) || !covered(config.RuntimePaths, filepath.Dir(launcher)) {
		return fail("runtimePaths must cover the omp binary directory and bridges/confined-exec")
	}
	if !covered(config.RuntimePaths, "/etc/ssl/certs") {
		return fail("runtimePaths must include /etc/ssl/certs for provider TLS")
	}
	have := false
	for _, p := range ompProviders {
		if os.Getenv(p.env) != "" {
			have = true
		}
	}
	if !have {
		return fail("set a provider credential in the daemon environment (" + ompProviderNames() + "); host login stores are never mounted")
	}
	return r.confinementProbeLocked(config)
}

type confinedOMP struct {
	relay       *Relay
	profileID   string
	workspaceID string
	agentID     string
}

// Launch exists to satisfy model.ConfinedLauncher; OMP always needs its own
// arguments, so the plain form is refused.
func (c *confinedOMP) Launch(context.Context, string) (*model.ConfinedProcess, error) {
	return nil, model.Err(503, "restricted OMP launches only through LaunchNative")
}

func (c *confinedOMP) LaunchNative(ctx context.Context, provider string, args []string) (*model.ConfinedProcess, error) {
	r := c.relay
	r.mu.Lock()
	ready := r.confinedOMPReadyLocked()
	config, _, cfgErr := r.runtimeConfigLocked()
	r.mu.Unlock()
	if ready != nil {
		return nil, ready
	}
	if cfgErr != nil {
		return nil, cfgErr
	}
	prov, ok := ompProviders[provider]
	if !ok {
		return nil, model.Err(503, "restricted execution is unavailable for omp provider \""+provider+"\": only reviewed providers ("+ompProviderNames()+") can run confined (no host fallback)")
	}
	key := os.Getenv(prov.env)
	if key == "" {
		return nil, model.Err(503, "restricted execution is unavailable for omp: "+prov.env+" is not set in the daemon environment (no host fallback)")
	}
	exe, node, launcher := config.Executables["omp"], config.Executables["node"], config.Executables["omp-confined"]
	argv := append([]string{node, launcher, "--exec", exe, "--"}, args...)
	return r.launchConfinedWorker(ctx, config, confinedSpec{
		profileID: c.profileID, workspaceID: c.workspaceID, agentID: c.agentID, stateName: "omp-state",
		origins: []string{prov.origin}, argv: argv, env: map[string]string{prov.env: key},
		prepareState: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "safety.yml"), []byte(ompSafetyOverlay), 0600)
		},
	})
}
