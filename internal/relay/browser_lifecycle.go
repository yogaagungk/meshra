package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image/png"
	"os"
	"time"

	"meshra/internal/model"
)

func browserValidatePNG(frame []byte, limit int) error {
	if len(frame) > limit {
		return model.Err(413, "browser frame exceeds configured image limit")
	}
	if len(frame) < 33 || !bytes.Equal(frame[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return model.Err(503, "browser worker returned invalid PNG pixels")
	}
	config, err := png.DecodeConfig(bytes.NewReader(frame))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return model.Err(503, "browser PNG dimensions are invalid")
	}
	// Validate the complete bounded container without allocating a decoded image.
	imageData := false
	for offset := 8; offset < len(frame); {
		if len(frame)-offset < 12 {
			return model.Err(503, "truncated browser PNG")
		}
		size := uint64(binary.BigEndian.Uint32(frame[offset : offset+4]))
		end := uint64(offset) + 12 + size
		if end > uint64(len(frame)) {
			return model.Err(503, "truncated browser PNG")
		}
		dataEnd := int(end) - 4
		if crc32.ChecksumIEEE(frame[offset+4:dataEnd]) != binary.BigEndian.Uint32(frame[dataEnd:int(end)]) {
			return model.Err(503, "browser PNG checksum mismatch")
		}
		kind := string(frame[offset+4 : offset+8])
		if kind == "IDAT" && size > 0 {
			imageData = true
		}
		if kind == "IEND" {
			if size != 0 || int(end) != len(frame) || !imageData {
				return model.Err(503, "invalid browser PNG ending")
			}
			return nil
		}
		offset = int(end)
	}
	return model.Err(503, "browser PNG has no ending")
}

func (w *confinedBrowserWorker) isClosed() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.closed }

// Stop admission first, then cancel and drain input before clearing ephemeral
// pixels/leases. Persistent browser application data is retained until clearData.
func (r *Relay) browserStopRuntime(runtime *browserRuntime, reason string) error {
	runtime.control.Lock()
	defer runtime.control.Unlock()
	runtime.mu.Lock()
	previous := runtime.record
	if previous.State == browserStateClosed && previous.ArtifactsExpired {
		runtime.mu.Unlock()
		return nil
	}
	next := previous
	next.Epoch++
	next.State = browserStateClosed
	next.LeaseOwner = ""
	next.LeaseExpiresAt = nil
	next.SnapshotText = ""
	next.SnapshotAt = nil
	next.UpdatedAt = now()
	next.ArtifactsExpired = false
	next, err := r.browserPersistEpochChange(previous, next, reason)
	if err != nil {
		runtime.mu.Unlock()
		return err
	}
	runtime.record = next
	cancel := runtime.active
	runtime.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	runtime.dispatch.Lock()
	defer runtime.dispatch.Unlock()
	if runtime.worker != nil {
		err = runtime.worker.Close()
	}
	runtime.frame = nil
	runtime.lastFrame = next.UpdatedAt
	if err != nil {
		return err
	}
	if err = r.browserCleanupTransient(next); err != nil {
		return err
	}
	runtime.worker = nil
	next = runtime.snapshot()
	next.ArtifactsExpired = true
	next.UpdatedAt = now()
	if err = r.storeBrowserSession(next); err != nil {
		return err
	}
	runtime.store(next)
	r.browserManager().remove(next.ID)
	return nil
}

func (r *Relay) browserCleanupTransient(session browserSession) error {
	root, err := os.OpenRoot(r.browserStateDirectory(session.ID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cleanup error
	for _, name := range []string{"uploads", "downloads", "download-staging", "runtime"} {
		cleanup = errors.Join(cleanup, root.RemoveAll(name))
	}
	return errors.Join(cleanup, root.Close())
}

func (r *Relay) recoverBrowserSessions() error {
	for {
		rows, err := r.db.Query("SELECT id FROM browser_sessions WHERE state NOT IN ('closed','error') OR coalesce(json_extract(body,'$.artifactsExpired'),0)=0 LIMIT 64")
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			session, err := r.loadBrowserSession(id)
			if err != nil {
				return err
			}
			if err = r.browserStopRuntime(&browserRuntime{record: session}, "daemon restart invalidated browser control"); err != nil {
				return err
			}
		}
	}
}

// Expiry or disconnect pauses control; it never hands input back to a model.
func (r *Relay) pauseBrowserLease(runtime *browserRuntime, expected browserSession) error {
	runtime.control.Lock()
	defer runtime.control.Unlock()
	runtime.mu.Lock()
	current := runtime.record
	if current.Epoch != expected.Epoch || current.LeaseOwner != expected.LeaseOwner || current.LeaseOwner == "" {
		runtime.mu.Unlock()
		return nil
	}
	next := current
	next.Epoch++
	next.State = browserStateTakingOver
	next.LeaseOwner = ""
	next.LeaseExpiresAt = nil
	next.SnapshotText = ""
	next.SnapshotAt = nil
	next.UpdatedAt = now()
	next, err := r.browserPersistEpochChange(current, next, "human control expired or disconnected; explicit release required")
	if err != nil {
		runtime.mu.Unlock()
		return err
	}
	runtime.record = next
	cancel := runtime.active
	runtime.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	runtime.dispatch.Lock()
	runtime.frame = nil
	runtime.dispatch.Unlock()
	return nil
}

func (r *Relay) revokeBrowserSession(owner string) error {
	var result error
	for _, runtime := range r.browserManager().list() {
		session := runtime.snapshot()
		if session.LeaseOwner == owner && owner != "" {
			result = errors.Join(result, r.pauseBrowserLease(runtime, session))
		}
	}
	return result
}

func (r *Relay) sweepBrowserSessions(ctx context.Context, at time.Time) error {
	var result error
	for _, runtime := range r.browserManager().list() {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		session := runtime.snapshot()
		if session.LeaseOwner != "" && (session.LeaseExpiresAt == nil || !session.LeaseExpiresAt.After(at) || r.requireConsole(model.Principal{Admin: true, ConsoleSessionID: session.LeaseOwner}) != nil) {
			result = errors.Join(result, r.pauseBrowserLease(runtime, session))
		}
		if at.Sub(session.UpdatedAt) >= browserIdleClose {
			result = errors.Join(result, r.browserStopRuntime(runtime, "browser idle timeout"))
		}
	}
	return result
}
