package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"time"

	"meshra/internal/model"
)

const browserWireFrameLimit = 3 << 20 //2MiB viewer PNG, base64 and bounded metadata

type browserWorkerFrame struct {
	ID     string        `json:"id"`
	Type   string        `json:"type"`
	OK     bool          `json:"ok"`
	Error  string        `json:"error"`
	Result browserResult `json:"result"`
}

func readBrowserWorkerFrame(reader *bufio.Reader) (browserWorkerFrame, error) {
	var collected []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(collected)+len(fragment) > browserWireFrameLimit {
			return browserWorkerFrame{}, model.Err(413, "browser worker frame exceeds3MiB")
		}
		if err == nil {
			if len(collected) != 0 {
				collected = append(collected, fragment...)
				fragment = collected
			}
			var frame browserWorkerFrame
			if json.Unmarshal(fragment, &frame) != nil {
				return frame, model.Err(503, "browser bridge sent an invalid frame")
			}
			return frame, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return browserWorkerFrame{}, model.Err(503, "browser bridge stopped responding")
		}
		collected = append(collected, fragment...)
	}
}

func (w *confinedBrowserWorker) readFrame(ctx context.Context, timeout time.Duration) (browserWorkerFrame, error) {
	type outcome struct {
		frame browserWorkerFrame
		err   error
	}
	results := make(chan outcome, 1)
	go func() { frame, err := readBrowserWorkerFrame(w.stdout); results <- outcome{frame, err} }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		_ = w.Close()
		<-results
		return browserWorkerFrame{}, ctx.Err()
	case <-timer.C:
		_ = w.Close()
		<-results
		return browserWorkerFrame{}, model.Err(503, "browser bridge did not answer in time")
	case result := <-results:
		if result.err != nil {
			_ = w.Close()
		}
		return result.frame, result.err
	}
}
