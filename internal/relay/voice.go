package relay

// Local voice runtime: whisper.cpp speech to text and sherpa-onnx Kokoro text to
// speech. Both binaries and every model file are operator installed and
// operator configured. Nothing here downloads a model, calls a cloud service or
// falls back to a browser operating system voice.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"meshra/internal/model"
	"meshra/internal/sandbox"
)

const (
	voiceTextLimit        = 8 << 10
	voiceTranscriptLimit  = 64 << 10
	voiceAudioLimit       = 20 << 20
	voiceMicrophoneLimit  = 10 << 20
	voiceMicrophoneTime   = 60 * time.Second
	voiceJobWallTime      = 180 * time.Second
	voiceFileTTL          = 10 * time.Minute
	voiceDefaultSpeaker   = 3
	voiceMaxSpeaker       = 52
	voiceKokoroDataDir    = "espeak-ng-data"
)

// The Kokoro bundle layout published by sherpa-onnx. Every file is verified by
// digest before it is used.
var voiceKokoroFiles = []string{"model.onnx", "voices.bin", "tokens.txt", "lexicon-us-en.txt", "lexicon-zh.txt"}

type voiceSettings struct {
	Revision         int               `json:"revision"`
	STTExecutable    string            `json:"sttExecutable,omitempty"`
	STTModelPath     string            `json:"sttModelPath,omitempty"`
	TTSExecutable    string            `json:"ttsExecutable,omitempty"`
	TTSModelDirectory string           `json:"ttsModelDirectory,omitempty"`
	FFmpegExecutable string            `json:"ffmpegExecutable,omitempty"`
	SpeakerID        int               `json:"speakerId"`
	AutoReadReplies  bool              `json:"autoReadReplies"`
	Manifest         map[string]string `json:"manifest,omitempty"`
}

type voiceJob struct {
	ID                 string    `json:"id"`
	Kind               string    `json:"kind"`
	SessionOwner       string    `json:"sessionOwner"`
	State              string    `json:"state"`
	ExpiresAt          time.Time `json:"expiresAt"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
	Transcript         string    `json:"transcript,omitempty"`
	Draft              bool      `json:"draft"`
	AutoSend           bool      `json:"autoSend"`
	Language           string    `json:"language,omitempty"`
	SpeakerID          int       `json:"speakerId,omitempty"`
	AudioFile          string    `json:"audioFile,omitempty"`
	AudioBytes         int64     `json:"audioBytes,omitempty"`
	AudioAvailable     bool      `json:"audioAvailable"`
	SourceAttachmentID string    `json:"sourceAttachmentId,omitempty"`
	Error              string    `json:"error,omitempty"`
}

// voiceExecutor is implemented only by the confined native runtime in production.
type voiceExecutor interface {
	Transcribe(ctx context.Context, audio []byte) (string, error)
	Synthesize(ctx context.Context, text string, speakerID int) ([]byte, error)
}

func (r *Relay) voiceDirectory() (string, error) {
	base := filepath.Dir(r.lock.Name())
	dir := filepath.Join(base, "voice")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", errors.New("voice directory must be private")
	}
	return dir, nil
}

// ---- configuration -------------------------------------------------------

func voiceValidPath(path string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	if strings.ContainsAny(path, "\x00\n") || strings.Contains(path, "/../") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// voiceValidateExecutable accepts only an absolute, existing, regular,
// executable file. A directory, a script path with a relative segment or a
// missing binary is rejected before any configuration is stored.
func voiceValidateExecutable(path string) (string, error) {
	if !voiceValidPath(path) {
		return "", model.Err(400, "voice executable must be an absolute canonical path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", model.Err(400, "voice executable is missing: "+path)
		}
		return "", model.Err(400, "voice executable cannot be resolved: "+path)
	}
	if !filepath.IsAbs(resolved) {
		return "", model.Err(400, "voice executable must resolve to an absolute path")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", model.Err(400, "voice executable cannot be inspected: "+path)
	}
	if !info.Mode().IsRegular() {
		return "", model.Err(400, "voice executable must be a regular file: "+path)
	}
	if info.Mode().Perm()&0111 == 0 {
		return "", model.Err(400, "voice executable is not executable: "+path)
	}
	if info.Size() == 0 {
		return "", model.Err(400, "voice executable is empty: "+path)
	}
	return resolved, nil
}

func voiceValidateFile(path string) (string, error) {
	if !voiceValidPath(path) {
		return "", model.Err(400, "voice model must be an absolute canonical path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", model.Err(400, "voice model is missing: "+path)
		}
		return "", model.Err(400, "voice model cannot be resolved: "+path)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", model.Err(400, "voice model cannot be inspected: "+path)
	}
	if !info.Mode().IsRegular() {
		return "", model.Err(400, "voice model must be a regular file: "+path)
	}
	if info.Size() == 0 {
		return "", model.Err(400, "voice model is empty: "+path)
	}
	return resolved, nil
}

func voiceValidateDirectory(path string) (string, error) {
	if !voiceValidPath(path) {
		return "", model.Err(400, "voice model directory must be an absolute canonical path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", model.Err(400, "voice model directory is missing: "+path)
		}
		return "", model.Err(400, "voice model directory cannot be resolved: "+path)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", model.Err(400, "voice model directory cannot be inspected: "+path)
	}
	if !info.IsDir() {
		return "", model.Err(400, "voice model directory must be a directory: "+path)
	}
	return resolved, nil
}

func voiceFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<30 {
		return "", errors.New("voice manifest entry must be a regular file no larger than 4 GiB")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(file, 4<<30)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// voiceDirectoryDigest hashes a directory tree so a changed model bundle is
// detected instead of silently used.
func voiceDirectoryDigest(path string) (string, error) {
	entries := []string{}
	err := filepath.WalkDir(path, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("voice model directory contains a non-regular entry: %s", current)
		}
		relative, err := filepath.Rel(path, current)
		if err != nil {
			return err
		}
		entries = append(entries, relative)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	hash := sha256.New()
	for _, relative := range entries {
		digest, err := voiceFileDigest(filepath.Join(path, relative))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00%s\x00", relative, digest)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// voiceBuildManifest records the exact bytes the operator reviewed. A changed
// file after configuration makes the affected capability unavailable.
func voiceBuildManifest(settings voiceSettings) (map[string]string, error) {
	manifest := map[string]string{}
	add := func(key, path string, directory bool) error {
		var digest string
		var err error
		if directory {
			digest, err = voiceDirectoryDigest(path)
		} else {
			digest, err = voiceFileDigest(path)
		}
		if err != nil {
			return err
		}
		manifest[key] = digest
		return nil
	}
	if settings.STTExecutable != "" {
		if err := add("sttExecutable", settings.STTExecutable, false); err != nil {
			return nil, err
		}
	}
	if settings.STTModelPath != "" {
		if err := add("sttModelPath", settings.STTModelPath, false); err != nil {
			return nil, err
		}
	}
	if settings.TTSExecutable != "" {
		if err := add("ttsExecutable", settings.TTSExecutable, false); err != nil {
			return nil, err
		}
	}
	if settings.FFmpegExecutable != "" {
		if err := add("ffmpegExecutable", settings.FFmpegExecutable, false); err != nil {
			return nil, err
		}
	}
	if settings.TTSModelDirectory != "" {
		for _, name := range voiceKokoroFiles {
			if err := add("kokoro/"+name, filepath.Join(settings.TTSModelDirectory, name), false); err != nil {
				return nil, err
			}
		}
		if err := add("kokoroBundle", settings.TTSModelDirectory, true); err != nil {
			return nil, err
		}
	}
	return manifest, nil
}

func (r *Relay) voiceSettingsLocked() (voiceSettings, error) {
	var settings voiceSettings
	var body string
	var revision int
	err := r.db.QueryRow("SELECT revision,body FROM voice_settings WHERE id=1").Scan(&revision, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if err = json.Unmarshal([]byte(body), &settings); err != nil {
		return settings, err
	}
	settings.Revision = revision
	return settings, nil
}

func (r *Relay) voiceSettings() (voiceSettings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.voiceSettingsLocked()
}

// voiceRuntimeCoverage verifies that the configured binaries are part of the
// operator reviewed confined runtime manifest. A binary outside the manifest
// would not be visible inside the worker, so the capability stays unavailable.
func (r *Relay) voiceRuntimeCoverage(settings voiceSettings) []string {
	missing := []string{}
	config, _, err := r.runtimeConfig()
	if err != nil || len(config.Executables) == 0 {
		return []string{"configure the confined runtime with runtime.configure before using local voice"}
	}
	listed := map[string]bool{}
	for _, path := range config.Executables {
		listed[path] = true
	}
	for _, entry := range []struct{ role, path string }{
		{"whisper-cli", settings.STTExecutable},
		{"sherpa-onnx-offline-tts", settings.TTSExecutable},
		{"ffmpeg", settings.FFmpegExecutable},
	} {
		if entry.path == "" {
			continue
		}
		if !listed[entry.path] {
			missing = append(missing, "add "+entry.path+" to runtime.configure executables so the confined worker can run it")
		}
	}
	return missing
}

// voiceVerifyManifest re-checks every recorded digest.
func voiceVerifyManifest(settings voiceSettings) []string {
	actual, err := voiceBuildManifest(settings)
	if err != nil {
		return []string{"voice runtime/model files are unavailable: " + err.Error()}
	}
	problems := []string{}
	if len(actual) != len(settings.Manifest) {
		problems = append(problems, "voice digest manifest is incomplete; configure the installed runtime again")
	}
	for key, digest := range actual {
		if expected, ok := settings.Manifest[key]; !ok || expected != digest {
			problems = append(problems, "voice model changed after configuration: "+key)
		}
	}
	sort.Strings(problems)
	return problems
}

func voiceStatus(settings voiceSettings, problems []string) string {
	if settings.STTExecutable == "" && settings.TTSExecutable == "" {
		return "missing"
	}
	if len(problems) > 0 {
		return "failed"
	}
	return "ready"
}

// ---- jobs ----------------------------------------------------------------

func (r *Relay) voiceJobRecord(id string) (voiceJob, error) {
	var job voiceJob
	var body, state, expires string
	err := r.db.QueryRow("SELECT body,state,expires_at FROM voice_jobs WHERE id=?", id).Scan(&body, &state, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return job, model.Err(404, "voice job not found")
	}
	if err != nil {
		return job, err
	}
	if err = json.Unmarshal([]byte(body), &job); err != nil {
		return job, err
	}
	job.State = state
	if job.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
		return job, err
	}
	return job, nil
}

func (r *Relay) storeVoiceJob(job voiceJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.storeVoiceJobLocked(job)
}

// storeVoiceJobLocked persists a job while the caller already owns relay
// ownership.
func (r *Relay) storeVoiceJobLocked(job voiceJob) error {
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO voice_jobs(id,session_owner,state,expires_at,body) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,expires_at=excluded.expires_at,body=excluded.body", job.ID, job.SessionOwner, job.State, job.ExpiresAt.Format(time.RFC3339Nano), string(body)); err != nil {
		return err
	}
	if err = insertAutomationEvent(tx, "voice."+job.State, job.ID, "", map[string]any{"kind": job.Kind}); err != nil {
		return err
	}
	return tx.Commit()
}


// ---- executor ------------------------------------------------------------

// sandboxVoiceExecutor runs the configured binaries inside a confined worker
// with only the staged input present. It never inherits the daemon environment.
type sandboxVoiceExecutor struct {
	relay    *Relay
	settings voiceSettings
	config   sandbox.Config
}

func (r *Relay) voiceStageDir() (string, error) {
	dir, err := os.MkdirTemp("", "meshra-voice-")
	if err != nil {
		return "", err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// voiceRunWorker copies the staged files into a private workspace copy and runs
// one confined worker. The staged names become /workspace/<name> inside the
// worker. The returned snapshot stays open so the caller can read a produced
// file before the private copy is removed.
func (r *Relay) voiceRunWorker(ctx context.Context, config sandbox.Config, stageDir string, staged map[string]string, argv []string, timeout time.Duration) (sandbox.Result, *sandbox.Snapshot, error) {
	var result sandbox.Result
	for name, source := range staged {
		if !voiceValidPath("/workspace/" + name) {
			return result, nil, model.Err(400, "invalid staged voice input name")
		}
		target := filepath.Join(stageDir, name)
		if filepath.Clean(source) == target {
			continue
		}
		if err := copyVoiceFile(source, target); err != nil {
			return result, nil, err
		}
	}
	snapshot, err := sandbox.SnapshotWorkspace(ctx, stageDir, r.sandboxStore(), config.Limits)
	if err != nil {
		return result, nil, sandboxError(err)
	}
	result, err = sandbox.Run(ctx, config, sandbox.Request{Workspace: snapshot, Argv: argv, Kind: sandbox.Voice, Timeout: timeout})
	if result.Changes != nil {
		result.Changes.Close()
	}
	return result, snapshot, err
}

func copyVoiceFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyVoiceTree(source, target)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	if _, err = io.Copy(output, io.LimitReader(input, 4<<30)); err != nil {
		return err
	}
	return output.Sync()
}

func copyVoiceTree(source, target string) error {
	return filepath.WalkDir(source, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("voice model directory contains a non-regular entry: %s", current)
		}
		return copyVoiceFile(current, destination)
	})
}

// Transcribe normalizes the uploaded audio to 16 kHz mono PCM in a confined
// ffmpeg worker and then runs whisper.cpp on the normalized file.
func (e *sandboxVoiceExecutor) Transcribe(ctx context.Context, audio []byte) (string, error) {
	settings := e.settings
	if settings.FFmpegExecutable == "" {
		return "", model.Err(503, "configure an ffmpeg executable to decode uploaded audio before local transcription")
	}
	dir, err := e.relay.voiceStageDir()
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "input.bin")
	if err = os.WriteFile(input, audio, 0600); err != nil {
		return "", err
	}
	// Decode one audio stream only; reject overlong input rather than silently
	// transcribing a clipped recording. Protocols cannot fetch URLs or host files.
	argv := []string{settings.FFmpegExecutable, "-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file", "-i", "/workspace/input.bin", "-map", "0:a:0", "-vn", "-sn", "-dn", "-t", "60.001", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-f", "wav", "/workspace/audio.wav"}
	result, snapshot, err := e.relay.voiceRunWorker(ctx, e.config, dir, map[string]string{"input.bin": input}, argv, voiceJobWallTime)
	if snapshot != nil {
		defer snapshot.Close()
	}
	if err != nil {
		return "", sandboxError(err)
	}
	if result.ExitCode != 0 {
		return "", model.Err(503, "local audio decoding failed: "+truncate(redact(result.Stderr), 512))
	}
	normalized := filepath.Join(dir, "normalized.wav")
	if err = copyVoiceSnapshotFile(snapshot, "audio.wav", normalized); err != nil {
		return "", model.Err(503, "local audio decoding produced no normalized audio")
	}
	normalizedAudio, err := voiceReadAudioFile(normalized)
	if err != nil {
		return "", err
	}
	if err = voiceValidateWAV(normalizedAudio, true); err != nil {
		return "", model.Err(400, "microphone recording must be at most 60 seconds of decodable audio: "+err.Error())
	}
	if err = copyVoiceFile(settings.STTModelPath, filepath.Join(dir, "model.bin")); err != nil {
		return "", err
	}
	digest, err := voiceFileDigest(filepath.Join(dir, "model.bin"))
	if err != nil || digest != settings.Manifest["sttModelPath"] {
		return "", model.Err(503, "speech to text model changed while staging")
	}
	argv = []string{settings.STTExecutable, "-m", "/workspace/model.bin", "-f", "/workspace/audio.wav", "-l", "auto", "-nt"}
	staged := map[string]string{"audio.wav": normalized, "model.bin": filepath.Join(dir, "model.bin")}
	result, snapshot, err = e.relay.voiceRunWorker(ctx, e.config, dir, staged, argv, voiceJobWallTime)
	if snapshot != nil {
		defer snapshot.Close()
	}
	if err != nil {
		return "", sandboxError(err)
	}
	if result.ExitCode != 0 {
		return "", model.Err(503, "local speech to text failed: "+truncate(redact(result.Stderr), 512))
	}
	transcript := strings.TrimSpace(result.Stdout)
	if transcript == "" {
		return "", model.Err(503, "local speech to text produced no transcript")
	}
	if len(transcript) > voiceTranscriptLimit || !utf8.ValidString(transcript) {
		return "", model.Err(503, "local speech to text produced an invalid or oversized transcript")
	}
	return transcript, nil
}

// Synthesize runs the sherpa-onnx Kokoro offline command with fixed arguments
// and the user text as one argument. There is no shell and no cloud fallback.
func (e *sandboxVoiceExecutor) Synthesize(ctx context.Context, text string, speakerID int) ([]byte, error) {
	settings := e.settings
	bundle := "kokoro"
	dir, err := e.relay.voiceStageDir()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err = copyVoiceTree(settings.TTSModelDirectory, filepath.Join(dir, bundle)); err != nil {
		return nil, err
	}
	digest, err := voiceDirectoryDigest(filepath.Join(dir, bundle))
	if err != nil || digest != settings.Manifest["kokoroBundle"] {
		return nil, model.Err(503, "Kokoro bundle changed while staging")
	}
	staged := map[string]string{}
	argv := []string{
		settings.TTSExecutable,
		"--kokoro-model=/workspace/" + bundle + "/model.onnx",
		"--kokoro-voices=/workspace/" + bundle + "/voices.bin",
		"--kokoro-tokens=/workspace/" + bundle + "/tokens.txt",
		"--kokoro-data-dir=/workspace/" + bundle + "/" + voiceKokoroDataDir,
		"--kokoro-lexicon=/workspace/" + bundle + "/lexicon-us-en.txt,/workspace/" + bundle + "/lexicon-zh.txt",
		"--kokoro-dict-dir=/workspace/" + bundle + "/dict",
		"--num-threads=2",
		fmt.Sprintf("--sid=%d", speakerID),
		"--output-filename=/workspace/out.wav",
		"--",
		text,
	}
	result, snapshot, err := e.relay.voiceRunWorker(ctx, e.config, dir, staged, argv, voiceJobWallTime)
	if snapshot != nil {
		defer snapshot.Close()
	}
	if err != nil {
		return nil, sandboxError(err)
	}
	if result.ExitCode != 0 {
		return nil, model.Err(503, "local text to speech failed: "+truncate(redact(result.Stderr), 512))
	}
	file, _, err := snapshot.OpenRegular("out.wav")
	if err != nil {
		return nil, model.Err(503, "local text to speech produced no audio")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, model.Err(503, "local text to speech produced an empty audio file")
	}
	if info.Size() > voiceAudioLimit {
		return nil, model.Err(413, "synthesized audio exceeds 20 MiB")
	}
	audio := make([]byte, info.Size())
	if _, err = io.ReadFull(file, audio); err != nil {
		return nil, err
	}
	if err = voiceValidateWAV(audio, false); err != nil {
		return nil, model.Err(503, "local text to speech produced invalid WAV audio")
	}
	return audio, nil
}

// copyVoiceSnapshotFile copies one produced file out of the private copy.
func copyVoiceSnapshotFile(snapshot *sandbox.Snapshot, name, target string) error {
	if snapshot == nil {
		return errors.New("private copy is unavailable")
	}
	file, _, err := snapshot.OpenRegular(name)
	if err != nil {
		return err
	}
	defer file.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	if _, err = io.Copy(output, io.LimitReader(file, voiceAudioLimit+1)); err != nil {
		return err
	}
	return output.Sync()
}

// ---- language ------------------------------------------------------------

// voiceLanguage classifies the requested speech text. The configured Kokoro
// bundle carries English and Chinese only; anything else is reported instead
// of being spoken with the wrong voice.
func voiceLanguage(text string) (string, error) {
	language := "en"
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			language = "zh"
		case unicode.IsLetter(r) && !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z'):
			return "", model.Err(400, "the configured Kokoro bundle supports English and Chinese only; the requested script needs another local model")
		}
	}
	return language, nil
}

// ---- operations ----------------------------------------------------------

type voiceConfigureRequest struct {
	ExpectedRevision int    `json:"expectedRevision"`
	STTExecutable    string `json:"sttExecutable"`
	STTModelPath     string `json:"sttModelPath"`
	TTSExecutable    string `json:"ttsExecutable"`
	TTSModelDirectory string `json:"ttsModelDirectory"`
	FFmpegExecutable string `json:"ffmpegExecutable"`
	SpeakerID        *int   `json:"speakerId"`
	AutoReadReplies  bool   `json:"autoReadReplies"`
}

type voiceTextRequest struct {
	Text      string `json:"text"`
	SpeakerID *int   `json:"speakerId"`
}

type voiceAttachmentRequest struct {
	AudioAttachmentID string `json:"audioAttachmentId"`
}

type voiceJobRequest struct {
	ID string `json:"id"`
}

func (r *Relay) voiceOperation(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch operation {
	case "voice.configure":
		return r.voiceConfigure(p, raw)
	case "voice.status":
		return r.voiceStatusOperation(p)
	case "voice.transcribe":
		return r.voiceTranscribe(ctx, p, raw)
	case "voice.synthesize":
		return r.voiceSynthesize(ctx, p, raw)
	case "voice.job":
		return r.voiceJobOperation(ctx, p, raw)
	case "voice.cancel":
		return r.voiceCancel(ctx, p, raw)
	}
	return nil, model.Err(404, "unknown voice operation")
}

func (r *Relay) voiceConfigure(p model.Principal, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	var q voiceConfigureRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	settings := voiceSettings{SpeakerID: voiceDefaultSpeaker, AutoReadReplies: q.AutoReadReplies}
	if q.SpeakerID != nil {
		settings.SpeakerID = *q.SpeakerID
	}
	if q.STTExecutable != "" {
		resolved, err := voiceValidateExecutable(q.STTExecutable)
		if err != nil {
			return nil, err
		}
		settings.STTExecutable = resolved
	}
	if q.STTModelPath != "" {
		resolved, err := voiceValidateFile(q.STTModelPath)
		if err != nil {
			return nil, err
		}
		settings.STTModelPath = resolved
	}
	if q.TTSExecutable != "" {
		resolved, err := voiceValidateExecutable(q.TTSExecutable)
		if err != nil {
			return nil, err
		}
		settings.TTSExecutable = resolved
	}
	if q.FFmpegExecutable != "" {
		resolved, err := voiceValidateExecutable(q.FFmpegExecutable)
		if err != nil {
			return nil, err
		}
		settings.FFmpegExecutable = resolved
	}
	if q.TTSModelDirectory != "" {
		resolved, err := voiceValidateDirectory(q.TTSModelDirectory)
		if err != nil {
			return nil, err
		}
		settings.TTSModelDirectory = resolved
		for _, name := range voiceKokoroFiles {
			if _, err := voiceValidateFile(filepath.Join(resolved, name)); err != nil {
				return nil, model.Err(400, "the configured Kokoro bundle is missing "+name)
			}
		}
		for _, name := range []string{voiceKokoroDataDir, "dict"} {
			if _, err := voiceValidateDirectory(filepath.Join(resolved, name)); err != nil {
				return nil, model.Err(400, "the configured Kokoro bundle is missing the "+name+" directory")
			}
		}
	}
	if (settings.STTExecutable == "") != (settings.STTModelPath == "") {
		return nil, model.Err(400, "configure both a speech to text executable and its model, or neither")
	}
	if settings.STTExecutable == "" && settings.TTSExecutable == "" {
		return nil, model.Err(400, "configure a local speech to text or text to speech runtime")
	}
	if settings.STTExecutable != "" && settings.FFmpegExecutable == "" {
		return nil, model.Err(400, "configure an ffmpeg executable to decode uploaded audio before transcription")
	}
	if settings.TTSExecutable != "" && settings.TTSModelDirectory == "" {
		return nil, model.Err(400, "configure the Kokoro model directory for local speech synthesis")
	}
	if settings.SpeakerID < 0 || settings.SpeakerID > voiceMaxSpeaker {
		return nil, model.Err(400, "the Kokoro v1_0 speaker id must be between 0 and 52")
	}
	manifest, err := voiceBuildManifest(settings)
	if err != nil {
		return nil, model.Err(400, "the configured voice runtime cannot be hashed: "+err.Error())
	}
	settings.Manifest = manifest
	return r.commitVoiceConfiguration(q.ExpectedRevision, settings)
}

func (r *Relay) voiceStatusOperation(p model.Principal) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	settings, err := r.voiceSettings()
	if err != nil {
		return nil, err
	}
	problems := []string{}
	if settings.STTExecutable == "" && settings.TTSExecutable == "" {
		problems = append(problems, "voice.configure has not recorded a local voice runtime")
	}
	problems = append(problems, voiceVerifyManifest(settings)...)
	problems = append(problems, r.voiceRuntimeCoverage(settings)...)
	return map[string]any{
		"revision":        settings.Revision,
		"configured":      settings.STTExecutable != "" || settings.TTSExecutable != "",
		"state":           voiceStatus(settings, problems),
		"prerequisites":   problems,
		"speakerId":       settings.SpeakerID,
		"autoReadReplies": settings.AutoReadReplies,
		"stt":             map[string]any{"configured": settings.STTExecutable != "", "model": settings.STTModelPath, "maxRecordingSeconds": 60, "maxUploadBytes": voiceMicrophoneLimit},
		"tts":             map[string]any{"configured": settings.TTSExecutable != "", "modelDirectory": settings.TTSModelDirectory, "bundle": "kokoro-multi-lang-v1_0", "languages": []string{"en", "zh"}, "speakerMinimum": 0, "speakerMaximum": voiceMaxSpeaker, "defaultSpeakerName": "af_heart"},
	}, nil
}

// voiceReady resolves the executor for one operation. It fails closed when the
// configured runtime, its models or the confined runtime are unavailable.
func (r *Relay) voiceReady(settings voiceSettings, needSTT, needTTS bool) (voiceExecutor, sandbox.Config, error) {
	var config sandbox.Config
	if settings.STTExecutable == "" && settings.TTSExecutable == "" {
		return nil, config, model.Err(503, "local voice is not configured; run voice.configure first")
	}
	if needSTT && settings.STTExecutable == "" {
		return nil, config, model.Err(503, "local speech to text is not configured")
	}
	if needTTS && settings.TTSExecutable == "" {
		return nil, config, model.Err(503, "local text to speech is not configured")
	}
	if problems := voiceVerifyManifest(settings); len(problems) > 0 {
		return nil, config, model.Err(503, "local voice runtime is unavailable: "+strings.Join(problems, "; "))
	}
	// The injected executor exists only for tests inside this package. It never
	// relaxes the recorded model digests.
	if executor := r.injectedVoiceExecutor(); executor != nil {
		return executor, config, nil
	}
	if problems := r.voiceRuntimeCoverage(settings); len(problems) > 0 {
		return nil, config, model.Err(503, "local voice runtime is unavailable: "+strings.Join(problems, "; "))
	}
	config, _, err := r.runtimeConfig()
	if err != nil {
		return nil, config, err
	}
	if status := sandbox.Check(context.Background(), config); status.State != "ready" {
		return nil, config, model.Err(503, "confined runtime "+status.State+": "+strings.Join(status.Prerequisites, "; "))
	}
	return &sandboxVoiceExecutor{relay: r, settings: settings, config: config}, config, nil
}

func (r *Relay) voiceTranscribe(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	var q voiceAttachmentRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.AudioAttachmentID == "" {
		return nil, model.Err(400, "audioAttachmentId is required")
	}
	settings, err := r.voiceSettings()
	if err != nil {
		return nil, err
	}
	executor, _, err := r.voiceReady(settings, true, false)
	if err != nil {
		return nil, err
	}
	attachment, reader, err := r.ReadAttachment(ctx, p, q.AudioAttachmentID)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	audio, err := io.ReadAll(io.LimitReader(reader, voiceMicrophoneLimit+1))
	if err != nil {
		return nil, err
	}
	if len(audio) == 0 || len(audio) > voiceMicrophoneLimit {
		return nil, model.Err(413, "microphone recording must be nonempty and at most 10 MiB")
	}
	job := voiceJob{Kind: "transcribe", SessionOwner: p.ConsoleSessionID, Draft: true, SourceAttachmentID: attachment.ID}
	return r.startVoiceJob(ctx, p, settings, job, func(workerContext context.Context) (string, []byte, error) {
		transcript, err := executor.Transcribe(workerContext, audio)
		clear(audio)
		return transcript, nil, err
	})
}

func (r *Relay) voiceSynthesize(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	var q voiceTextRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.Text == "" {
		return nil, model.Err(400, "text is required")
	}
	if len(q.Text) > voiceTextLimit {
		return nil, model.Err(413, "synthesis text exceeds 8 KiB")
	}
	if !utf8.ValidString(q.Text) || strings.ContainsRune(q.Text, 0) {
		return nil, model.Err(400, "synthesis text must be valid UTF-8 without NUL")
	}
	settings, err := r.voiceSettings()
	if err != nil {
		return nil, err
	}
	speaker := settings.SpeakerID
	if q.SpeakerID != nil {
		speaker = *q.SpeakerID
	}
	if speaker < 0 || speaker > voiceMaxSpeaker {
		return nil, model.Err(400, "the Kokoro v1_0 speaker id must be between 0 and 52")
	}
	language, err := voiceLanguage(q.Text)
	if err != nil {
		return nil, err
	}
	executor, _, err := r.voiceReady(settings, false, true)
	if err != nil {
		return nil, err
	}
	job := voiceJob{Kind: "synthesize", SessionOwner: p.ConsoleSessionID, Language: language, SpeakerID: speaker}
	return r.startVoiceJob(ctx, p, settings, job, func(workerContext context.Context) (string, []byte, error) {
		audio, err := executor.Synthesize(workerContext, q.Text, speaker)
		return "", audio, err
	})
}

func (r *Relay) voiceJobOperation(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	var q voiceJobRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ID == "" {
		return nil, model.Err(400, "id is required")
	}
	return r.readVoiceJob(ctx, p, q.ID)
}

func (r *Relay) voiceCancel(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	var q voiceJobRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ID == "" {
		return nil, model.Err(400, "id is required")
	}
	return r.cancelVoiceJob(ctx, p, q.ID)
}

// VoiceAudio returns the synthesized audio of one job to its owning console
// session. Expired, cancelled or foreign jobs have no audio.
func (r *Relay) VoiceAudio(ctx context.Context, p model.Principal, id string) ([]byte, error) {
	return r.readVoiceAudio(ctx, p, id)
}
