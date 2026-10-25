package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"meshra/internal/adapters/process"
)

// ProcessLauncher starts the agent with the repository's bounded JSONL process
// transport. It keeps the identity-owned native state directory and passes
// only explicitly selected environment values to the child.
//
// The launcher does not confine the child process. A profile-managed agent
// needs a launcher that confines it: the agent must run inside the read-only
// sandbox with the attempt-scoped broker socket mounted and with no route
// except the operator-selected origins in Config.AllowedOrigins.
type ProcessLauncher struct {
	// Identity owns the native state directory and its exclusive lock.
	Identity string
	// Ephemeral uses disposable native state instead of a durable one.
	Ephemeral bool
	// Environment replaces the complete environment of the agent. A nil
	// value selects the minimal environment built from the configuration.
	Environment []string
	// Secrets holds the resolved secret values that the launcher passes to
	// the agent. The values are never part of a stored configuration.
	Secrets map[string]string
	// WorkDir is the working directory of the agent. An empty value uses the
	// configuration working directory.
	WorkDir string
}

// Launch starts the reviewed ACP CLI and returns its bounded transport.
func (l ProcessLauncher) Launch(ctx context.Context, config Config) (Transport, error) {
	if l.Identity == "" {
		return nil, errors.New("the ACP launcher requires a canonical identity")
	}
	dir, unlock, err := process.RuntimeStateDir("acp", l.Identity, l.Ephemeral)
	if err != nil {
		return nil, err
	}
	environment := l.Environment
	if environment == nil {
		environment, err = agentEnvironment(dir, config)
		if err != nil {
			unlock()
			return nil, err
		}
	}
	if len(l.Secrets) > 0 {
		names := make([]string, 0, len(l.Secrets))
		for name := range l.Secrets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			environment = append(environment, name+"="+l.Secrets[name])
		}
	}
	work := l.WorkDir
	if work == "" {
		work = config.CWD
	}
	if work == "" {
		unlock()
		return nil, errors.New("the ACP launcher requires a working directory")
	}
	if !filepath.IsAbs(work) {
		unlock()
		return nil, errors.New("the ACP working directory must be an absolute path")
	}
	child, err := process.Start(ctx, config.Executable, config.Args, work, environment)
	if err != nil {
		unlock()
		return nil, err
	}
	return &processTransport{child: child, unlock: unlock}, nil
}

// agentEnvironment builds the minimal environment of the agent. Only the
// reviewed values are passed; the daemon environment, its credentials and its
// provider secrets are never inherited.
func agentEnvironment(dir string, config Config) ([]string, error) {
	temporary := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(temporary, 0700); err != nil {
		return nil, err
	}
	path := "/usr/local/bin:/usr/bin:/bin"
	if len(config.RuntimePaths) > 0 {
		reviewed := make([]string, 0, len(config.RuntimePaths))
		for _, entry := range config.RuntimePaths {
			if entry == "" || !filepath.IsAbs(entry) || strings.ContainsRune(entry, 0) {
				return nil, errors.New("the reviewed runtime path must be an absolute path")
			}
			reviewed = append(reviewed, entry)
		}
		path = strings.Join(reviewed, ":") + ":" + path
	}
	environment := []string{"HOME=" + dir, "PATH=" + path, "TMPDIR=" + temporary, "LANG=C.UTF-8"}
	names := make([]string, 0, len(config.Env))
	for name := range config.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		environment = append(environment, name+"="+config.Env[name])
	}
	return environment, nil
}

// processTransport adapts the repository's process transport to the ACP
// framing and releases the native state lock exactly once.
type processTransport struct {
	child  *process.Process
	unlock func()
	once   sync.Once
}

func (t *processTransport) Read(ctx context.Context) (json.RawMessage, error) {
	return t.child.Read(ctx)
}

func (t *processTransport) Send(ctx context.Context, value any) error {
	return t.child.Send(ctx, value)
}

func (t *processTransport) Close() error {
	t.once.Do(func() {
		_ = t.child.Close()
		t.unlock()
	})
	return nil
}
