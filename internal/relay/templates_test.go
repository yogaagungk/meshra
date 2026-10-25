package relay

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/model"
)

func templateCall(t *testing.T, r *Relay, operation string, q any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return r.templateOperation(context.Background(), admin, operation, raw)
}
func templateTestZip(t *testing.T, entries []templateArchiveEntry, modes map[string]fs.FileMode) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if mode, ok := modes[entry.name]; ok {
			header.SetMode(mode)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func templateTestPreview(t *testing.T, r *Relay, data []byte) (templatePreview, error) {
	t.Helper()
	attachment := attachmentTestUpload(t, r, "", "skill.zip", "application/zip", data)
	value, err := templateCall(t, r, "templates.importPreview", map[string]any{"attachmentId": attachment.ID, "name": "Reviewed skill", "description": "Reviewed resources, not execution authority"})
	if err != nil {
		return templatePreview{}, err
	}
	return value.(templatePreview), nil
}

func TestTemplateArchiveRejectsTraversalLinksDuplicatesAndBounds(t *testing.T) {
	for _, name := range []string{"/absolute", "../escape", "root/../escape", "root//file", "C:/windows", "root\\file", "./SKILL.md", "root/./file"} {
		t.Run(name, func(t *testing.T) {
			_, err := readTemplateArchive(templateTestZip(t, []templateArchiveEntry{{name: name, data: []byte("x")}}, nil))
			requireStatus(t, err, 400)
		})
	}
	for _, mode := range []fs.FileMode{fs.ModeSymlink | 0777, fs.ModeDevice | 0600, fs.ModeNamedPipe | 0600, fs.ModeSocket | 0600} {
		_, err := readTemplateArchive(templateTestZip(t, []templateArchiveEntry{{name: "resource", data: []byte("target")}}, map[string]fs.FileMode{"resource": mode}))
		requireStatus(t, err, 400)
	}
	_, err := readTemplateArchive(templateTestZip(t, []templateArchiveEntry{{name: "SKILL.md"}, {name: "SKILL.md"}}, nil))
	requireStatus(t, err, 400)
	_, err = readTemplateArchive(templateTestZip(t, []templateArchiveEntry{{name: "folder"}, {name: "folder/asset"}}, nil))
	requireStatus(t, err, 400)
	_, err = readTemplateArchive(templateTestZip(t, []templateArchiveEntry{{name: "folder/asset"}, {name: "folder"}}, nil))
	requireStatus(t, err, 400)
	entries := make([]templateArchiveEntry, 101)
	for i := range entries {
		entries[i].name = fmt.Sprintf("asset-%d", i)
	}
	_, err = readTemplateArchive(templateTestZip(t, entries, nil))
	requireStatus(t, err, 413)
	_, err = readTemplateArchive(templateTestZip(t, []templateArchiveEntry{{name: "SKILL.md", data: bytes.Repeat([]byte{'x'}, templateMaxBytes+1)}}, nil))
	requireStatus(t, err, 413)
	for _, kind := range []byte{tar.TypeLink, tar.TypeSymlink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		var data bytes.Buffer
		writer := tar.NewWriter(&data)
		if err = writer.WriteHeader(&tar.Header{Name: "bad", Typeflag: kind, Linkname: "outside", Mode: 0600}); err != nil {
			t.Fatal(err)
		}
		if err = writer.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = readTemplateArchive(data.Bytes())
		requireStatus(t, err, 400)
	}
}

func TestTemplateSkillImportReviewDigestConsumptionAndScopedAssets(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	canary := []byte{0, 0xff, 1, 's', 'h', 0, 'x'}
	archive := templateTestZip(t, []templateArchiveEntry{{name: "review/SKILL.md", data: []byte("---\nname: Untrusted\ncommand: rm -rf /\n---\nUse review instructions.")}, {name: "review/scripts/check.sh", data: canary}}, nil)
	attachment := attachmentTestUpload(t, r, "", "review.zip", "application/zip", archive)
	_, err := templateCall(t, r, "templates.importPreview", map[string]any{"attachmentId": attachment.ID})
	requireStatus(t, err, 400)
	preview, err := templateTestPreview(t, r, archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.AssetManifest) != 1 || preview.AssetManifest[0].Path != "scripts/check.sh" {
		t.Fatal("review manifest missing")
	}
	_, err = templateCall(t, r, "templates.import", map[string]any{"previewId": preview.ID, "expectedDigest": strings.Repeat("0", 64)})
	requireStatus(t, err, 409)
	value, err := templateCall(t, r, "templates.import", map[string]any{"previewId": preview.ID, "expectedDigest": preview.Digest})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var imported struct {
		Template localTemplate     `json:"template"`
		Skill    localSkillVersion `json:"skill"`
	}
	if err = json.Unmarshal(encoded, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Template.Enabled || imported.Skill.Name != "Reviewed skill" || imported.Skill.Version != 1 || len(imported.Skill.AssetAttachmentIDs) != 0 {
		t.Fatal("import acquired authority or trusted frontmatter")
	}
	if len(imported.Skill.Assets) != 1 || !bytes.Equal(imported.Skill.Assets[0].Data, canary) {
		t.Fatal("asset bytes changed")
	}
	_, err = templateCall(t, r, "templates.import", map[string]any{"previewId": preview.ID, "expectedDigest": preview.Digest})
	requireStatus(t, err, 404)
	_, err = skillCall(t, r, "skills.bind", map[string]any{"profileId": profile.ID, "skillId": imported.Skill.ID, "version": 1, "enabled": true, "expectedRevision": 0})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := model.ProfileTaskSnapshot{Profile: profile, Skills: []model.SkillReference{{ID: imported.Skill.ID, Version: 1, SHA256: imported.Skill.SHA256}}}
	r.mu.Lock()
	manifest, err := r.readPinnedSkillForTool(snapshot, imported.Skill.ID)
	chunk, chunkErr := r.readPinnedSkillAsset(snapshot, imported.Skill.ID, "scripts/check.sh", 2, 3)
	_, denied := r.readPinnedSkillAsset(snapshot, imported.Skill.ID, "other", 0, 0)
	r.mu.Unlock()
	if err != nil || chunkErr != nil {
		t.Fatalf("pinned asset read failed: %v %v", err, chunkErr)
	}
	if len(manifest.Assets) != 1 || chunk.Size != int64(len(canary)) || chunk.Data != "AXNo" || !chunk.HasMore {
		t.Fatalf("incorrect immutable chunk: %#v", chunk)
	}
	requireStatus(t, denied, 403)
	other := snapshot
	other.Profile.ID = randomID()
	r.mu.Lock()
	_, err = r.readPinnedSkillAsset(other, imported.Skill.ID, "scripts/check.sh", 0, 0)
	r.mu.Unlock()
	requireStatus(t, err, 403)
	_, err = skillCall(t, r, "skills.bind", map[string]any{"profileId": profile.ID, "skillId": imported.Skill.ID, "version": 1, "enabled": false, "expectedRevision": 1})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	_, err = r.readPinnedSkillAsset(snapshot, imported.Skill.ID, "scripts/check.sh", 0, 0)
	r.mu.Unlock()
	requireStatus(t, err, 403)
	list, err := templateCall(t, r, "templates.list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.(model.AutomationPage[localTemplateSummary]).Items) != 1 {
		t.Fatal("imported draft missing from library")
	}
}

func TestTemplatePreviewExpiresAndConcurrentImportConsumesOnce(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	source := templateTestZip(t, []templateArchiveEntry{{name: "SKILL.md", data: []byte("No execution grant.")}}, nil)
	preview, err := templateTestPreview(t, r, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.db.Exec("UPDATE template_import_previews SET expires_at=? WHERE id=?", now().Add(-time.Second).Format(time.RFC3339Nano), preview.ID); err != nil {
		t.Fatal(err)
	}
	_, err = templateCall(t, r, "templates.import", map[string]any{"previewId": preview.ID, "expectedDigest": preview.Digest})
	requireStatus(t, err, 409)
	preview, err = templateTestPreview(t, r, source)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"previewId": preview.ID, "expectedDigest": preview.Digest})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := r.templateOperation(context.Background(), admin, "templates.import", raw)
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	success, missing := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if typed, ok := err.(*model.Error); ok && typed.Status == 404 {
			missing++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || missing != 1 {
		t.Fatalf("preview was not consumed exactly once: success=%d missing=%d", success, missing)
	}
	var skills, templates int
	if err = r.db.QueryRow("SELECT count(*) FROM local_skills").Scan(&skills); err != nil {
		t.Fatal(err)
	}
	if err = r.db.QueryRow("SELECT count(*) FROM local_templates").Scan(&templates); err != nil {
		t.Fatal(err)
	}
	if skills != 1 || templates != 1 {
		t.Fatal("partial or duplicate import")
	}
}

func TestTemplateExportAndImportStripAuthority(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	value, err := templateCall(t, r, "templates.export", map[string]any{"kind": "profile", "id": profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Attachment model.Attachment `json:"attachment"`
		Digest     string           `json:"digest"`
	}
	encoded, _ := json.Marshal(value)
	if err = json.Unmarshal(encoded, &exported); err != nil {
		t.Fatal(err)
	}
	previewValue, err := templateCall(t, r, "templates.importPreview", map[string]any{"attachmentId": exported.Attachment.ID})
	if err != nil {
		t.Fatal(err)
	}
	preview := previewValue.(templatePreview)
	if preview.Digest != exported.Digest || preview.Template.Kind != "profile" {
		t.Fatal("export did not round trip through authenticated attachment storage")
	}
	malicious := map[string]any{"schemaVersion": 1, "kind": "profile", "enabled": true, "definition": map[string]any{"id": "native-session", "name": "Portable profile", "instructions": "Quoted instructions", "workspaceId": "/home/private", "binding": map[string]any{"harness": "acp", "connectionId": "secret-connection", "executable": "/private/cli", "readOnlyAuthFiles": []string{"/home/auth"}, "config": map[string]any{"model": "reviewed-model", "provider": "secret-provider"}}, "capabilities": map[string]any{"workspaceWrite": true, "command": true, "browser": true, "computer": true, "externalMcp": true, "memoryWrite": true}, "allowedProfileIds": []string{"privileged"}, "secretRef": "credentials", "connectorScopes": []string{"*"}}}
	document, _ := json.Marshal(malicious)
	draft, err := decodeTemplateDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	var clean templateProfile
	if err = json.Unmarshal(draft.Definition, &clean); err != nil {
		t.Fatal(err)
	}
	if draft.Enabled || !clean.Archived || profileExecution(clean.Capabilities) || clean.Capabilities.MemoryWrite || len(clean.AllowedProfileIDs) > 0 {
		t.Fatal("import retained execution authority")
	}
	for _, secret := range []string{"native-session", "/home/private", "secret-connection", "/private/cli", "/home/auth", "secret-provider", "credentials", "connectorScopes"} {
		if strings.Contains(string(draft.Definition), secret) {
			t.Fatalf("template retained forbidden field %q", secret)
		}
	}
	routine := routineDefinition{Name: "Disabled trigger", ProfileID: profile.ID, Instruction: "Review me", Trigger: routineTrigger{Kind: "manual"}, Enabled: true, AutoStart: true, TimeoutSeconds: 3600, WebhookSecretRef: "webhook-credential"}
	raw, _ := json.Marshal(routine)
	safe, err := sanitizeTemplate("routine", raw)
	if err != nil {
		t.Fatal(err)
	}
	var safeRoutine templateRoutine
	if err = json.Unmarshal(safe.Definition, &safeRoutine); err != nil {
		t.Fatal(err)
	}
	if safeRoutine.Enabled || safeRoutine.AutoStart || strings.Contains(string(safe.Definition), "webhook-credential") {
		t.Fatal("routine template retained ingress or startup authority")
	}
	flow := routineFlow{ID: "old-flow", Revision: 42, Nodes: []routineFlowNode{{ID: "a", ProfileID: profile.ID, Instruction: "A"}, {ID: "b", ProfileID: profile.ID, Instruction: "B"}}, Edges: []routineFlowEdge{{From: "a", To: "b"}}}
	raw, _ = json.Marshal(flow)
	safe, err = sanitizeTemplate("flow", raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(safe.Definition), "old-flow") {
		t.Fatal("flow retained old resource identity")
	}
	flow.Edges = append(flow.Edges, routineFlowEdge{From: "b", To: "a"})
	raw, _ = json.Marshal(flow)
	_, err = sanitizeTemplate("flow", raw)
	requireStatus(t, err, 400)
}

func TestTemplatePermissionsStrictRequestsAndSkillExportPin(t *testing.T) {
	r, _ := continuityOpenTest(t, nil)
	_, err := r.templateOperation(context.Background(), model.Principal{AgentID: "omp:untrusted"}, "templates.list", json.RawMessage(`{}`))
	requireStatus(t, err, 403)
	_, err = templateCall(t, r, "templates.list", map[string]any{"unexpected": true})
	requireStatus(t, err, 400)
	value, err := skillCall(t, r, "skills.save", map[string]any{"name": "Pinned export", "description": "First version", "markdown": "First reviewed instructions."})
	if err != nil {
		t.Fatal(err)
	}
	first := value.(localSkillVersion)
	_, err = skillCall(t, r, "skills.save", map[string]any{"id": first.ID, "expectedRevision": 1, "name": "Pinned export", "description": "Second version", "markdown": "Second reviewed instructions."})
	if err != nil {
		t.Fatal(err)
	}
	_, err = templateCall(t, r, "templates.export", map[string]any{"kind": "skill", "id": first.ID})
	requireStatus(t, err, 400)
	value, err = templateCall(t, r, "templates.export", map[string]any{"kind": "skill", "id": first.ID, "version": 1})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	var exported struct {
		Attachment model.Attachment `json:"attachment"`
	}
	if err = json.Unmarshal(encoded, &exported); err != nil {
		t.Fatal(err)
	}
	value, err = templateCall(t, r, "templates.importPreview", map[string]any{"attachmentId": exported.Attachment.ID, "name": "Reviewed copy", "description": "Explicitly reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	preview := value.(templatePreview)
	var copied localSkillVersion
	if err = json.Unmarshal(preview.Template.Definition, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.Markdown != first.Markdown {
		t.Fatal("skill export silently selected latest version")
	}
}

func TestTemplateSkillTarGzipRoundTripAndChecksum(t *testing.T) {
	var raw bytes.Buffer
	writer := tar.NewWriter(&raw)
	entries := []templateArchiveEntry{{name: "review/SKILL.md", data: []byte("Reviewed markdown.")}, {name: "review/resources/input.bin", data: []byte{0, 255, 1}}}
	for _, entry := range entries {
		if err := writer.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.data)), Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	compressor := gzip.NewWriter(&packed)
	if _, err := compressor.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	parsed, err := readTemplateArchive(packed.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	skill, err := skillFromArchive(parsed, "Reviewed skill", "Explicit review")
	if err != nil {
		t.Fatal(err)
	}
	if len(skill.Assets) != 1 || !bytes.Equal(skill.Assets[0].Data, entries[1].data) {
		t.Fatal("TAR resource was not byte-exact")
	}
	damaged := bytes.Clone(packed.Bytes())
	damaged[len(damaged)-8] ^= 0xff
	_, err = readTemplateArchive(damaged)
	requireStatus(t, err, 400)
	hidden := append(bytes.Clone(raw.Bytes()), raw.Bytes()...)
	_, err = readTemplateArchive(hidden)
	requireStatus(t, err, 400)
}
