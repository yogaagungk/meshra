package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"meshra/internal/model"
	"meshra/internal/netpolicy"
	"meshra/internal/sandbox"
)

// Restricted native Claude runs the reviewed SDK bridge inside the same
// bubblewrap worker used for MCP and browser processes: private root, private
// HOME, no network namespace access, no daemon database/token/vault, the scoped
// broker socket and a provider-only egress proxy. If any prerequisite is missing
// the profile is unavailable; there is no host fallback.
//
// Operator prerequisites (runtime.configure):
//   - executables.node          Node.js used by the bridge
//   - executables.claude-confined  bridges/claude-agent-sdk/confined.mjs
//   - runtimePaths covering bridges/claude-agent-sdk (with node_modules),
//     bridges/mcp-stdio and /etc/ssl/certs
//   - ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN in the daemon environment
//   - a reviewed network grant for the provider origin on the profile
const claudeProviderOrigin = "https://api.anthropic.com"

var confinementProbe struct {
	sync.Mutex
	key string
	at  time.Time
	err error
}

func claudeCredentialEnv() (string, string) {
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if v := os.Getenv(name); v != "" {
			return name, v
		}
	}
	return "", ""
}

func covered(paths []string, p string) bool {
	for _, base := range paths {
		if p == base || strings.HasPrefix(p, base+"/") {
			return true
		}
	}
	return false
}

// confinedClaudeReadyLocked reports why restricted Claude cannot run, or nil.
// The OS probe runs the real namespace/cgroup path (no model call) and is
// cached briefly per manifest so admission stays cheap.
func (r *Relay) confinedClaudeReadyLocked() error {
	config, _, err := r.runtimeConfigLocked()
	if err != nil {
		return model.Err(503, "restricted Claude is unavailable: runtime settings unreadable")
	}
	fail := func(reason string) error {
		return model.Err(503, "restricted execution is unavailable for claude: "+reason+" (no host fallback)")
	}
	if config.Executables["bwrap"] == "" {
		return fail("configure the confined runtime (executables.bwrap)")
	}
	if config.Executables["node"] == "" || config.Executables["claude-confined"] == "" {
		return fail("runtime.configure needs executables.node and executables.claude-confined (bridges/claude-agent-sdk/confined.mjs)")
	}
	launcher := config.Executables["claude-confined"]
	bridgeDir := filepath.Dir(launcher)
	if !covered(config.RuntimePaths, bridgeDir) || !covered(config.RuntimePaths, filepath.Join(filepath.Dir(bridgeDir), "mcp-stdio")) {
		return fail("runtimePaths must cover bridges/claude-agent-sdk (with node_modules) and bridges/mcp-stdio")
	}
	if !covered(config.RuntimePaths, "/etc/ssl/certs") {
		return fail("runtimePaths must include /etc/ssl/certs for provider TLS")
	}
	if name, _ := claudeCredentialEnv(); name == "" {
		return fail("set CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY in the daemon environment; host login files are never mounted")
	}
	return r.confinementProbeLocked(config)
}

// confinedClaude launches one profile's bridge. It implements model.ConfinedLauncher.
type confinedClaude struct {
	relay       *Relay
	profileID   string
	workspaceID string
	agentID     string
	// argv overrides the worker command; tests use it to run the containment probe.
	argv func(node, launcher, bridge string) []string
}

func (r *Relay) confinedLauncher(harness string, p model.Profile, agentID string) model.ConfinedLauncher {
	if harness == "omp" {
		return &confinedOMP{relay: r, profileID: p.ID, workspaceID: p.WorkspaceID, agentID: agentID}
	}
	if harness != "claude" {
		return nil
	}
	return &confinedClaude{relay: r, profileID: p.ID, workspaceID: p.WorkspaceID, agentID: agentID}
}

func (c *confinedClaude) Launch(ctx context.Context, toolSocket string) (*model.ConfinedProcess, error) {
	r := c.relay
	r.mu.Lock()
	ready := r.confinedClaudeReadyLocked()
	config, _, cfgErr := r.runtimeConfigLocked()
	r.mu.Unlock()
	if ready != nil {
		return nil, ready
	}
	if cfgErr != nil {
		return nil, cfgErr
	}
	credName, credValue := claudeCredentialEnv()
	node, launcher := config.Executables["node"], config.Executables["claude-confined"]
	bridge := filepath.Join(filepath.Dir(launcher), "bridge.mjs")
	argv := []string{node, launcher, "--bridge", bridge}
	if c.argv != nil {
		argv = c.argv(node, launcher, bridge)
	}
	return r.launchConfinedWorker(ctx, config, confinedSpec{
		profileID: c.profileID, workspaceID: c.workspaceID, agentID: c.agentID, stateName: "state",
		origins: []string{claudeProviderOrigin}, toolSocket: toolSocket, argv: argv,
		env: map[string]string{credName: credValue},
	})
}

// confinedSpec is everything harness-specific about a confined worker. The
// containment itself (workspace snapshot, private HOME, sealed cgroup launch,
// provider-only egress proxy) is shared so Claude and OMP cannot drift apart.
type confinedSpec struct {
	profileID, workspaceID, agentID string
	stateName                       string
	origins                         []string
	toolSocket                      string
	argv                            []string
	env                             map[string]string
	// prepareState runs against the private HOME directory before launch.
	prepareState func(dir string) error
}

func (r *Relay) launchConfinedWorker(ctx context.Context, config sandbox.Config, spec confinedSpec) (*model.ConfinedProcess, error) {
	workspace, err := r.prepareWorkspace(ctx, spec.workspaceID)
	if err != nil {
		return nil, err
	}
	closers := []func() error{workspace.Close}
	cleanup := func() error {
		var first error
		for i := len(closers) - 1; i >= 0; i-- {
			if e := closers[i](); e != nil && first == nil {
				first = e
			}
		}
		return first
	}
	fail := func(e error) (*model.ConfinedProcess, error) { _ = cleanup(); return nil, e }
	base := filepath.Join(filepath.Dir(r.lock.Name()), "confined")
	if err = os.MkdirAll(base, 0700); err != nil {
		return fail(err)
	}
	state, err := sandbox.OpenPrivateState(filepath.Join(base, spec.stateName), spec.agentID)
	if err != nil {
		return fail(sandboxError(err))
	}
	if spec.prepareState != nil {
		if err = spec.prepareState(filepath.Join(base, spec.stateName, "state-"+spec.agentID)); err != nil {
			return fail(err)
		}
	}
	runDir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return fail(err)
	}
	closers = append(closers, func() error { return os.RemoveAll(runDir) })
	proxy, err := r.networkProxy(ctx, filepath.Join(runDir, "egress.sock"), netpolicy.Scope{ProfileID: spec.profileID}, netpolicy.ProxyOptions{
		AllowedOrigins: spec.origins,
		MaxStreams:     16, MaxStreamBytes: 256 << 20, MaxStreamDuration: 30 * time.Minute,
	})
	if err != nil {
		return fail(model.Err(503, "the provider egress proxy is unavailable: "+err.Error()))
	}
	closers = append(closers, func() error { proxy.Close(); return nil })
	mounts := []sandbox.SocketMount{{Source: proxy.Path(), Target: "/run/meshra/egress.sock", Purpose: "egress"}}
	if spec.toolSocket != "" {
		mounts = append(mounts, sandbox.SocketMount{Source: spec.toolSocket, Target: "/run/meshra/broker.sock", Purpose: "broker"})
	}
	prepared, err := sandbox.Prepare(ctx, config, sandbox.Request{
		Workspace: workspace, Kind: sandbox.ACP, ReadOnly: true, State: state, SocketMounts: mounts,
		Argv: spec.argv, Env: spec.env,
	})
	if err != nil {
		return fail(sandboxError(err))
	}
	closers = append(closers, prepared.Close)
	prepared.Cmd.Stderr = nil
	out := &model.ConfinedProcess{Cmd: prepared.Cmd, Start: prepared.Start, Wait: prepared.Wait, Close: cleanup, Cwd: "/workspace"}
	if spec.toolSocket != "" {
		out.ToolSocket = "/run/meshra/broker.sock"
	}
	return out, nil
}

// confinementProbeLocked runs the real namespace/cgroup readiness check (no
// model call), cached briefly per manifest so admission stays cheap.
func (r *Relay) confinementProbeLocked(config sandbox.Config) error {
	fail := func(reason string) error {
		return model.Err(503, "restricted execution is unavailable: "+reason+" (no host fallback)")
	}
	encoded, _ := json.Marshal(config)
	sum := sha256.Sum256(encoded)
	key := hex.EncodeToString(sum[:])
	confinementProbe.Lock()
	defer confinementProbe.Unlock()
	if confinementProbe.key != key || time.Since(confinementProbe.at) > 5*time.Minute {
		status := sandbox.Check(context.Background(), config)
		var probeErr error
		if status.State != "ready" {
			probeErr = fmt.Errorf("confined runtime %s: %s", status.State, strings.Join(status.Prerequisites, "; "))
		}
		confinementProbe.key, confinementProbe.at, confinementProbe.err = key, time.Now(), probeErr
	}
	if confinementProbe.err != nil {
		return fail(confinementProbe.err.Error())
	}
	return nil
}