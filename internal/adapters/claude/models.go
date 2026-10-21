package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"meshra/internal/model"
	"syscall"
	"time"
)

func (r *runtimeSession) Models(ctx context.Context) ([]model.ModelOption, error) {
	cmd := r.command(ctx)
	cmd.Dir = r.agent.Cwd
	payload, _ := json.Marshal(map[string]any{"discoverModels": true, "cwd": r.agent.Cwd})
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	out := []model.ModelOption{}
	found := false
	for scanner.Scan() {
		var f struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if json.Unmarshal(scanner.Bytes(), &f) != nil {
			break
		}
		if f.Type == "models" {
			if json.Unmarshal([]byte(f.Data), &out) != nil {
				break
			}
			found = true
		}
	}
	err = cmd.Wait()
	if !found || scanner.Err() != nil || err != nil {
		return nil, errors.New("Claude model catalog unavailable")
	}
	return out, nil
}
