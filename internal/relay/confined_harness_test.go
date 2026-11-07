package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"meshra/internal/sandbox"
)

// This is the evidence test for restricted Claude containment. It runs the real
// production launch path (sandbox.Prepare, private state, egress proxy, scoped
// broker socket) with the containment probe as the worker command. It needs a
// delegated cgroup v2 and bubblewrap; otherwise it skips, and restricted Claude
// stays unavailable on that host.
func TestRealConfinedClaudeWorkerContainmentProbe(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap is not installed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	node, _ = filepath.EvalSymlinks(node)
	_, source, _, _ := runtime.Caller(0)
	bridges := filepath.Join(filepath.Dir(source), "../../bridges")
	sdk, _ := filepath.Abs(filepath.Join(bridges, "claude-agent-sdk"))
	mcp, _ := filepath.Abs(filepath.Join(bridges, "mcp-stdio"))
	config, err := sandbox.Configure(sandbox.Config{
		Executables:  map[string]string{"bwrap": bwrap, "node": node, "claude-confined": filepath.Join(sdk, "confined.mjs")},
		RuntimePaths: []string{sdk, mcp, "/etc/ssl/certs"},
	})
	if err != nil {
		t.Skipf("runtime manifest cannot be built on this host: %v", err)
	}
	if status := sandbox.Check(context.Background(), config); status.State != "ready" {
		t.Skipf("real confinement prerequisites unavailable (%s): %v", status.State, status.Prerequisites)
	}
	r, _ := openTest(t, nil)
	setRuntimeForTest(t, r, config)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fixture-credential-not-real")
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	home, _ := os.UserHomeDir()
	stateDir := filepath.Dir(r.lock.Name())
	launcher := &confinedClaude{relay: r, profileID: profile.ID, workspaceID: profile.WorkspaceID, agentID: "probe-agent",
		argv: func(node, _, _ string) []string {
			return []string{node, filepath.Join(sdk, "confined-probe.mjs"), "--deny", filepath.Join(home, ".claude"), "--deny", stateDir, "--deny", r.lock.Name()}
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	process, err := launcher.Launch(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	stdout, err := process.Cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	_ = process.Wait()
	var report struct {
		OK     bool            `json:"ok"`
		Checks map[string]bool `json:"checks"`
	}
	if err = json.Unmarshal([]byte(line), &report); err != nil {
		t.Fatalf("probe produced no report: %q", line)
	}
	for name, ok := range report.Checks {
		if !ok {
			t.Errorf("containment check failed: %s", name)
		}
	}
	if !report.OK {
		t.Fatalf("probe reported failure: %s", line)
	}
}

// Evidence test for restricted OMP containment. The production launcher
// (launchConfinedWorker through confined-exec.mjs) runs the same containment
// probe the Claude test uses, with node as the confined executable. It needs a
// delegated cgroup v2; otherwise it skips and restricted OMP stays unavailable.
func TestRealConfinedOMPWorkerContainmentProbe(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap is not installed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	node, _ = filepath.EvalSymlinks(node)
	_, source, _, _ := runtime.Caller(0)
	bridges := filepath.Join(filepath.Dir(source), "../../bridges")
	exe, _ := filepath.Abs(filepath.Join(bridges, "confined-exec"))
	sdk, _ := filepath.Abs(filepath.Join(bridges, "claude-agent-sdk"))
	config, err := sandbox.Configure(sandbox.Config{
		// node doubles as the "omp" executable: the probe, not a model, runs.
		Executables:  map[string]string{"bwrap": bwrap, "node": node, "omp": node, "omp-confined": filepath.Join(exe, "confined-exec.mjs")},
		RuntimePaths: []string{exe, sdk, "/etc/ssl/certs"},
	})
	if err != nil {
		t.Skipf("runtime manifest cannot be built on this host: %v", err)
	}
	if status := sandbox.Check(context.Background(), config); status.State != "ready" {
		t.Skipf("real confinement prerequisites unavailable (%s): %v", status.State, status.Prerequisites)
	}
	r, _ := openTest(t, nil)
	setRuntimeForTest(t, r, config)
	t.Setenv("ANTHROPIC_API_KEY", "fixture-credential-not-real")
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	home, _ := os.UserHomeDir()
	stateDir := filepath.Dir(r.lock.Name())
	launcher := &confinedOMP{relay: r, profileID: profile.ID, workspaceID: profile.WorkspaceID, agentID: "probe-omp"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	process, err := launcher.LaunchNative(ctx, "anthropic", []string{filepath.Join(sdk, "confined-probe.mjs"),
		"--deny", filepath.Join(home, ".omp"), "--deny", stateDir, "--deny", r.lock.Name()})
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	stdout, err := process.Cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	_ = process.Wait()
	var report struct {
		OK     bool            `json:"ok"`
		Checks map[string]bool `json:"checks"`
	}
	if err = json.Unmarshal([]byte(line), &report); err != nil {
		t.Fatalf("probe produced no report: %q", line)
	}
	for name, ok := range report.Checks {
		if !ok {
			t.Errorf("containment check failed: %s", name)
		}
	}
	if !report.OK {
		t.Fatalf("probe reported failure: %s", line)
	}
}
