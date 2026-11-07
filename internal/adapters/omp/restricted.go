package omp

import (
	"context"
	"errors"
	"sync"
	"syscall"

	"meshra/internal/adapters/process"
	"meshra/internal/model"
)

// restrictedSafetyConfig is written into the worker's private HOME by the
// launcher (see relay.confinedOMP). The adapter only names it.
const (
	RestrictedConfig   = "/home/worker/safety.yml"
	RestrictedSessions = "/home/worker/sessions"
)

// RestrictedArgs is the exact native command line for a confined run. Native
// builtins are removed (--no-tools), approvals always ask, discovery sources are
// disabled by the overlay config, and no host path is referenced.
func RestrictedArgs(a model.Agent) []string {
	args := []string{"--mode=rpc", "--no-ui", "--no-extensions", "--no-skills", "--no-rules", "--no-lsp", "--no-pty", "--no-title",
		"--no-tools", "--approval-mode=always-ask", "--no-prewalk",
		"--config=" + RestrictedConfig, "--session-dir=" + RestrictedSessions}
	if a.SessionID != "" {
		args = append(args, "--resume="+a.SessionID)
	}
	if a.Config.Model != "" {
		args = append(args, "--model="+a.Config.Model)
	}
	if a.Config.Provider != "" {
		args = append(args, "--provider="+a.Config.Provider)
	}
	return args
}

// restrictedFactory starts OMP only inside the confined worker. There is no
// host fallback: a missing launcher, a draft or an unmanaged run is refused
// before any native process exists.
func restrictedFactory(ctx context.Context, a model.Agent, emit model.Emit) (model.Runtime, error) {
	if a.Confined == nil {
		return nil, model.Err(503, "restricted OMP execution requires the confined worker; refusing to run on the host")
	}
	if !a.ProfileManaged || a.GenerationDraft {
		return nil, model.Err(503, "restricted OMP runs only as a profile-managed task with the admitted broker catalog")
	}
	launcher, ok := a.Confined.(model.NativeLauncher)
	if !ok {
		return nil, model.Err(503, "restricted OMP execution requires a native confined launcher; refusing to run on the host")
	}
	proc, err := launcher.LaunchNative(ctx, a.Config.Provider, RestrictedArgs(a))
	if err != nil {
		return nil, err
	}
	if proc == nil || proc.Cmd == nil || proc.Start == nil || proc.Wait == nil {
		return nil, errors.New("confined launcher returned no sealed process")
	}
	var closeOnce sync.Once
	closeWorker := func() {
		if proc.Close != nil {
			closeOnce.Do(func() { _ = proc.Close() })
		}
	}
	p, err := process.StartPrepared(ctx, proc.Cmd, proc.Start, func() error {
		err := proc.Wait()
		closeWorker()
		return err
	}, func() error {
		// Prepared.Wait/Close kill the whole cgroup; also signal the direct child.
		if proc.Cmd.Process != nil {
			_ = proc.Cmd.Process.Signal(syscall.SIGKILL)
		}
		return nil
	})
	if err != nil {
		closeWorker()
		return nil, err
	}
	return handshake(ctx, a, emit, p, func() {}, RestrictedSessions)
}
