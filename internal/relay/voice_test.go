package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

// fakeVoiceExecutor replaces the confined worker so the operation rules can be
// checked without whisper, sherpa or ffmpeg installed.
type fakeVoiceExecutor struct {
	transcript string
	audio      []byte
	err        error
}

func (f *fakeVoiceExecutor) Transcribe(ctx context.Context, audio []byte) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.transcript, nil
}

func (f *fakeVoiceExecutor) Synthesize(ctx context.Context, text string, speakerID int) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return bytes.Clone(f.audio), nil
}

func voiceTestExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func voiceTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("model-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// voiceTestBundle builds the reviewed Kokoro bundle layout.
func voiceTestBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range voiceKokoroFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("kokoro"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(dir, voiceKokoroDataDir)
	if err := os.MkdirAll(data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "phondata"), []byte("espeak"), 0644); err != nil {
		t.Fatal(err)
	}
	dictionary := filepath.Join(dir, "dict")
	if err := os.MkdirAll(dictionary, 0755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// voiceTestWAV builds a valid mono 16 kHz 16-bit PCM WAV.
func voiceTestWAV() []byte {
	data := make([]byte, 3200)
	out := make([]byte, 0, 44+len(data))
	out = append(out, "RIFF"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(36+len(data)))
	out = append(out, "WAVEfmt "...)
	out = binary.LittleEndian.AppendUint32(out, 16)
	out = binary.LittleEndian.AppendUint16(out, 1)
	out = binary.LittleEndian.AppendUint16(out, 1)
	out = binary.LittleEndian.AppendUint32(out, 16000)
	out = binary.LittleEndian.AppendUint32(out, 32000)
	out = binary.LittleEndian.AppendUint16(out, 2)
	out = binary.LittleEndian.AppendUint16(out, 16)
	out = append(out, "data"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(data)))
	return append(out, data...)
}

// voiceSettled polls voice.job until the asynchronous worker leaves pending/running.
func voiceSettled(t *testing.T, r *Relay, p model.Principal, job voiceJob) voiceJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		value, err := voiceCall(t, r, p, "voice.job", map[string]any{"id": job.ID})
		if err != nil {
			t.Fatal(err)
		}
		job = value.(voiceJob)
		if job.State != "pending" && job.State != "running" {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("voice job never settled: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func voiceCall(t *testing.T, r *Relay, p model.Principal, op string, q any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return r.voiceOperation(context.Background(), p, op, raw)
}

func voiceStatusView(t *testing.T, r *Relay) map[string]any {
	t.Helper()
	value, err := voiceCall(t, r, admin, "voice.status", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

func TestVoiceConfigureRejectsMissingOrUnsafeExecutables(t *testing.T) {
	r, _ := openTest(t, nil)
	automationTestSchema(t, r, "018.sql")
	dir := t.TempDir()
	missing := filepath.Join(dir, "never-installed-whisper-cli")
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"missing executable", map[string]any{"sttExecutable": missing, "sttModelPath": voiceTestFile(t, dir, "model.bin"), "ffmpegExecutable": voiceTestExecutable(t, dir, "ffmpeg")}, "missing"},
		{"relative executable", map[string]any{"sttExecutable": "whisper-cli", "sttModelPath": voiceTestFile(t, dir, "model.bin"), "ffmpegExecutable": voiceTestExecutable(t, dir, "ffmpeg")}, "absolute"},
		{"directory executable", map[string]any{"sttExecutable": dir, "sttModelPath": voiceTestFile(t, dir, "model.bin"), "ffmpegExecutable": voiceTestExecutable(t, dir, "ffmpeg")}, "regular file"},
		{"missing model", map[string]any{"sttExecutable": voiceTestExecutable(t, dir, "whisper-cli"), "sttModelPath": filepath.Join(dir, "absent.bin"), "ffmpegExecutable": voiceTestExecutable(t, dir, "ffmpeg")}, "missing"},
		{"half configured runtime", map[string]any{"sttExecutable": voiceTestExecutable(t, dir, "whisper-cli")}, "both"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			test.payload["expectedRevision"] = 0
			_, err := voiceCall(t, r, admin, "voice.configure", test.payload)
			if err == nil {
				t.Fatal("an unsafe voice configuration was accepted")
			}
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Status != 400 {
				t.Fatalf("unexpected error %v", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not explain the problem (%s)", err.Error(), test.want)
			}
		})
	}
}

func TestVoiceStatusReportsActionablePrerequisites(t *testing.T) {
	r, _ := openTest(t, nil)
	automationTestSchema(t, r, "018.sql")
	status := voiceStatusView(t, r)
	if status["state"] != "missing" || status["configured"] != false {
		t.Fatalf("unconfigured voice is not reported as missing: %+v", status)
	}
	dir := t.TempDir()
	payload := map[string]any{
		"expectedRevision":  0,
		"sttExecutable":     voiceTestExecutable(t, dir, "whisper-cli"),
		"sttModelPath":      voiceTestFile(t, dir, "model.bin"),
		"ffmpegExecutable":  voiceTestExecutable(t, dir, "ffmpeg"),
		"ttsExecutable":     voiceTestExecutable(t, dir, "sherpa-onnx-offline-tts"),
		"ttsModelDirectory": voiceTestBundle(t),
		"speakerId":         3,
		"autoReadReplies":   false,
	}
	if _, err := voiceCall(t, r, admin, "voice.configure", payload); err != nil {
		t.Fatal(err)
	}
	status = voiceStatusView(t, r)
	if status["configured"] != true {
		t.Fatalf("configured voice is not reported: %+v", status)
	}
	prerequisites, _ := status["prerequisites"].([]string)
	if len(prerequisites) == 0 || !strings.Contains(strings.Join(prerequisites, "; "), "runtime.configure") {
		t.Fatalf("missing runtime prerequisite is not actionable: %+v", prerequisites)
	}
	// A changed model after configuration is detected.
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	status = voiceStatusView(t, r)
	prerequisites, _ = status["prerequisites"].([]string)
	if !strings.Contains(strings.Join(prerequisites, "; "), "changed after configuration") {
		t.Fatalf("a changed model was not detected: %+v", prerequisites)
	}
}

func TestVoiceTranscribeProducesDraftWithoutSendingOrApproving(t *testing.T) {
	r, _ := openTest(t, nil)
	automationTestSchema(t, r, "018.sql")
	r.SetConsoleSessionValidator(func(id string) bool { return id == "voice-session" })
	console := model.Principal{Admin: true, ConsoleSessionID: "voice-session"}
	dir := t.TempDir()
	payload := map[string]any{
		"expectedRevision":  0,
		"sttExecutable":     voiceTestExecutable(t, dir, "whisper-cli"),
		"sttModelPath":      voiceTestFile(t, dir, "model.bin"),
		"ffmpegExecutable":  voiceTestExecutable(t, dir, "ffmpeg"),
		"speakerId":         3,
		"autoReadReplies":   false,
	}
	if _, err := voiceCall(t, r, admin, "voice.configure", payload); err != nil {
		t.Fatal(err)
	}
	r.setVoiceExecutor(&fakeVoiceExecutor{transcript: "hello from the local microphone"})
	conversation := randomID()
	if _, err := r.db.Exec("INSERT INTO conversations VALUES(?,?,?,?)", conversation, "Voice test", "admin", now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	audio := []byte("RIFF----WAVEfmt ")
	attachment, err := r.UploadAttachment(context.Background(), console, conversation, "voice.wav", "audio/wav", bytes.NewReader(audio), int64(len(audio)))
	if err != nil {
		t.Fatal(err)
	}
	value, err := voiceCall(t, r, console, "voice.transcribe", map[string]any{"audioAttachmentId": attachment.ID})
	if err != nil {
		t.Fatal(err)
	}
	job := voiceSettled(t, r, console, value.(voiceJob))
	if job.State != "completed" || job.Transcript != "hello from the local microphone" {
		t.Fatalf("unexpected transcription job: %+v", job)
	}
	if !job.Draft || job.AutoSend {
		t.Fatalf("the transcript is not an inert draft: %+v", job)
	}
	// The transcript never becomes a task and never approves an action.
	var tasks int
	if err = r.db.QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 {
		t.Fatalf("the transcript created %d tasks", tasks)
	}
	var approvals int
	if err = r.db.QueryRow("SELECT count(*) FROM tool_approvals").Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if approvals != 0 {
		t.Fatalf("the transcript created %d approvals", approvals)
	}
	// A token without a live console session may not transcribe.
	if _, err = voiceCall(t, r, admin, "voice.transcribe", map[string]any{"audioAttachmentId": attachment.ID}); err == nil {
		t.Fatal("a bearer administrator transcribed without a console session")
	}
}

func TestVoiceJobExpiryAndCancellationRemoveAudio(t *testing.T) {
	r, _ := openTest(t, nil)
	automationTestSchema(t, r, "018.sql")
	r.SetConsoleSessionValidator(func(id string) bool { return id == "voice-session" })
	console := model.Principal{Admin: true, ConsoleSessionID: "voice-session"}
	dir := t.TempDir()
	payload := map[string]any{
		"expectedRevision":  0,
		"ttsExecutable":     voiceTestExecutable(t, dir, "sherpa-onnx-offline-tts"),
		"ttsModelDirectory": voiceTestBundle(t),
		"speakerId":         3,
		"autoReadReplies":   false,
	}
	if _, err := voiceCall(t, r, admin, "voice.configure", payload); err != nil {
		t.Fatal(err)
	}
	audio := voiceTestWAV()
	r.setVoiceExecutor(&fakeVoiceExecutor{audio: audio})
	value, err := voiceCall(t, r, console, "voice.synthesize", map[string]any{"text": "local speech"})
	if err != nil {
		t.Fatal(err)
	}
	job := voiceSettled(t, r, console, value.(voiceJob))
	if job.State != "completed" || !job.AudioAvailable || job.AudioBytes != int64(len(audio)) {
		t.Fatalf("unexpected synthesis job: %+v", job)
	}
	played, err := r.VoiceAudio(context.Background(), console, job.ID)
	if err != nil || !bytes.Equal(played, audio) {
		t.Fatalf("the synthesized audio is not available: %v", err)
	}
	// Cancellation removes the audio.
	cancelled, err := voiceCall(t, r, console, "voice.cancel", map[string]any{"id": job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.(voiceJob).State != "cancelled" {
		t.Fatalf("the job was not cancelled: %+v", cancelled)
	}
	if _, err = r.VoiceAudio(context.Background(), console, job.ID); err == nil {
		t.Fatal("the cancelled job still served audio")
	}
	// Expiry removes the audio as well.
	value, err = voiceCall(t, r, console, "voice.synthesize", map[string]any{"text": "local speech"})
	if err != nil {
		t.Fatal(err)
	}
	expiring := voiceSettled(t, r, console, value.(voiceJob))
	if _, err = r.VoiceAudio(context.Background(), console, expiring.ID); err != nil {
		t.Fatalf("the fresh job has no audio: %v", err)
	}
	r.mu.Lock()
	_, err = r.db.Exec("UPDATE voice_jobs SET expires_at=? WHERE id=?", now().Add(-time.Minute).Format(time.RFC3339Nano), expiring.ID)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	expired, err := voiceCall(t, r, console, "voice.job", map[string]any{"id": expiring.ID})
	if err != nil {
		t.Fatal(err)
	}
	if expired.(voiceJob).State != "expired" {
		t.Fatalf("the expired job is still active: %+v", expired)
	}
	if _, err = r.VoiceAudio(context.Background(), console, expiring.ID); err == nil {
		t.Fatal("the expired job still served audio")
	}
	// Another console session may not read the audio of a foreign job.
	other := model.Principal{Admin: true, ConsoleSessionID: "other-session"}
	r.SetConsoleSessionValidator(func(id string) bool { return id == "voice-session" || id == "other-session" })
	if _, err = r.VoiceAudio(context.Background(), other, expiring.ID); err == nil {
		t.Fatal("a foreign console session read a voice job")
	}
}

func TestVoiceSynthesisRejectsUnsupportedLanguage(t *testing.T) {
	r, _ := openTest(t, nil)
	automationTestSchema(t, r, "018.sql")
	r.SetConsoleSessionValidator(func(id string) bool { return id == "voice-session" })
	console := model.Principal{Admin: true, ConsoleSessionID: "voice-session"}
	dir := t.TempDir()
	payload := map[string]any{
		"expectedRevision":  0,
		"ttsExecutable":     voiceTestExecutable(t, dir, "sherpa-onnx-offline-tts"),
		"ttsModelDirectory": voiceTestBundle(t),
		"speakerId":         3,
		"autoReadReplies":   false,
	}
	if _, err := voiceCall(t, r, admin, "voice.configure", payload); err != nil {
		t.Fatal(err)
	}
	r.setVoiceExecutor(&fakeVoiceExecutor{audio: voiceTestWAV()})
	_, err := voiceCall(t, r, console, "voice.synthesize", map[string]any{"text": "こんにちは"})
	if err == nil || !strings.Contains(err.Error(), "English and Chinese") {
		t.Fatalf("an unsupported language was synthesized: %v", err)
	}
	// Chinese is supported by the configured bundle.
	if _, err = voiceCall(t, r, console, "voice.synthesize", map[string]any{"text": "本地语音"}); err != nil {
		t.Fatalf("Chinese synthesis was refused: %v", err)
	}
}
