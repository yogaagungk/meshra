//go:build linux

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

const (
	maxBackgroundProcesses = 4
	maxBackgroundLogBytes  = 256 << 10
	maxBackgroundReadBytes = 64 << 10
)

// ProcessManager owns background commands for one trusted turn. The caller must
// check approval and authorization in Request.BeforeStart, use a turn-scoped
// context, and close the manager before closing its workspace snapshot.
type ProcessManager struct {
	mu        sync.Mutex
	closeDone chan struct{}
	closeErr  error
	ctx       context.Context
	closed    bool
	processes map[string]*backgroundProcess
}

type backgroundProcess struct {
	mu        sync.Mutex
	worker    *Prepared
	log       []byte
	truncated bool
	finished  bool
	exitCode  *int
	failure   string
	done      chan struct{}
}

// ProcessStatus is a bounded, stable view of a process. ExitCode is absent
// until the worker has exited. Failure reports timeout or launcher/cleanup
// errors, not ordinary nonzero exit status.
type ProcessStatus struct {
	Finished  bool   `json:"finished"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	Failure   string `json:"failure,omitempty"`
	LogBytes  int    `json:"logBytes"`
	Truncated bool   `json:"truncated"`
}

func NewProcessManager(ctx context.Context) *ProcessManager {
	return &ProcessManager{ctx: ctx, processes: make(map[string]*backgroundProcess), closeDone: make(chan struct{})}
}

// Start accepts only already-authorized command requests. BeforeStart is
// mandatory so approval can be revalidated and the invocation claimed at the
// same boundary as a foreground command. Records remain until Close, bounding
// both process count and retained logs for the entire turn.
func (m *ProcessManager) Start(config Config, request Request) (string, error) {
	id, _, err := m.StartContext(m.ctx, config, request)
	return id, err
}

// StartContext additionally binds the worker to a trusted owner's lifetime.
// The returned channel closes only after the worker is reaped and cleaned up.
func (m *ProcessManager) StartContext(ctx context.Context, config Config, request Request) (string, <-chan struct{}, error) {
	if request.Kind != "" && request.Kind != Command {
		return "", nil, ErrDenied
	}
	if request.BeforeStart == nil {
		return "", nil, fmt.Errorf("%w: background command requires a start authorization guard", ErrDenied)
	}
	if request.Timeout == 0 {
		request.Timeout = 60 * time.Second
	}
	if request.Timeout < time.Second || request.Timeout > 300*time.Second {
		return "", nil, fmt.Errorf("invalid worker timeout")
	}
	request.Kind = Command
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx == nil || ctx == nil {
		return "", nil, ErrDenied
	}
	if len(m.processes) >= maxBackgroundProcesses {
		return "", nil, fmt.Errorf("%w: maximum background process records", ErrLimit)
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", nil, err
	}
	id := hex.EncodeToString(idBytes[:])
	if _, exists := m.processes[id]; exists {
		return "", nil, fmt.Errorf("background process ID collision")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	worker, err := Prepare(workerCtx, config, request)
	if err != nil {
		stop()
		cancel()
		return "", nil, err
	}
	p := &backgroundProcess{worker: worker, done: make(chan struct{})}
	worker.Cmd.Stdout = p
	worker.Cmd.Stderr = p
	if err = worker.Start(); err != nil {
		stop()
		cancel()
		return "", nil, errors.Join(err, worker.Close())
	}
	m.processes[id] = p
	go func() {
		p.wait()
		stop()
		cancel()
	}()
	return id, p.done, nil
}

// Write drains all output even after the retained log limit is reached, so a
// noisy process cannot deadlock on a full pipe or allocate unbounded memory.
func (p *backgroundProcess) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := min(len(data), maxBackgroundLogBytes-len(p.log))
	p.log = append(p.log, data[:n]...)
	if n < len(data) {
		p.truncated = true
	}
	return len(data), nil
}

func (p *backgroundProcess) wait() {
	err := p.worker.Wait()
	ctxErr := p.worker.ctx.Err()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		err = nil
	}
	if ctxErr != nil {
		err = errors.Join(err, ctxErr)
	}
	err = errors.Join(err, p.worker.Close())
	p.mu.Lock()
	if p.worker.Cmd.ProcessState != nil {
		code := p.worker.Cmd.ProcessState.ExitCode()
		p.exitCode = &code
	}
	if err != nil {
		p.failure = err.Error()
	}
	p.finished = true
	close(p.done)
	p.mu.Unlock()
}

func (m *ProcessManager) lookup(id string) (*backgroundProcess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrDenied
	}
	p := m.processes[id]
	if p == nil {
		return nil, fmt.Errorf("background process not found")
	}
	return p, nil
}

func (m *ProcessManager) Poll(id string) (ProcessStatus, error) {
	p, err := m.lookup(id)
	if err != nil {
		return ProcessStatus{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProcessStatus{Finished: p.finished, ExitCode: p.exitCode, Failure: p.failure, LogBytes: len(p.log), Truncated: p.truncated}, nil
}

// ReadLog reads a byte range of the combined stdout/stderr log.
func (m *ProcessManager) ReadLog(id string, offset, limit int) ([]byte, error) {
	if offset < 0 || limit < 1 || limit > maxBackgroundReadBytes {
		return nil, fmt.Errorf("invalid bounded log read")
	}
	p, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if offset >= len(p.log) {
		return []byte{}, nil
	}
	end := min(len(p.log)-offset, limit) + offset
	return append([]byte(nil), p.log[offset:end]...), nil
}

func (m *ProcessManager) Kill(id string) error {
	p, err := m.lookup(id)
	if err != nil {
		return err
	}
	err = p.worker.Close()
	<-p.done
	return err
}

// Close kills and reaps every process before returning, including descendants.
// A cleanup failure is returned to the caller and the snapshot remains held by
// its worker rather than being removed unsafely.
func (m *ProcessManager) Close() error {
	m.mu.Lock()
	if m.closed {
		done := m.closeDone
		m.mu.Unlock()
		<-done
		return m.closeErr
	}
	m.closed = true
	processes := make([]*backgroundProcess, 0, len(m.processes))
	for _, p := range m.processes {
		processes = append(processes, p)
	}
	m.mu.Unlock()
	var err error
	for _, p := range processes {
		err = errors.Join(err, p.worker.Close())
		<-p.done
	}
	m.closeErr = err
	close(m.closeDone)
	return err
}
