package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"meshra/internal/adapters/acp"
	"meshra/internal/adapters/process"
	"meshra/internal/model"
	"meshra/internal/netpolicy"
	"meshra/internal/sandbox"
)

// NewACPRuntime defers native allocation until an admitted turn owns its broker.
// ACP sessions are fresh on every turn; bounded conversation reconstruction is
// handled by the relay, not by trusting an agent's unadvertised resume support.
func (r *Relay) NewACPRuntime(ctx context.Context, agent model.Agent, emit model.Emit) (model.Runtime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := r.ACPAgent(ctx, agent.Config.Provider)
	if err != nil {
		return nil, err
	}
	if !config.Enabled {
		return nil, model.Err(409, "ACP configuration is disabled")
	}
	runtime, _, err := r.runtimeConfig()
	if err != nil {
		return nil, err
	}
	if err := acpRuntimeArtifacts(runtime, config); err != nil {
		return nil, err
	}
	status := sandbox.Check(ctx, runtime)
	if status.State != "ready" {
		return nil, model.Err(503, "ACP requires confined execution: "+strings.Join(status.Prerequisites, "; "))
	}
	return &confinedACPRuntime{relay: r, life: ctx, agent: agent, config: config}, nil
}

type confinedACPRuntime struct {
	relay   *Relay
	agent   model.Agent
	config  ACPAgentConfig
	life    context.Context
	mu      sync.Mutex
	session string
	cancel  context.CancelFunc
	closed  bool
	running bool
	done    chan struct{}
}

func (r *confinedACPRuntime) SessionID() string { r.mu.Lock(); defer r.mu.Unlock(); return r.session }
func (r *confinedACPRuntime) Close() error {
	r.mu.Lock()
	r.closed = true
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}
func (r *confinedACPRuntime) Run(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
	r.mu.Lock()
	if r.closed || r.running {
		r.mu.Unlock()
		return "", model.Err(409, "ACP runtime is closed or already running")
	}
	if err := r.life.Err(); err != nil {
		r.mu.Unlock()
		return "", err
	}
	childContext, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.running = true
	r.session = ""
	r.done = make(chan struct{})
	done := r.done
	r.mu.Unlock()
	stopLife := context.AfterFunc(r.life, cancel)
	defer stopLife()
	defer func() {
		cancel()
		r.mu.Lock()
		r.cancel = nil
		r.running = false
		close(done)
		r.done = nil
		r.mu.Unlock()
	}()
	var broker *toolBroker
	if r.agent.ProfileManaged {
		var ok bool
		broker, ok = turn.Tools.(*toolBroker)
		if !ok || broker.relay != r.relay || broker.taskID != turn.Task.ID {
			return "", model.Err(403, "ACP requires the admitted attempt broker")
		}
	}
	stored := r.config
	if broker != nil {
		if len(broker.snapshot.Connection) == 0 {
			return "", model.Err(503, "admitted ACP configuration is unavailable")
		}
		if err := json.Unmarshal(broker.snapshot.Connection, &stored); err != nil || stored.ID != r.agent.Config.Provider {
			return "", model.Err(503, "admitted ACP configuration is invalid")
		}
		if err := r.relay.validateSecretVersions(ctx, broker.snapshot.SecretVersions); err != nil {
			return "", err
		}
	}
	config, err := acpStoredConfiguration(stored)
	if err != nil {
		return "", err
	}
	var credential, socket string
	if broker != nil {
		socket, credential, err = broker.ScopedMCP(childContext)
		if err != nil {
			return "", err
		}
		runtime, _, err := r.relay.runtimeConfig()
		if err != nil {
			return "", err
		}
		config.MCPServers = []acp.MCPServer{{Name: "meshra", Command: runtime.Executables["node"], Args: []string{runtime.Executables["mcp-bridge"]}, Env: []acp.EnvVariable{{Name: "MESHRA_TOOL_SOCKET", Value: "/run/meshra/broker.sock"}, {Name: "MESHRA_TOOL_TOKEN", Value: credential}}}}
	}
	launcher := acpWorkerLauncher{relay: r.relay, agent: r.agent, stored: stored, broker: broker, brokerSocket: socket}
	adapter := acp.New(acp.Options{Resolve: func(context.Context, string) (acp.Config, error) { return config, nil }, Launcher: launcher})
	agent := r.agent
	agent.SessionID = ""
	agent.Cwd = "/workspace"
	if broker != nil {
		agent.ToolDefinitions = broker.Definitions()
	}
	nativeEmit := func(kind, data string) {
		if kind == "session" {
			r.mu.Lock()
			r.session = data
			r.mu.Unlock()
		}
		// Deferred session/new must update this task's destination session and
		// credential, not only the backing agent's idle startup record.
		if emit != nil {
			emit(kind, data)
		}
	}
	native, err := adapter.Factory(childContext, agent, nativeEmit)
	if err != nil {
		return "", err
	}
	defer native.Close()
	return native.Run(childContext, turn, emit)
}

type acpWorkerLauncher struct {
	relay           *Relay
	agent           model.Agent
	stored          ACPAgentConfig
	broker          *toolBroker
	brokerSocket    string
	active          func(context.Context) error
	disposableState bool
}

func acpRuntimeArtifacts(runtime sandbox.Config, config ACPAgentConfig) error {
	for _, role := range []string{"node", "acp-bridge", "mcp-bridge"} {
		if runtime.Executables[role] == "" {
			return model.Err(503, "configure runtime executable "+role+" for confined ACP")
		}
	}
	selected := false
	for _, path := range runtime.Executables {
		if path == config.Executable {
			selected = true
			break
		}
	}
	if !selected {
		return model.Err(503, "record the selected ACP executable in the runtime manifest")
	}
	for _, path := range config.RuntimePaths {
		covered := false
		for _, asset := range runtime.Manifest {
			if path == asset.Path || strings.HasPrefix(path, asset.Path+"/") {
				covered = true
				break
			}
		}
		if !covered {
			return model.Err(503, "record the selected ACP runtime paths in the runtime manifest")
		}
	}
	return nil
}
func (l acpWorkerLauncher) Launch(ctx context.Context, config acp.Config) (acp.Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runtime, _, err := l.relay.runtimeConfig()
	if err != nil {
		return nil, err
	}
	if err = acpRuntimeArtifacts(runtime, l.stored); err != nil {
		return nil, err
	}
	var snapshot *sandbox.Snapshot
	ownSnapshot := false
	var empty string
	if l.broker != nil && l.broker.snapshot.Profile.Capabilities.WorkspaceRead {
		snapshot, err = l.broker.ensureWorkspace(ctx)
	} else {
		parent := filepath.Join(filepath.Dir(l.relay.lock.Name()), "acp-empty")
		if err = os.MkdirAll(parent, 0700); err == nil {
			empty, err = os.MkdirTemp(parent, "workspace-")
		}
		if err == nil {
			snapshot, err = sandbox.SnapshotWorkspace(ctx, empty, l.relay.sandboxStore(), runtime.Limits)
			ownSnapshot = true
		}
	}
	if err != nil {
		if empty != "" {
			_ = os.RemoveAll(empty)
		}
		return nil, sandboxError(err)
	}
	var proxy *netpolicy.Proxy
	var socketDir string
	var state *sandbox.PrivateState
	var unlock func()
	release := func() {
		if proxy != nil {
			_ = proxy.Close()
		}
		if socketDir != "" {
			_ = os.RemoveAll(socketDir)
		}
		if ownSnapshot {
			_ = snapshot.Close()
		}
		if empty != "" {
			_ = os.RemoveAll(empty)
		}
		if state != nil && l.disposableState {
			_ = state.Remove()
		}
		if unlock != nil {
			unlock()
		}
	}
	success := false
	defer func() {
		if !success {
			release()
		}
	}()
	_, unlock, err = process.RuntimeStateDir("acp", l.agent.ID, l.disposableState)
	if err != nil {
		return nil, model.Err(409, "the ACP native state is already owned or unavailable")
	}
	socketRoot := filepath.Join(filepath.Dir(l.relay.lock.Name()), "acp-sockets")
	if err = os.MkdirAll(socketRoot, 0700); err != nil {
		return nil, err
	}
	socketDir, err = os.MkdirTemp(socketRoot, "worker-")
	if err != nil {
		return nil, err
	}
	environment, err := l.relay.ACPSecretEnvironment(ctx, l.stored)
	if err != nil {
		return nil, err
	}
	for name, value := range config.Env {
		environment[name] = value
	}
	active := func(requestContext context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if l.active != nil {
			if err := l.active(requestContext); err != nil {
				return err
			}
		}
		if l.broker != nil {
			l.relay.mu.Lock()
			_, _, _, err := l.broker.activeAttemptLocked(false)
			l.relay.mu.Unlock()
			if err != nil {
				return err
			}
			if err := l.relay.validateSecretVersions(requestContext, l.broker.snapshot.SecretVersions); err != nil {
				return err
			}
		}
		current, err := l.relay.ACPAgent(requestContext, l.stored.ID)
		if err != nil {
			return model.Err(403, "ACP credential grant is unavailable")
		}
		for _, reference := range l.stored.SecretEnvRefs {
			if !slices.Contains(current.SecretEnvRefs, reference) {
				return model.Err(403, "ACP credential grant was revoked")
			}
			name, id, ok := strings.Cut(reference, "=")
			if !ok {
				return model.Err(403, "invalid ACP secret reference")
			}
			value, err := l.relay.readSecret(requestContext, id)
			if err != nil || subtle.ConstantTimeCompare([]byte(value), []byte(environment[name])) != 1 {
				return model.Err(403, "ACP credential was revoked or replaced")
			}
		}
		return nil
	}
	proxy, err = l.relay.networkProxy(ctx, filepath.Join(socketDir, "egress.sock"), netpolicy.Scope{ConnectionID: config.ID}, netpolicy.ProxyOptions{Active: active, AllowedOrigins: config.AllowedOrigins})
	if err != nil {
		return nil, err
	}
	state, err = sandbox.OpenPrivateState(l.relay.sandboxStore(), "acp"+tokenHash(l.agent.ID))
	if err != nil {
		return nil, sandboxError(err)
	}
	mounts := []sandbox.SocketMount{{Source: proxy.Path(), Target: "/run/meshra/egress.sock", Purpose: "egress"}}
	if l.brokerSocket != "" {
		mounts = append(mounts, sandbox.SocketMount{Source: l.brokerSocket, Target: "/run/meshra/broker.sock", Purpose: "broker"})
	}
	authFiles, err := acpAuthenticationFiles(config.ReadOnlyAuthFiles)
	if err != nil {
		return nil, err
	}
	argv := []string{runtime.Executables["node"], runtime.Executables["acp-bridge"], config.Executable}
	argv = append(argv, config.Args...)
	worker, err := sandbox.Prepare(ctx, runtime, sandbox.Request{Kind: sandbox.ACP, Workspace: snapshot, Argv: argv, Env: environment, ReadOnly: true, SocketMounts: mounts, AuthFiles: authFiles, State: state, BeforeStart: func() error { return active(ctx) }})
	if err != nil {
		return nil, sandboxError(err)
	}
	child, err := process.StartConfined(ctx, worker)
	if err != nil {
		return nil, sandboxError(err)
	}
	success = true
	return &acpOwnedTransport{Process: child, release: release}, nil
}

type acpOwnedTransport struct {
	*process.Process
	once    sync.Once
	release func()
}

func (t *acpOwnedTransport) Close() error {
	var err error
	t.once.Do(func() { err = t.Process.Close(); t.release() })
	return err
}
func acpAuthenticationFiles(paths []string) ([]sandbox.AuthFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	files := make([]sandbox.AuthFile, 0, len(paths))
	for _, path := range paths {
		relative, err := filepath.Rel(home, path)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
			return nil, model.Err(403, "selected ACP authentication files must be within the native user home")
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
			file.Close()
			return nil, model.Err(403, "invalid selected ACP authentication file")
		}
		hash := sha256.New()
		_, err = io.Copy(hash, io.LimitReader(file, 8<<20+1))
		file.Close()
		if err != nil {
			return nil, err
		}
		files = append(files, sandbox.AuthFile{Source: path, Target: filepath.Join("/home/worker", relative), SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	return files, nil
}
