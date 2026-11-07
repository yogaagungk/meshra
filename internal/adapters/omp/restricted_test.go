package omp

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

// fakeNative stands in for the confined worker: it runs the protocol helper as
// a child process and records the exact command line the adapter chose.
type fakeNative struct {
	args     []string
	provider string
	launches int
}

func (f *fakeNative) Launch(context.Context, string) (*model.ConfinedProcess, error) {
	return nil, model.Err(503, "plain launch is not used")
}

func (f *fakeNative) LaunchNative(_ context.Context, provider string, args []string) (*model.ConfinedProcess, error) {
	f.launches++
	f.provider, f.args = provider, append([]string(nil), args...)
	cmd := exec.Command("omp", args...)
	return &model.ConfinedProcess{
		Cmd: cmd, Start: cmd.Start, Wait: cmd.Wait, Close: func() error { return nil },
		Cwd: "/workspace",
	}, nil
}

func restrictedAgent(t *testing.T, l model.ConfinedLauncher) model.Agent {
	a := profileAgent(t)
	a.Restricted, a.Confined = true, l
	a.Config.Provider = "anthropic"
	return a
}

func TestRestrictedOMPNeverStartsOnTheHost(t *testing.T) {
	mockOMP(t, "profile-success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New().Factory(ctx, restrictedAgent(t, nil), nil)
	var e *model.Error
	if err == nil || !errors.As(err, &e) || e.Status != 503 || !strings.Contains(err.Error(), "confined worker") {
		t.Fatalf("restricted OMP without a launcher must be refused: %v", err)
	}
	// A launcher that cannot supply native arguments is not enough either.
	plain := struct{ model.ConfinedLauncher }{}
	_, err = New().Factory(ctx, restrictedAgent(t, plain), nil)
	if err == nil || !strings.Contains(err.Error(), "native confined launcher") {
		t.Fatalf("plain launcher accepted: %v", err)
	}
	// Drafts and unmanaged runs never reach a restricted worker.
	a := restrictedAgent(t, &fakeNative{})
	a.ProfileManaged = false
	if _, err = New().Factory(ctx, a, nil); err == nil || !strings.Contains(err.Error(), "profile-managed") {
		t.Fatalf("unmanaged restricted OMP accepted: %v", err)
	}
}

func TestRestrictedOMPUsesConfinedPathsAndExactRoster(t *testing.T) {
	mockOMP(t, "profile-success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l := &fakeNative{}
	a := restrictedAgent(t, l)
	rt, err := New().Factory(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if l.launches != 1 || l.provider != "anthropic" {
		t.Fatalf("launcher use: %+v", l)
	}
	for _, want := range []string{"--no-tools", "--approval-mode=always-ask", "--no-extensions", "--config=" + RestrictedConfig, "--session-dir=" + RestrictedSessions} {
		if !slices.Contains(l.args, want) {
			t.Fatalf("missing %s in %v", want, l.args)
		}
	}
	for _, arg := range l.args {
		if strings.Contains(arg, a.Cwd) || arg == "--approval-mode=yolo" {
			t.Fatalf("host path or full-access flag leaked into confined argv: %v", l.args)
		}
	}
}

func TestRestrictedOMPRefusesNativeToolsBeyondTheCatalog(t *testing.T) {
	mockOMP(t, "profile-native") // a builtin "read" survives beside the broker tools
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New().Factory(ctx, restrictedAgent(t, &fakeNative{}), nil)
	if err == nil || !strings.Contains(err.Error(), "beyond the admitted broker catalog") {
		t.Fatalf("restricted OMP accepted a native builtin: %v", err)
	}
}
