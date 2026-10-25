package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
)

const (
	voiceConcurrentLimit = 8
	voiceSessionConcurrentLimit = 2
	voiceSessionJobLimit = 32
	voiceSweepLimit = 64
)

type voiceWorker struct {
	owner string
	cancel context.CancelFunc
	done chan struct{}
}

type voiceRuntime struct {
	mu sync.Mutex // before r.mu; never held when invoking a session validator
	workers map[string]*voiceWorker
	executor voiceExecutor // package-private test seam; never populated in production
	stopped bool
	sweepCursor string
}

var voiceRuntimes = struct {
	sync.Mutex
	items map[*Relay]*voiceRuntime
}{items: make(map[*Relay]*voiceRuntime)}

func (r *Relay) voiceRuntimeState() (*voiceRuntime, error) {
	voiceRuntimes.Lock()
	defer voiceRuntimes.Unlock()
	if r.ctx.Err() != nil {
		return nil, model.Err(503, "relay is stopping")
	}
	state := voiceRuntimes.items[r]
	if state == nil {
		state = &voiceRuntime{workers: make(map[string]*voiceWorker)}
		voiceRuntimes.items[r] = state
	}
	return state, nil
}

func (r *Relay) setVoiceExecutor(executor voiceExecutor) {
	state, err := r.voiceRuntimeState()
	if err != nil { return }
	state.mu.Lock()
	state.executor = executor
	state.mu.Unlock()
}

func (r *Relay) injectedVoiceExecutor() voiceExecutor {
	state, err := r.voiceRuntimeState()
	if err != nil { return nil }
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.executor
}

func (r *Relay) voiceSessionRevoked(owner string) bool {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	return r.revokedConsoleSessions[owner]
}

func (r *Relay) startVoiceJob(request context.Context, p model.Principal, settings voiceSettings, job voiceJob, run func(context.Context) (string, []byte, error)) (voiceJob, error) {
	if err := request.Err(); err != nil { return voiceJob{}, err }
	// The HTTP request owns admission only. Accepted jobs outlive that request,
	// but not the real console session, relay, or the single 180-second wall.
	owned, release, err := r.beginConsoleJob(r.ctx, p.ConsoleSessionID)
	if err != nil { return voiceJob{}, err }
	workerContext, cancel := context.WithTimeout(owned, voiceJobWallTime)
	state, err := r.voiceRuntimeState()
	if err != nil { cancel(); release(); return voiceJob{}, err }
	state.mu.Lock()
	r.mu.Lock()
	fail := func(err error) (voiceJob, error) {
		r.mu.Unlock(); state.mu.Unlock(); cancel(); release()
		return voiceJob{}, err
	}
	if state.stopped || r.closed || workerContext.Err() != nil || r.voiceSessionRevoked(p.ConsoleSessionID) {
		return fail(model.Err(403, "voice session is no longer active"))
	}
	current, err := r.voiceSettingsLocked()
	if err != nil { return fail(err) }
	if current.Revision != settings.Revision { return fail(model.Err(409, "voice settings changed before admission")) }
	_, runtimeRevision, err := r.runtimeConfigLocked()
	if err != nil { return fail(err) }
	active := 0
	for _, worker := range state.workers { if worker.owner == p.ConsoleSessionID { active++ } }
	if len(state.workers) >= voiceConcurrentLimit || active >= voiceSessionConcurrentLimit {
		return fail(model.Err(429, "local voice worker capacity reached"))
	}
	var retained int
	if err = r.db.QueryRow("SELECT count(*) FROM voice_jobs WHERE session_owner=? AND expires_at>? AND state IN ('pending','running','completed','failed')", p.ConsoleSessionID, now().Format(time.RFC3339Nano)).Scan(&retained); err != nil { return fail(err) }
	if retained >= voiceSessionJobLimit { return fail(model.Err(429, "voice session job capacity reached; cancel jobs or wait for expiry")) }
	job.ID = randomID()
	job.State = "pending"
	job.CreatedAt = now()
	job.UpdatedAt = job.CreatedAt
	job.ExpiresAt = job.CreatedAt.Add(voiceFileTTL)
	if err = r.storeVoiceJobLocked(job); err != nil { return fail(err) }
	worker := &voiceWorker{owner: p.ConsoleSessionID, cancel: cancel, done: make(chan struct{})}
	state.workers[job.ID] = worker
	r.mu.Unlock()
	state.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			release()
			state.mu.Lock()
			delete(state.workers, job.ID)
			close(worker.done)
			state.mu.Unlock()
		}()
		if !r.voiceMarkRunning(workerContext, state, job.ID) { return }
		transcript, audio, runErr := run(workerContext)
		defer clear(audio)
		r.finishVoiceJob(workerContext, p, state, job.ID, settings.Revision, runtimeRevision, transcript, audio, runErr)
	}()
	return job, nil
}

func (r *Relay) voiceMarkRunning(ctx context.Context, state *voiceRuntime, id string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	job, err := r.voiceJobRecord(id)
	if err != nil || job.State != "pending" || ctx.Err() != nil || state.stopped || r.closed { return false }
	job.State = "running"
	job.UpdatedAt = now()
	return r.storeVoiceJobLocked(job) == nil
}

func (r *Relay) finishVoiceJob(ctx context.Context, p model.Principal, state *voiceRuntime, id string, settingsRevision, runtimeRevision int, transcript string, audio []byte, runErr error) {
	// A callback may reenter the relay. Resolve it before taking either lock;
	// durable state, revision and revocation are checked again under ownership.
	authorized := r.requireConsole(p) == nil
	state.mu.Lock()
	defer state.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	job, err := r.voiceJobRecord(id)
	if err != nil || (job.State != "pending" && job.State != "running") { return }
	settings, settingsErr := r.voiceSettingsLocked()
	_, revision, runtimeErr := r.runtimeConfigLocked()
	if !job.ExpiresAt.After(now()) {
		_ = r.terminalVoiceJobLocked(&job, "expired")
		return
	}
	if !authorized || r.voiceSessionRevoked(job.SessionOwner) || ctx.Err() != nil || state.stopped || r.closed || settingsErr != nil || runtimeErr != nil || settings.Revision != settingsRevision || revision != runtimeRevision {
		_ = r.terminalVoiceJobLocked(&job, "cancelled")
		return
	}
	if runErr == nil && job.Kind == "transcribe" && (transcript == "" || len(transcript) > voiceTranscriptLimit || !utf8.ValidString(transcript)) {
		runErr = model.Err(503, "local speech to text produced an invalid transcript")
	}
	if runErr == nil && job.Kind == "synthesize" { runErr = voiceValidateWAV(audio, false) }
	if runErr != nil {
		job.State = "failed"
		// Native stderr and input text may contain secrets. The durable failure
		// is intentionally generic; configuration/status provides prerequisites.
		job.Error = "local voice processing failed; check configured native runtime and audio input"
		job.UpdatedAt = now()
		_ = r.storeVoiceJobLocked(job)
		return
	}
	if job.Kind == "synthesize" {
		name, err := r.voiceWriteAudio(job.ID, audio)
		if err != nil {
			job.State = "failed"
			job.Error = "local voice audio could not be stored privately"
			job.UpdatedAt = now()
			_ = r.storeVoiceJobLocked(job)
			return
		}
		job.AudioFile = name
		job.AudioAvailable = true
		job.AudioBytes = int64(len(audio))
	} else { job.Transcript = transcript }
	// Logout sets the private revoked bit before waiting for relay ownership.
	if ctx.Err() != nil || r.voiceSessionRevoked(job.SessionOwner) {
		_ = r.terminalVoiceJobLocked(&job, "cancelled")
		return
	}
	job.State = "completed"
	job.UpdatedAt = now()
	if err = r.storeVoiceJobLocked(job); err != nil { _ = r.voiceRemoveAudio(job) }
}

func (r *Relay) voiceWriteAudio(id string, audio []byte) (string, error) {
	if filepath.Base(id) != id || id == "." || id == "" { return "", errors.New("invalid voice id") }
	dir, err := r.voiceDirectory()
	if err != nil { return "", err }
	file, err := os.CreateTemp(dir, ".voice-*")
	if err != nil { return "", err }
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(audio); err == nil { err = file.Sync() }
	err = errors.Join(err, file.Close())
	if err != nil { return "", err }
	name := id + ".wav"
	if err = os.Rename(temporary, filepath.Join(dir, name)); err != nil { return "", err }
	return name, nil
}

func (r *Relay) voiceRemoveAudio(job voiceJob) error {
	if job.AudioFile == "" { return nil }
	if filepath.Base(job.ID) != job.ID || job.AudioFile != job.ID+".wav" { return errors.New("invalid private voice audio path") }
	dir, err := r.voiceDirectory()
	if err != nil { return err }
	err = os.Remove(filepath.Join(dir, job.AudioFile))
	if errors.Is(err, os.ErrNotExist) { return nil }
	return err
}

// Caller owns state.mu and r.mu. Invalidation precedes worker drain, so a late
// native result can never overwrite a terminal job or publish a playable file.
func (r *Relay) terminalVoiceJobLocked(job *voiceJob, terminal string) error {
	removeErr := r.voiceRemoveAudio(*job)
	job.State = terminal
	job.UpdatedAt = now()
	job.Transcript = ""
	job.Error = ""
	job.SourceAttachmentID = ""
	job.AudioAvailable = false
	job.AudioFile = ""
	job.AudioBytes = 0
	return errors.Join(removeErr, r.storeVoiceJobLocked(*job))
}

func (r *Relay) readVoiceJob(ctx context.Context, p model.Principal, id string) (voiceJob, error) {
	if err := ctx.Err(); err != nil { return voiceJob{}, err }
	if err := r.requireConsole(p); err != nil { return voiceJob{}, err }
	state, err := r.voiceRuntimeState()
	if err != nil { return voiceJob{}, err }
	state.mu.Lock()
	r.mu.Lock()
	job, err := r.voiceJobRecord(id)
	var worker *voiceWorker
	if err == nil && job.SessionOwner != p.ConsoleSessionID { err = model.Err(403, "voice job belongs to another console session") }
	if err == nil && r.voiceSessionRevoked(p.ConsoleSessionID) { err = model.Err(403, "console session is no longer active") }
	if err == nil && !job.ExpiresAt.After(now()) && job.State != "expired" {
		worker = state.workers[id]
		if worker != nil { worker.cancel() }
		err = r.terminalVoiceJobLocked(&job, "expired")
	}
	r.mu.Unlock()
	state.mu.Unlock()
	if worker != nil { <-worker.done }
	return job, err
}

func (r *Relay) cancelVoiceJob(ctx context.Context, p model.Principal, id string) (voiceJob, error) {
	if err := ctx.Err(); err != nil { return voiceJob{}, err }
	if err := r.requireConsole(p); err != nil { return voiceJob{}, err }
	state, err := r.voiceRuntimeState()
	if err != nil { return voiceJob{}, err }
	state.mu.Lock()
	r.mu.Lock()
	job, err := r.voiceJobRecord(id)
	if err == nil && job.SessionOwner != p.ConsoleSessionID { err = model.Err(403, "voice job belongs to another console session") }
	worker := state.workers[id]
	if err == nil {
		if worker != nil { worker.cancel() }
		terminal := "cancelled"
		if job.State == "expired" || !job.ExpiresAt.After(now()) { terminal = "expired" }
		err = r.terminalVoiceJobLocked(&job, terminal)
	}
	r.mu.Unlock()
	state.mu.Unlock()
	// Do not acknowledge cancellation until the actual confined process has
	// exited and released all staged private files, even if HTTP disconnects.
	if err == nil && worker != nil { <-worker.done }
	return job, err
}

func (r *Relay) readVoiceAudio(ctx context.Context, p model.Principal, id string) ([]byte, error) {
	job, err := r.readVoiceJob(ctx, p, id)
	if err != nil { return nil, err }
	if job.State != "completed" || !job.AudioAvailable || job.AudioFile != id+".wav" { return nil, model.Err(404, "voice audio is no longer available") }
	state, err := r.voiceRuntimeState()
	if err != nil { return nil, err }
	// Validate outside locks immediately before the atomic owned read.
	if err = r.requireConsole(p); err != nil { return nil, err }
	state.mu.Lock()
	defer state.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	job, err = r.voiceJobRecord(id)
	if err != nil { return nil, err }
	if ctx.Err() != nil { return nil, ctx.Err() }
	if r.closed || state.stopped || r.voiceSessionRevoked(p.ConsoleSessionID) || job.SessionOwner != p.ConsoleSessionID || !job.ExpiresAt.After(now()) || job.State != "completed" || !job.AudioAvailable || job.AudioFile != id+".wav" { return nil, model.Err(404, "voice audio is no longer available") }
	dir, err := r.voiceDirectory()
	if err != nil { return nil, err }
	audio, err := voiceReadAudioFile(filepath.Join(dir, job.AudioFile))
	if err != nil { return nil, model.Err(404, "voice audio is no longer available") }
	if int64(len(audio)) != job.AudioBytes || voiceValidateWAV(audio, false) != nil { return nil, model.Err(503, "stored voice WAV is invalid") }
	return audio, nil
}

func voiceReadAudioFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil { return nil, err }
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > voiceAudioLimit { return nil, model.Err(413, "voice audio must be a regular WAV no larger than 20 MiB") }
	file, err := os.Open(path)
	if err != nil { return nil, err }
	defer file.Close()
	opened, err := file.Stat()
	if err != nil { return nil, err }
	if !os.SameFile(info, opened) { return nil, errors.New("voice audio changed while opening") }
	audio, err := io.ReadAll(io.LimitReader(file, voiceAudioLimit+1))
	if len(audio) > voiceAudioLimit { return nil, model.Err(413, "voice audio exceeds 20 MiB") }
	return audio, err
}

func (r *Relay) commitVoiceConfiguration(expected int, settings voiceSettings) (any, error) {
	state, err := r.voiceRuntimeState()
	if err != nil { return nil, err }
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	state.mu.Lock()
	r.mu.Lock()
	workers := []*voiceWorker{}
	commit := func() error {
		if r.closed || state.stopped { return model.Err(503, "relay is stopping") }
		current, err := r.voiceSettingsLocked()
		if err != nil { return err }
		if current.Revision != expected { return model.Err(409, "voice settings revision changed") }
		settings.Revision = current.Revision + 1
		body, err := json.Marshal(settings)
		if err != nil { return err }
		tx, err := r.db.Begin()
		if err != nil { return err }
		defer tx.Rollback()
		if _, err = tx.Exec("INSERT INTO voice_settings(id,revision,body) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,body=excluded.body", settings.Revision, string(body)); err != nil { return err }
		if err = insertAutomationEvent(tx, "voice.configured", "voice", "", map[string]any{"revision": settings.Revision}); err != nil { return err }
		return tx.Commit()
	}
	err = commit()
	if err == nil {
		for _, worker := range state.workers { worker.cancel(); workers = append(workers, worker) }
		_, cleanupErr := r.invalidateVoiceJobsLocked("", "cancelled", 0)
		err = errors.Join(err, cleanupErr)
	}
	r.mu.Unlock()
	state.mu.Unlock()
	for _, worker := range workers { <-worker.done }
	if err != nil { return nil, err }
	return settings, nil
}

// Caller owns r.mu; rows are completely closed before any write (SQLite uses a
// single connection). limit=0 is used only for explicit teardown/configuration.
func (r *Relay) invalidateVoiceJobsLocked(owner, terminal string, limit int) (int, error) {
	query := "SELECT id FROM voice_jobs WHERE state IN ('pending','running','completed','failed')"
	args := []any{}
	if owner != "" { query += " AND session_owner=?"; args = append(args, owner) }
	if limit > 0 { query += " LIMIT ?"; args = append(args, limit) }
	rows, err := r.db.Query(query, args...)
	if err != nil { return 0, err }
	ids := []string{}
	for rows.Next() { var id string; if err = rows.Scan(&id); err != nil { break }; ids = append(ids, id) }
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil { return 0, err }
	for _, id := range ids {
		job, loadErr := r.voiceJobRecord(id)
		if loadErr == nil { loadErr = r.terminalVoiceJobLocked(&job, terminal) }
		err = errors.Join(err, loadErr)
	}
	return len(ids), err
}

// revokeVoiceSession runs without relay/session ownership and acknowledges only
// after actual workers have drained and private audio/transcripts are removed.
func (r *Relay) revokeVoiceSession(owner string) error {
	if owner == "" { return nil }
	state, err := r.voiceRuntimeState()
	if err != nil { return err }
	state.mu.Lock()
	workers := []*voiceWorker{}
	for _, worker := range state.workers { if worker.owner == owner { worker.cancel(); workers = append(workers, worker) } }
	r.mu.Lock()
	_, err = r.invalidateVoiceJobsLocked(owner, "cancelled", 0)
	r.mu.Unlock()
	state.mu.Unlock()
	for _, worker := range workers { <-worker.done }
	return err
}

// sweepVoiceJobs scans at most 64 durable jobs per call, including sessions that
// expired without a logout request. The cursor prevents active rows starving
// later sessions. Parent calls it periodically outside all relay/session locks.
func (r *Relay) sweepVoiceJobs(ctx context.Context, at time.Time) error {
	state, err := r.voiceRuntimeState()
	if err != nil { return err }
	state.mu.Lock()
	cursor := state.sweepCursor
	state.mu.Unlock()
	r.mu.Lock()
	rows, err := r.db.Query("SELECT id,session_owner FROM voice_jobs WHERE id>? AND state IN ('pending','running','completed','failed') ORDER BY id LIMIT ?", cursor, voiceSweepLimit)
	if err != nil { r.mu.Unlock(); return err }
	type candidate struct { id, owner string }
	candidates := []candidate{}
	for rows.Next() { var entry candidate; if err = rows.Scan(&entry.id, &entry.owner); err != nil { break }; candidates = append(candidates, entry) }
	err = errors.Join(err, rows.Err(), rows.Close())
	r.mu.Unlock()
	if err != nil { return err }
	state.mu.Lock()
	if len(candidates) < voiceSweepLimit { state.sweepCursor = "" } else { state.sweepCursor = candidates[len(candidates)-1].id }
	state.mu.Unlock()
	for _, entry := range candidates {
		if err := ctx.Err(); err != nil { return err }
		valid := r.requireConsole(model.Principal{Admin: true, ConsoleSessionID: entry.owner}) == nil
		state.mu.Lock()
		r.mu.Lock()
		job, loadErr := r.voiceJobRecord(entry.id)
		var worker *voiceWorker
		if loadErr == nil && (!valid || !job.ExpiresAt.After(at)) && job.State != "cancelled" && job.State != "expired" {
			worker = state.workers[entry.id]
			if worker != nil { worker.cancel() }
			terminal := "expired"
			if !valid { terminal = "cancelled" }
			loadErr = r.terminalVoiceJobLocked(&job, terminal)
		}
		r.mu.Unlock()
		state.mu.Unlock()
		err = errors.Join(err, loadErr)
		if worker != nil { select { case <-worker.done: case <-ctx.Done(): return errors.Join(err, ctx.Err()) } }
	}
	return err
}

// recoverVoiceJobs is startup-only. A restarted daemon cannot revive a former
// console session, playback URL or transcript; remove orphan files as well.
func (r *Relay) recoverVoiceJobs() error {
	r.mu.Lock()
	_, err := r.invalidateVoiceJobsLocked("", "cancelled", 0)
	r.mu.Unlock()
	dir, dirErr := r.voiceDirectory()
	if dirErr != nil { return errors.Join(err, dirErr) }
	entries, dirErr := os.ReadDir(dir)
	if dirErr != nil { return errors.Join(err, dirErr) }
	for _, entry := range entries { err = errors.Join(err, os.Remove(filepath.Join(dir, entry.Name()))) }
	return err
}

// closeVoiceJobs cancels and joins every native worker before the parent's DB
// close. No validator is invoked and the typed registry/test seam is removed.
func (r *Relay) closeVoiceJobs() error {
	voiceRuntimes.Lock()
	state := voiceRuntimes.items[r]
	voiceRuntimes.Unlock()
	if state == nil { return nil }
	state.mu.Lock()
	state.stopped = true
	workers := []*voiceWorker{}
	for _, worker := range state.workers { worker.cancel(); workers = append(workers, worker) }
	r.mu.Lock()
	_, err := r.invalidateVoiceJobsLocked("", "cancelled", 0)
	r.mu.Unlock()
	state.mu.Unlock()
	for _, worker := range workers { <-worker.done }
	voiceRuntimes.Lock()
	delete(voiceRuntimes.items, r)
	voiceRuntimes.Unlock()
	return err
}
