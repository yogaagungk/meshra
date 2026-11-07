package relay

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"meshra/internal/model"
	"meshra/internal/sandbox"
)

// legacyProfile rewrites a stored profile exactly as a pre-mode release left it.
func legacyProfile(t *testing.T, r *Relay, id string) model.Profile {
	t.Helper()
	if _, err := r.db.Exec(`UPDATE agent_profiles SET body=json_remove(json_remove(body,'$.executionMode'),'$.modeReviewed') WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.profile(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewProfilesAreRestrictedAndTrustedNeedsAcknowledgment(t *testing.T) {
	r, _ := openTest(t, nil)
	w := profileStoreWorkspace(t, r)
	p := profileStoreCreate(t, r, profileStoreRequest(w))
	if p.ExecutionMode != model.ModeRestricted || !p.ModeReviewed {
		t.Fatalf("new profile must default to reviewed restricted mode: %+v", p)
	}
	q := profileStoreRequest(w)
	q["executionMode"] = "trusted"
	if _, err := profileStoreCall(t, r, "profiles.save", q); err == nil || !strings.Contains(err.Error(), "acknowledgeTrusted") {
		t.Fatalf("trusted creation without acknowledgment must fail: %v", err)
	}
	q["acknowledgeTrusted"] = true
	v, err := profileStoreCall(t, r, "profiles.save", q)
	if err != nil || v.(model.Profile).ExecutionMode != model.ModeTrusted {
		t.Fatalf("acknowledged trusted creation failed: %v", err)
	}
	// An ordinary save cannot change the mode.
	up := profileStoreRequest(w)
	up["id"], up["expectedRevision"], up["executionMode"] = p.ID, p.Revision, "trusted"
	if _, err = profileStoreCall(t, r, "profiles.save", up); err == nil {
		t.Fatal("profiles.save must not change execution mode")
	}
}

func TestLegacyProfileIsTruthfullyTrustedAndBlockedFromUnattendedWork(t *testing.T) {
	r, _ := openTest(t, nil)
	p := routineProfile(t, r, "legacy")
	legacy := legacyProfile(t, r, p.ID)
	if legacy.ExecutionMode != model.ModeTrusted || legacy.ModeReviewed || legacy.Restricted() {
		t.Fatalf("legacy profile must be unreviewed trusted: %+v", legacy)
	}
	r.mu.Lock()
	err := r.enforceModeAdmission(model.Principal{}, true, model.Agent{}, model.ProfileTaskSnapshot{Profile: legacy, ExecutionMode: legacy.EffectiveMode(), External: true})
	enableErr := r.requireRestrictedProfiles([]string{p.ID})
	r.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "blocked") || enableErr == nil {
		t.Fatalf("unattended admission and enabling must be blocked: %v / %v", err, enableErr)
	}
	// Operator-driven (non external) work remains possible.
	r.mu.Lock()
	err = r.enforceModeAdmission(admin, false, model.Agent{}, model.ProfileTaskSnapshot{Profile: legacy, ExecutionMode: legacy.EffectiveMode()})
	r.mu.Unlock()
	if err != nil {
		t.Fatalf("operator chat with a trusted profile must still work: %v", err)
	}
}

func TestStartupReviewDisablesUnsafeRoutinesKeepingHistory(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	p := routineProfile(t, r, "reviewed")
	view := routineSave(t, r, map[string]any{"name": "Nightly", "profileId": p.ID, "instruction": "Summarize", "trigger": map[string]any{"kind": "manual"}, "enabled": true})
	if !view.Enabled {
		t.Fatal("restricted routine should enable")
	}
	legacyProfile(t, r, p.ID)
	if err := r.reviewUnsafeAutomation(); err != nil {
		t.Fatal(err)
	}
	d, _, err := r.routineDefinition(view.ID)
	if err != nil || d.Enabled || d.BlockedReason == "" || d.Revision != view.Revision+1 {
		t.Fatalf("routine must be disabled with an actionable reason and a new revision: %+v %v", d, err)
	}
	if err = r.reviewUnsafeAutomation(); err != nil { // idempotent
		t.Fatal(err)
	}
	_, err = routineCall(t, r, "routines.setEnabled", map[string]any{"id": view.ID, "expectedRevision": d.Revision, "enabled": true})
	if err == nil {
		t.Fatal("enabling a routine targeting a trusted profile must be rejected")
	}
}

func TestSetModeRequiresAcknowledgmentQuiescenceAndProtectsDelegation(t *testing.T) {
	r, _ := openTest(t, nil)
	w := profileStoreWorkspace(t, r)
	a := profileStoreCreate(t, r, profileStoreRequest(w))
	q := profileStoreRequest(w)
	q["name"] = "second"
	b := profileStoreCreate(t, r, q)
	// Restricted a may delegate to restricted b.
	up := profileStoreRequest(w)
	up["id"], up["expectedRevision"], up["allowedProfileIds"] = a.ID, a.Revision, []string{b.ID}
	v, err := profileStoreCall(t, r, "profiles.save", up)
	if err != nil {
		t.Fatal(err)
	}
	a = v.(model.Profile)
	if _, err = profileStoreCall(t, r, "profiles.setMode", map[string]any{"id": b.ID, "expectedRevision": b.Revision, "mode": "trusted"}); err == nil {
		t.Fatal("trusted requires acknowledgment")
	}
	if _, err = profileStoreCall(t, r, "profiles.setMode", map[string]any{"id": b.ID, "expectedRevision": b.Revision, "mode": "trusted", "acknowledgeTrusted": true}); err == nil || !strings.Contains(err.Error(), "delegate") {
		t.Fatalf("a restricted profile delegating to b must prevent b becoming trusted: %v", err)
	}
	// Remove the grant, then the switch succeeds and bumps the revision.
	up["expectedRevision"], up["allowedProfileIds"] = a.Revision, []string{}
	if _, err = profileStoreCall(t, r, "profiles.save", up); err != nil {
		t.Fatal(err)
	}
	v, err = profileStoreCall(t, r, "profiles.setMode", map[string]any{"id": b.ID, "expectedRevision": b.Revision, "mode": "trusted", "acknowledgeTrusted": true})
	if err != nil || v.(model.Profile).ExecutionMode != model.ModeTrusted || v.(model.Profile).Revision != b.Revision+1 {
		t.Fatalf("mode change failed: %v", err)
	}
	// A restricted profile can no longer be granted delegation to the trusted one.
	up["expectedRevision"], up["allowedProfileIds"] = a.Revision+1, []string{b.ID}
	if _, err = profileStoreCall(t, r, "profiles.save", up); err == nil {
		t.Fatal("restricted->trusted delegation grant must be rejected")
	}
}

func TestRestrictedNativeIsUnavailableWithoutProvenConfinement(t *testing.T) {
	r, _ := openTest(t, nil)
	r.mu.Lock()
	for h, ad := range r.adapters {
		delete(ad.Capabilities, "restrictedConfinement")
		r.adapters[h] = ad
	}
	r.mu.Unlock()
	w := profileStoreWorkspace(t, r)
	if _, err := profileStoreCall(t, r, "profiles.save", profileStoreRequest(w)); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("restricted omp must fail closed without proof: %v", err)
	}
	q := profileStoreRequest(w)
	q["binding"] = model.ProfileProvider{Harness: "omp"}
	q["executionMode"], q["acknowledgeTrusted"] = "trusted", true
	if _, err := profileStoreCall(t, r, "profiles.save", q); err != nil {
		t.Fatalf("acknowledged trusted mode remains available: %v", err)
	}
}

func setRuntimeForTest(t *testing.T, r *Relay, c sandbox.Config) {
	t.Helper()
	body, _ := json.Marshal(c)
	if _, err := r.db.Exec("INSERT INTO automation_runtime_settings(id,revision,body) VALUES(1,1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", string(body)); err != nil {
		t.Fatal(err)
	}
}

func claudeBlocked(r *Relay) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restrictedNativeBlocked("claude")
}

// Restricted Claude reports each unmet prerequisite precisely, and never
// becomes available from a flag: it needs the operator manifest, a credential
// the daemon environment supplies, and a passing OS confinement probe.
func TestRestrictedClaudeNamesEveryUnmetConfinementPrerequisite(t *testing.T) {
	r, _ := openTest(t, nil)
	r.mu.Lock()
	for h, ad := range r.adapters {
		delete(ad.Capabilities, "restrictedConfinement")
		r.adapters[h] = ad
	}
	r.mu.Unlock()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	expect := func(want string) {
		t.Helper()
		if err := claudeBlocked(r); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "no host fallback") {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	expect("executables.bwrap")
	setRuntimeForTest(t, r, sandbox.Config{Executables: map[string]string{"bwrap": "/usr/bin/bwrap"}})
	expect("executables.claude-confined")
	dir := t.TempDir()
	cfg := sandbox.Config{Executables: map[string]string{"bwrap": "/usr/bin/bwrap", "node": "/usr/bin/node", "claude-confined": filepath.Join(dir, "claude-agent-sdk/confined.mjs")}}
	setRuntimeForTest(t, r, cfg)
	expect("runtimePaths must cover")
	cfg.RuntimePaths = []string{filepath.Join(dir, "claude-agent-sdk"), filepath.Join(dir, "mcp-stdio")}
	setRuntimeForTest(t, r, cfg)
	expect("/etc/ssl/certs")
	cfg.RuntimePaths = append(cfg.RuntimePaths, "/etc/ssl/certs")
	setRuntimeForTest(t, r, cfg)
	expect("CLAUDE_CODE_OAUTH_TOKEN")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fixture-token-not-real")
	// The manifest in this test is not a real, hashed runtime, so the real OS
	// probe cannot pass: availability is refused rather than assumed.
	expect("confined runtime")
}

func ompBlocked(r *Relay) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restrictedNativeBlocked("omp")
}

// Restricted OMP is a real prerequisite chain, never a flag, and it names a
// reviewed provider list rather than guessing at one.
func TestRestrictedOMPNamesEveryUnmetConfinementPrerequisite(t *testing.T) {
	r, _ := openTest(t, nil)
	r.mu.Lock()
	for h, ad := range r.adapters {
		delete(ad.Capabilities, "restrictedConfinement")
		r.adapters[h] = ad
	}
	r.mu.Unlock()
	for _, p := range ompProviders {
		t.Setenv(p.env, "")
	}
	expect := func(want string) {
		t.Helper()
		if err := ompBlocked(r); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "no host fallback") {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	expect("executables.bwrap")
	setRuntimeForTest(t, r, sandbox.Config{Executables: map[string]string{"bwrap": "/usr/bin/bwrap"}})
	expect("executables.omp")
	dir := t.TempDir()
	cfg := sandbox.Config{Executables: map[string]string{"bwrap": "/usr/bin/bwrap", "node": "/usr/bin/node",
		"omp": filepath.Join(dir, "bin/omp"), "omp-confined": filepath.Join(dir, "confined-exec/confined-exec.mjs")}}
	setRuntimeForTest(t, r, cfg)
	expect("runtimePaths must cover")
	cfg.RuntimePaths = []string{filepath.Join(dir, "bin"), filepath.Join(dir, "confined-exec")}
	setRuntimeForTest(t, r, cfg)
	expect("/etc/ssl/certs")
	cfg.RuntimePaths = append(cfg.RuntimePaths, "/etc/ssl/certs")
	setRuntimeForTest(t, r, cfg)
	expect("provider credential")
	t.Setenv("ANTHROPIC_API_KEY", "fixture-not-real")
	expect("confined runtime")
}

// Launch re-checks readiness itself, so an unconfigured installation cannot
// start a worker even if admission was skipped, and plain Launch is refused.
func TestRestrictedOMPLaunchRefusesWhenUnconfigured(t *testing.T) {
	r, _ := openTest(t, nil)
	l := &confinedOMP{relay: r, profileID: "p", workspaceID: "w", agentID: "a"}
	if _, err := l.LaunchNative(context.Background(), "deepseek", []string{"--mode=rpc"}); err == nil || !strings.Contains(err.Error(), "no host fallback") {
		t.Fatalf("unreviewed provider must be unavailable: %v", err)
	}
	if _, err := l.Launch(context.Background(), ""); err == nil {
		t.Fatal("plain launch must be refused")
	}
}
