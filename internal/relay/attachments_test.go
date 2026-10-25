package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

func attachmentTestUpload(t *testing.T, r *Relay, conversation, name, mediaType string, data []byte) model.Attachment {
	t.Helper()
	a, err := r.UploadAttachment(context.Background(), admin, conversation, name, mediaType, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func attachmentTestRead(t *testing.T, r *Relay, p model.Principal, id string) []byte {
	t.Helper()
	_, body, err := r.ReadAttachment(context.Background(), p, id)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func attachmentTestConversation(t *testing.T, r *Relay, agents ...string) model.Conversation {
	t.Helper()
	v, err := r.Collaborate(admin, "conversation.create", model.CollaborationRequest{Title: "Attachment scope"})
	if err != nil {
		t.Fatal(err)
	}
	c := v.(model.Conversation)
	for _, id := range agents {
		if _, err = r.Collaborate(admin, "conversation.addParticipant", model.CollaborationRequest{ID: c.ID, AgentID: id}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func attachmentConsole(r *Relay) model.Principal {
	r.SetConsoleSessionValidator(func(id string) bool { return id == "attachment-session" })
	return model.Principal{Admin: true, ConsoleSessionID: "attachment-session"}
}

func TestAttachmentBinaryRoundTripPrivateDraftIntegrityAndRemoval(t *testing.T) {
	r, dir := openTest(t, nil)
	data := []byte{0, 0xff, 0xfe, 'x', '\r', '\n', 0, 0x80}
	a := attachmentTestUpload(t, r, "", `..\../outside\unsafe`+"\x00"+`.bin`, "application/octet-stream", data)
	if a.Name != "unsafe.bin" || a.Owner != "admin" || !validAttachmentID(a.ID) || a.Size != int64(len(data)) {
		t.Fatalf("bad metadata: %+v", a)
	}
	hash := sha256.Sum256(data)
	if a.SHA256 != hex.EncodeToString(hash[:]) || !bytes.Equal(attachmentTestRead(t, r, admin, a.ID), data) {
		t.Fatal("upload bytes or digest changed")
	}
	info, err := os.Stat(filepath.Join(dir, "attachments", a.ID))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("blob permissions: %v, %v", info, err)
	}
	_, body, err := r.ReadAttachment(context.Background(), model.Principal{AgentID: "codex:unrelated"}, a.ID)
	requireStatus(t, err, 403)
	if body != nil {
		t.Fatal("denied read returned bytes")
	}
	_, _, err = r.ReadAttachment(context.Background(), admin, "../admin.token")
	requireStatus(t, err, 400)
	if err = os.WriteFile(filepath.Join(dir, "attachments", a.ID), bytes.Repeat([]byte{'z'}, len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	_, body, err = r.ReadAttachment(context.Background(), admin, a.ID)
	requireStatus(t, err, 500)
	if body != nil {
		t.Fatal("corrupt storage returned a reader")
	}
	payload, _ := json.Marshal(map[string]any{"id": a.ID, "confirm": true})
	_, err = r.attachmentOperation(admin, "attachments.remove", payload)
	requireStatus(t, err, 403)
	console := attachmentConsole(r)
	v, err := r.attachmentOperation(console, "attachments.remove", payload)
	if err != nil || !v.(model.Attachment).Unavailable {
		t.Fatalf("remove: %v, %v", v, err)
	}
	_, _, err = r.ReadAttachment(context.Background(), admin, a.ID)
	requireStatus(t, err, 404)
	if _, err = os.Stat(filepath.Join(dir, "attachments", a.ID)); !os.IsNotExist(err) {
		t.Fatalf("removed blob remains: %v", err)
	}
	r.mu.Lock()
	var refs, audit int
	err = r.db.QueryRow(`SELECT count(*) FROM attachments WHERE id=?`, a.ID).Scan(&refs)
	if err == nil {
		err = r.db.QueryRow(`SELECT count(*) FROM attachment_read_audit WHERE attachment_id=?`, a.ID).Scan(&audit)
	}
	r.mu.Unlock()
	if err != nil || refs != 1 || audit < 4 {
		t.Fatalf("provenance lost: refs=%d audit=%d err=%v", refs, audit, err)
	}
	r.RevokeConsoleSession(console.ConsoleSessionID)
	_, err = r.attachmentOperation(console, "attachments.remove", payload)
	requireStatus(t, err, 403)
}

func TestAttachmentAgentRequiresExactActiveTaskAndConversation(t *testing.T) {
	// The controlled runtime only holds a turn open. All byte reads and ACL
	// checks below use the real relay/storage; no model file-read is simulated.
	turns := make(chan model.Turn, 1)
	release := make(chan struct{})
	r, _ := openTest(t, func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
		turns <- turn
		select {
		case <-release:
			return "", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	a := createTest(t, r, "codex", "attachment-reader")
	b := createTest(t, r, "codex", "attachment-unrelated")
	c := attachmentTestConversation(t, r, a.ID, b.ID)
	other := attachmentTestConversation(t, r, a.ID)
	data := []byte("scoped attachment data\n")
	input := attachmentTestUpload(t, r, c.ID, "input.txt", "text/plain", data)
	unbound := attachmentTestUpload(t, r, c.ID, "not-this-task.bin", "application/octet-stream", []byte("not linked"))
	private := attachmentTestUpload(t, r, "", "draft.bin", "application/octet-stream", []byte("private"))
	cross := attachmentTestUpload(t, r, other.ID, "other.bin", "application/octet-stream", []byte("other conversation"))
	startTest(t, r, a.ID)
	_, err := r.Send(admin, model.SendRequest{From: "human:operator", To: a.ID, Text: "hold active turn", ConversationID: c.ID, AttachmentIDs: []string{input.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var turn model.Turn
	select {
	case turn = <-turns:
	case <-time.After(5 * time.Second):
		t.Fatal("turn not claimed")
	}
	p, err := r.Authenticate(turn.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(attachmentTestRead(t, r, p, input.ID), data) {
		t.Fatal("active task bytes changed")
	}
	for _, id := range []string{unbound.ID, private.ID, cross.ID} {
		_, body, err := r.ReadAttachment(context.Background(), p, id)
		requireStatus(t, err, 403)
		if body != nil {
			t.Fatal("non-task reference returned a reader")
		}
	}
	for _, denied := range []model.Principal{
		{AgentID: a.ID, SessionID: p.SessionID},
		{AgentID: b.ID, TaskID: p.TaskID, SessionID: p.SessionID},
		{AgentID: a.ID, TaskID: p.TaskID, SessionID: "wrong-session"},
	} {
		_, _, err := r.ReadAttachment(context.Background(), denied, input.ID)
		requireStatus(t, err, 403)
	}
	v, err := r.attachmentOperation(p, "attachments.list", json.RawMessage(`{}`))
	if err != nil || len(v.([]model.Attachment)) != 1 || v.([]model.Attachment)[0].ID != input.ID {
		t.Fatalf("agent list scope: %v, %v", v, err)
	}
	_, err = r.attachmentOperation(p, "attachments.list", json.RawMessage(`{"conversationId":"`+other.ID+`"}`))
	requireStatus(t, err, 403)
	console := attachmentConsole(r)
	_, err = r.attachmentOperation(console, "attachments.remove", json.RawMessage(`{"id":"`+input.ID+`","confirm":true}`))
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	current, lookupErr := r.task(p.TaskID)
	if lookupErr == nil {
		lookupErr = r.validateAttachmentDispatch(current)
	}
	var links int
	if lookupErr != nil {
		err = r.db.QueryRow(`SELECT count(*) FROM attachment_tasks WHERE attachment_id=? AND task_id=?`, input.ID, p.TaskID).Scan(&links)
	}
	r.mu.Unlock()
	requireStatus(t, lookupErr, 404)
	if err != nil || links != 1 {
		t.Fatalf("removal erased task link: %d %v", links, err)
	}
	close(release)
	awaitTask(t, r, p.TaskID)
	_, _, err = r.ReadAttachment(context.Background(), p, input.ID)
	requireStatus(t, err, 403)
}

func TestAttachmentBoundsDraftExpiryAndScopedPagination(t *testing.T) {
	r, _ := openTest(t, nil)
	ctx := context.Background()
	_, err := r.UploadAttachment(ctx, admin, "", "x", "application/octet-stream", strings.NewReader(""), model.MaxAttachmentBytes+1)
	requireStatus(t, err, 413)
	_, err = r.UploadAttachment(ctx, admin, "", "x", "application/octet-stream", strings.NewReader("short"), 10)
	requireStatus(t, err, 400)
	_, err = r.UploadAttachment(ctx, admin, "", "x", "not a media type", strings.NewReader("x"), 1)
	requireStatus(t, err, 400)
	_, err = r.UploadAttachment(ctx, model.Principal{AgentID: "codex:other"}, "", "x", "text/plain", strings.NewReader("x"), 1)
	requireStatus(t, err, 403)
	_, err = r.UploadAttachment(ctx, admin, "missing", "x", "text/plain", strings.NewReader("x"), 1)
	requireStatus(t, err, 404)
	c := attachmentTestConversation(t, r)
	draft := attachmentTestUpload(t, r, "", "draft.txt", "text/plain", []byte("draft"))
	bound := attachmentTestUpload(t, r, c.ID, "bound.txt", "text/plain", []byte("keep"))
	for _, id := range []string{draft.ID, bound.ID} {
		r.mu.Lock()
		_, err = r.db.Exec(`UPDATE attachments SET created_at=? WHERE id=?`, now().Add(-25*time.Hour).Format(time.RFC3339Nano), id)
		r.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, _, err = r.ReadAttachment(ctx, admin, draft.ID)
	requireStatus(t, err, 404)
	if got := attachmentTestRead(t, r, admin, bound.ID); string(got) != "keep" {
		t.Fatal("referenced attachment expired")
	}
	for _, raw := range []string{`{"unexpected":true}`, `{"limit":101}`, `{"limit":-1}`, `{"after":"../outside"}`} {
		_, err = r.attachmentOperation(admin, "attachments.list", json.RawMessage(raw))
		requireStatus(t, err, 400)
	}
	v, err := r.attachmentOperation(admin, "attachments.list", json.RawMessage(`{"limit":1}`))
	if err != nil || len(v.([]model.Attachment)) != 1 {
		t.Fatalf("pagination first: %v, %v", v, err)
	}
	first := v.([]model.Attachment)[0]
	v, err = r.attachmentOperation(admin, "attachments.list", json.RawMessage(`{"limit":1,"after":"`+first.ID+`"}`))
	if err != nil || len(v.([]model.Attachment)) != 1 || v.([]model.Attachment)[0].ID <= first.ID {
		t.Fatalf("pagination next: %v, %v", v, err)
	}
	v, err = r.attachmentOperation(admin, "attachments.list", json.RawMessage(`{"conversationId":"`+c.ID+`"}`))
	if err != nil || len(v.([]model.Attachment)) != 1 || v.([]model.Attachment)[0].ID != bound.ID {
		t.Fatalf("conversation list: %v, %v", v, err)
	}
	r.mu.Lock()
	_, err = r.validateTaskAttachments(admin, model.SendRequest{ConversationID: c.ID, AttachmentIDs: []string{bound.ID, bound.ID, bound.ID, bound.ID, bound.ID}})
	r.mu.Unlock()
	requireStatus(t, err, 400)
	// Fill only quota metadata; this tests accounting without allocating 1 GiB
	// or claiming that absent fixture blobs can be read.
	r.mu.Lock()
	tx, err := r.db.Begin()
	if err == nil {
		for i := 0; i < 41 && err == nil; i++ {
			size := model.MaxAttachmentBytes
			if i == 40 {
				size = 24 * 1024 * 1024
			}
			_, err = tx.Exec(`INSERT INTO attachments(id,sha256,size,mime,name,owner,created_at,body) VALUES(?,?,?,?,?,?,?,?)`, randomID(), "quota-fixture", size, "application/octet-stream", "quota", "admin", now().Format(time.RFC3339Nano), `{}`)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.UploadAttachment(ctx, admin, "", "over.bin", "application/octet-stream", strings.NewReader("x"), 1)
	requireStatus(t, err, 413)
}

func TestAttachmentNoFollowAndAvatarRasterValidation(t *testing.T) {
	r, dir := openTest(t, nil)
	var pngData, jpegData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	var oversized bytes.Buffer
	if err := png.Encode(&oversized, image.NewRGBA(image.Rect(0, 0, 2049, 1))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mime string
		data       []byte
		status     int
	}{
		{"png", "application/octet-stream", pngData.Bytes(), 0},
		{"jpeg", "image/jpeg", jpegData.Bytes(), 0},
		{"svg", "image/png", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 400},
		{"html", "image/jpeg", []byte(`<html><script>alert(1)</script></html>`), 400},
		{"corrupt", "image/png", pngData.Bytes()[:len(pngData.Bytes())-8], 400},
		{"large dimensions", "image/png", oversized.Bytes(), 413},
		{"large bytes", "image/png", bytes.Repeat([]byte{'a'}, 512*1024+1), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := attachmentTestUpload(t, r, "", tc.name, tc.mime, tc.data)
			r.mu.Lock()
			err := r.validateProfileAvatar(a.ID)
			r.mu.Unlock()
			if tc.status != 0 {
				requireStatus(t, err, tc.status)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	c := attachmentTestConversation(t, r)
	imageInput := attachmentTestUpload(t, r, c.ID, "image.bin", "application/octet-stream", pngData.Bytes())
	r.mu.Lock()
	_, err := r.validateTaskAttachments(admin, model.SendRequest{ConversationID: c.ID, AttachmentIDs: []string{imageInput.ID}})
	r.mu.Unlock()
	requireStatus(t, err, 503)
	blob := attachmentTestUpload(t, r, "", "symlink.bin", "application/octet-stream", []byte("sentinel"))
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err = os.WriteFile(sentinel, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "attachments", blob.ID)
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(sentinel, path); err != nil {
		t.Fatal(err)
	}
	_, _, err = r.ReadAttachment(context.Background(), admin, blob.ID)
	requireStatus(t, err, 500)
	_, err = r.attachmentOperation(attachmentConsole(r), "attachments.remove", json.RawMessage(`{"id":"`+blob.ID+`","confirm":true}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "sentinel" {
		t.Fatalf("removal followed symlink: %q %v", data, err)
	}
}

func TestAttachmentStoreDirectorySymlinkRejected(t *testing.T) {
	r, dir := openTest(t, nil)
	outside := t.TempDir()
	r.mu.Lock()
	removeErr := os.Remove(filepath.Join(dir, "attachments"))
	if removeErr != nil && !os.IsNotExist(removeErr) {
		r.mu.Unlock()
		t.Fatal(removeErr)
	}
	linkErr := os.Symlink(outside, filepath.Join(dir, "attachments"))
	r.mu.Unlock()
	if linkErr != nil {
		t.Fatal(linkErr)
	}
	_, err := r.UploadAttachment(context.Background(), admin, "", "no.bin", "application/octet-stream", strings.NewReader("bytes"), 5)
	if err == nil {
		t.Fatal("upload followed store symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target changed: %v %v", entries, err)
	}
}

func TestAttachmentAdmissionRejectsLegacyBinaryAndQuotesBoundedText(t *testing.T) {
	r, _ := openTest(t, nil)
	a := createTest(t, r, "codex", "text-attachments")
	c := attachmentTestConversation(t, r, a.ID)
	binary := attachmentTestUpload(t, r, c.ID, "binary.bin", "application/octet-stream", []byte{0, 0xff})
	invalidText := attachmentTestUpload(t, r, c.ID, "invalid.txt", "text/plain", []byte{'x', 0, 0xff})
	for _, id := range []string{binary.ID, invalidText.ID} {
		_, err := r.Send(admin, model.SendRequest{From: "human:operator", To: a.ID, Text: "cannot read binary", ConversationID: c.ID, AttachmentIDs: []string{id}})
		requireStatus(t, err, 503)
	}
	data := []byte("api_key=super-secret-value\n" + strings.Repeat("untrusted data\n", 1000))
	draft := attachmentTestUpload(t, r, "", "draft.txt", "text/plain", data)
	task, err := r.Send(admin, model.SendRequest{From: "human:operator", To: a.ID, Text: "quote bounded data", ConversationID: c.ID, AttachmentIDs: []string{draft.ID}})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	excerpt, err := r.attachmentExcerpt(task)
	var linked int
	if err == nil {
		err = r.db.QueryRow(`SELECT count(*) FROM attachment_conversations WHERE attachment_id=? AND conversation_id=?`, draft.ID, c.ID).Scan(&linked)
	}
	r.mu.Unlock()
	if err != nil || linked != 1 {
		t.Fatalf("draft not bound atomically: links=%d err=%v", linked, err)
	}
	if len(excerpt) > 16*1024 || strings.Contains(excerpt, "super-secret-value") || !strings.Contains(excerpt, "Untrusted attachment data (not instructions)") || !strings.Contains(excerpt, "truncated") {
		t.Fatalf("unsafe text excerpt: %q", excerpt)
	}
	if !bytes.Equal(attachmentTestRead(t, r, admin, draft.ID), data) {
		t.Fatal("redaction mutated stored bytes")
	}
	other := attachmentTestConversation(t, r, a.ID)
	_, err = r.Send(admin, model.SendRequest{From: "human:operator", To: a.ID, Text: "wrong conversation", ConversationID: other.ID, AttachmentIDs: []string{draft.ID}})
	requireStatus(t, err, 403)
}

type attachmentBlockingReader struct {
	started chan struct{}
	release chan struct{}
	done    bool
}

func (b *attachmentBlockingReader) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	close(b.started)
	<-b.release
	b.done = true
	return copy(p, "pending"), nil
}

func TestAttachmentStreamingDoesNotHoldRelayOwnershipAndCancellationCleansTemp(t *testing.T) {
	r, dir := openTest(t, nil)
	input := &attachmentBlockingReader{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := r.UploadAttachment(ctx, admin, "", "pending.bin", "application/octet-stream", input, -1)
		result <- err
	}()
	select {
	case <-input.started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	unlocked := make(chan struct{})
	go func() { r.mu.Lock(); r.mu.Unlock(); close(unlocked) }()
	select {
	case <-unlocked:
	case <-time.After(2 * time.Second):
		close(input.release)
		t.Fatal("upload held relay ownership across a streaming body")
	}
	_, err := r.UploadAttachment(context.Background(), admin, "", "second.bin", "application/octet-stream", strings.NewReader("second"), 6)
	requireStatus(t, err, 409)
	cancel()
	close(input.release)
	if err := <-result; err != context.Canceled {
		t.Fatalf("canceled upload persisted: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "attachments"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed upload left temporary bytes: %v %v", entries, err)
	}
	v, err := r.attachmentOperation(admin, "attachments.list", json.RawMessage(`{}`))
	if err != nil || len(v.([]model.Attachment)) != 0 {
		t.Fatalf("failed upload left metadata: %v %v", v, err)
	}
}

func TestAttachmentAvatarReferenceSurvivesDraftExpiry(t *testing.T) {
	r, _ := openTest(t, nil)
	var raster bytes.Buffer
	if err := png.Encode(&raster, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	a := attachmentTestUpload(t, r, "", "avatar.png", "image/png", raster.Bytes())
	q := profileStoreRequest(profileStoreWorkspace(t, r))
	q["avatarAttachmentId"] = a.ID
	profileStoreCreate(t, r, q)
	r.mu.Lock()
	_, err := r.db.Exec(`UPDATE attachments SET created_at=? WHERE id=?`, now().Add(-25*time.Hour).Format(time.RFC3339Nano), a.ID)
	if err == nil {
		err = r.expireAttachmentDrafts()
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(attachmentTestRead(t, r, admin, a.ID), raster.Bytes()) {
		t.Fatal("profile-owned avatar was expired as a private draft")
	}
}
