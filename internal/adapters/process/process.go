// Package process owns bounded JSONL subprocess transports on Unix.
package process

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const MaxFrame = 1024 * 1024
const MaxLogical = 4 * 1024 * 1024

type packet struct {
	data json.RawMessage
	err  error
}
type Process struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	frames chan packet
	done   chan struct{}
	stop   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

// StateDir separates harness state by canonical identity, never by the latest session.
// The lock also prevents two daemons from owning the same identity's native state.
func StateDir(harness, identity string) (string, func(), error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(base, "meshra", "runtimes", harness, fmt.Sprintf("%x", sha256.Sum256([]byte(identity))))
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return "", nil, fmt.Errorf("native session is already owned: %w", err)
	}
	return dir, func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

// RuntimeStateDir uses disposable state for chat turns, so they can run alongside
// an identity's idle durable runtime without competing for its ownership lock.
func RuntimeStateDir(harness, identity string, ephemeral bool) (string, func(), error) {
	if !ephemeral {
		return StateDir(harness, identity)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", nil, err
	}
	parent := filepath.Join(base, "meshra", "runtimes", harness)
	if err = os.MkdirAll(parent, 0700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(parent, "chat-")
	if err != nil {
		return "", nil, err
	}
	var once sync.Once
	return dir, func() { once.Do(func() { _ = os.RemoveAll(dir) }) }, nil
}

func Start(ctx context.Context, binary string, args []string, cwd string, env []string) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = cwd
	if env != nil {
		cmd.Env = env
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = io.Discard // Native diagnostics may contain credentials or complete prompts.
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, fmt.Errorf("start %s (install CLI and authenticate natively): %w", binary, err)
	}
	p := &Process{cmd: cmd, in: in, frames: make(chan packet, 64), done: make(chan struct{}), stop: make(chan struct{})}
	go func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), MaxFrame)
		var scanErr error
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			if !json.Valid(line) {
				scanErr = errors.New("invalid JSON protocol frame")
				break
			}
			frame := append(json.RawMessage(nil), line...)
			select {
			case p.frames <- packet{data: frame}:
			case <-p.stop:
			}
		}
		if err := scanner.Err(); err != nil {
			scanErr = fmt.Errorf("protocol frame exceeded limit or read failed: %w", err)
		}
		if scanErr != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		waitErr := cmd.Wait()
		p.mu.Lock()
		switch {
		case scanErr != nil:
			p.err = scanErr
		case waitErr != nil:
			p.err = fmt.Errorf("native process exited: %w", waitErr)
		default:
			p.err = io.EOF
		}
		p.mu.Unlock()
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Close()
		case <-p.done:
		}
	}()
	return p, nil
}

func (p *Process) Read(ctx context.Context) (json.RawMessage, error) {
	// Drain frames delivered immediately before EOF before reporting process failure.
	select {
	case frame := <-p.frames:
		return frame.data, frame.err
	default:
	}
	select {
	case frame := <-p.frames:
		return frame.data, frame.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		select {
		case frame := <-p.frames:
			return frame.data, frame.err
		default:
		}
		p.mu.Lock()
		err := p.err
		p.mu.Unlock()
		return nil, err
	}
}

// Send bounds writes and tears down the process if its stdin stops accepting data.
func (p *Process) Send(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxFrame {
		return errors.New("outbound protocol frame exceeds 1 MiB")
	}
	data = append(data, '\n')
	result := make(chan error, 1)
	go func() { p.mu.Lock(); defer p.mu.Unlock(); _, err := p.in.Write(data); result <- err }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = p.Close()
		return ctx.Err()
	case <-p.done:
		return errors.New("native process closed while sending command")
	}
}

func (p *Process) Close() error {
	p.once.Do(func() {
		close(p.stop)
		_ = p.in.Close()
		select {
		case <-p.done:
		case <-time.After(500 * time.Millisecond):
		}
		// Kill the entire owned group even when its leader exited normally.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	})
	return nil
}
