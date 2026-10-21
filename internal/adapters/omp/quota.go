package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"meshra/internal/model"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type quotaOutput struct{ bytes.Buffer }

func (b *quotaOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 2*1024*1024 {
		return 0, errors.New("quota output limit")
	}
	return b.Buffer.Write(p)
}
func readQuotas(ctx context.Context) ([]model.QuotaWindow, error) {
	cmd := exec.CommandContext(ctx, "omp", "usage", "--json", "--redact", "--no-extensions")
	cmd.Dir, _ = os.UserHomeDir()
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var out quotaOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parseQuotas(out.Bytes())
}
func parseQuotas(raw []byte) ([]model.QuotaWindow, error) {
	var reply struct {
		Reports []struct {
			Provider  string `json:"provider"`
			FetchedAt int64  `json:"fetchedAt"`
			Limits    []struct {
				Window *struct {
					Label string `json:"label"`
					Reset *int64 `json:"resetsAt"`
				} `json:"window"`
				Amount struct {
					Used              *float64 `json:"used"`
					Limit             *float64 `json:"limit"`
					Remaining         *float64 `json:"remaining"`
					UsedFraction      *float64 `json:"usedFraction"`
					RemainingFraction *float64 `json:"remainingFraction"`
					Unit              string   `json:"unit"`
				} `json:"amount"`
			} `json:"limits"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	out := []model.QuotaWindow{}
	for i, report := range reply.Reports {
		for j, limit := range report.Limits {
			if len(out) >= 64 {
				return out, nil
			}
			a := limit.Amount
			var used *float64
			if a.UsedFraction != nil {
				v := *a.UsedFraction * 100
				used = &v
			} else if a.Used != nil && a.Limit != nil && *a.Limit > 0 {
				v := *a.Used / *a.Limit * 100
				used = &v
			} else if a.Unit == "percent" && a.Used != nil {
				used = a.Used
			} else if a.RemainingFraction != nil {
				v := (1 - *a.RemainingFraction) * 100
				used = &v
			}
			label := "Provider window"
			var reset, observed *time.Time
			if limit.Window != nil {
				if limit.Window.Label != "" {
					label = limit.Window.Label
				}
				if limit.Window.Reset != nil && *limit.Window.Reset > 0 {
					v := time.UnixMilli(*limit.Window.Reset).UTC()
					reset = &v
				}
			}
			if report.FetchedAt > 0 {
				v := time.UnixMilli(report.FetchedAt).UTC()
				observed = &v
			}
			out = append(out, model.QuotaWindow{ID: fmt.Sprintf("%d/%d", i, j), Provider: report.Provider, Account: fmt.Sprintf("Account %d", i+1), Label: label, UsedPercent: used, Remaining: a.Remaining, Unit: a.Unit, ResetsAt: reset, ObservedAt: observed})
		}
	}
	return out, nil
}
