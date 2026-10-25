package relay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
)

// A template is deliberately not a live profile, trigger, or execution grant.
// Consumers must review workspace/profile references and use the ordinary save
// operation before it can become an executable resource.
type localTemplate struct {
	ID            string          `json:"id,omitempty"`
	Revision      int             `json:"revision,omitempty"`
	SchemaVersion int             `json:"schemaVersion"`
	Kind          string          `json:"kind"`
	Definition    json.RawMessage `json:"definition"`
	Enabled       bool            `json:"enabled"`
	SkillID       string          `json:"skillId,omitempty"`
}
type localTemplateSummary struct {
	ID         string          `json:"id"`
	Revision   int             `json:"revision"`
	Kind       string          `json:"kind"`
	Name       string          `json:"name,omitempty"`
	Enabled    bool            `json:"enabled"`
	Definition json.RawMessage `json:"definition"`
}
type templatePreview struct {
	ID            string                 `json:"id"`
	Digest        string                 `json:"digest"`
	ExpiresAt     time.Time              `json:"expiresAt"`
	Template      localTemplate          `json:"template"`
	AssetManifest []templateAssetSummary `json:"assets,omitempty"`
}
type templateAssetSummary struct {
	Path   string `json:"path"`
	MIME   string `json:"mime"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}
type storedTemplatePreview struct {
	Owner    string        `json:"owner"`
	Template localTemplate `json:"template"`
}

type templateProfile struct {
	Name                 string                    `json:"name"`
	Instructions         string                    `json:"instructions"`
	Binding              templateProvider          `json:"binding"`
	Capabilities         model.ProfileCapabilities `json:"capabilities"`
	AllowedProfileIDs    []string                  `json:"allowedProfileIds"`
	NotificationsEnabled bool                      `json:"notificationsEnabled"`
	Archived             bool                      `json:"archived"`
}
type templateProvider struct {
	Harness string `json:"harness"`
	Config  struct {
		Model string `json:"model,omitempty"`
	} `json:"config"`
}
type templateRoutine struct {
	Name           string         `json:"name"`
	ProfileID      string         `json:"profileId"`
	Instruction    string         `json:"instruction"`
	Trigger        routineTrigger `json:"trigger"`
	FlowID         string         `json:"flowId,omitempty"`
	FlowRevision   int            `json:"flowRevision,omitempty"`
	Enabled        bool           `json:"enabled"`
	AutoStart      bool           `json:"autoStart"`
	TimeoutSeconds int            `json:"timeoutSeconds"`
}

// Whitelisting each definition prevents future DTO fields from accidentally
// exporting native sessions, auth paths, secret references or new permissions.
func sanitizeTemplate(kind string, raw json.RawMessage) (localTemplate, error) {
	value := localTemplate{SchemaVersion: 1, Kind: kind}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return value, model.Err(400, "template definition must be an object")
	}
	var definition any
	switch kind {
	case "profile":
		var source model.Profile
		if err := json.Unmarshal(raw, &source); err != nil {
			return value, model.Err(400, "invalid profile template")
		}
		if strings.TrimSpace(source.Name) == "" || len(source.Name) > 128 || !safeRegistryText(source.Name) || len(source.Instructions) > 16*1024 || !utf8.ValidString(source.Instructions) {
			return value, model.Err(400, "invalid profile template text")
		}
		target := templateProfile{Name: redactSecrets(source.Name), Instructions: redactSecrets(source.Instructions), AllowedProfileIDs: []string{}, Archived: true}
		switch source.Binding.Harness {
		case "codex", "claude", "omp", "opencode", "openai", "acp", "":
			target.Binding.Harness = source.Binding.Harness
		default:
			return value, model.Err(400, "unsupported template harness")
		}
		if len(source.Binding.Config.Model) > 200 || !safeRegistryText(source.Binding.Config.Model) {
			return value, model.Err(400, "invalid template model")
		}
		target.Binding.Config.Model = redactSecrets(source.Binding.Config.Model)
		definition = target
	case "routine":
		var source routineDefinition
		if err := json.Unmarshal(raw, &source); err != nil {
			return value, model.Err(400, "invalid routine template")
		}
		if strings.TrimSpace(source.Name) == "" || len(source.Name) > 128 || !safeRegistryText(source.Name) || len(source.Instruction) > 65536 || !utf8.ValidString(source.Instruction) {
			return value, model.Err(400, "invalid routine template text")
		}
		if err := source.Trigger.validate(); err != nil {
			return value, err
		}
		if source.TimeoutSeconds == 0 {
			source.TimeoutSeconds = 3600
		}
		if source.TimeoutSeconds < 1 || source.TimeoutSeconds > 86400 || source.FlowRevision < 0 {
			return value, model.Err(400, "invalid routine template bounds")
		}
		if source.ProfileID != "" && !profileIdentifier(source.ProfileID) || source.FlowID != "" && !profileIdentifier(source.FlowID) {
			return value, model.Err(400, "invalid template reference")
		}
		definition = templateRoutine{Name: redactSecrets(source.Name), ProfileID: source.ProfileID, Instruction: redactSecrets(source.Instruction), Trigger: source.Trigger, FlowID: source.FlowID, FlowRevision: source.FlowRevision, TimeoutSeconds: source.TimeoutSeconds}
	case "flow":
		var source routineFlow
		if err := json.Unmarshal(raw, &source); err != nil {
			return value, model.Err(400, "invalid flow template")
		}
		if len(source.Nodes) < 1 || len(source.Nodes) > 32 || len(source.Edges) > 64 {
			return value, model.Err(400, "invalid flow template size")
		}
		nodes := map[string]bool{}
		edges := map[routineFlowEdge]bool{}
		for i, node := range source.Nodes {
			if !profileIdentifier(node.ID) || !profileIdentifier(node.ProfileID) || nodes[node.ID] || len(node.Instruction) > 65536 || !utf8.ValidString(node.Instruction) {
				return value, model.Err(400, "invalid flow template node")
			}
			nodes[node.ID] = true
			source.Nodes[i].Instruction = redactSecrets(node.Instruction)
		}
		for _, edge := range source.Edges {
			if !nodes[edge.From] || !nodes[edge.To] || edges[edge] {
				return value, model.Err(400, "invalid flow template edge")
			}
			edges[edge] = true
		}
		if routineFlowHasCycle(source) {
			return value, model.Err(400, "flow template contains a cycle")
		}
		source.ID = ""
		source.Revision = 0
		definition = source
	case "skill":
		var source localSkillVersion
		if err := json.Unmarshal(raw, &source); err != nil {
			return value, model.Err(400, "invalid skill template")
		}
		if err := validateReviewedSkill(source.Name, source.Description, source.Markdown); err != nil {
			return value, err
		}
		if len(source.AssetAttachmentIDs) > 0 {
			return value, model.Err(400, "skill template assets must contain immutable bytes, not attachment references")
		}
		if len(source.Assets)+1 > templateMaxEntries {
			return value, model.Err(413, "skill asset entry limit exceeded")
		}
		seen := map[string]bool{"SKILL.md": true}
		total := len(source.Markdown)
		for i, asset := range source.Assets {
			clean, err := templateArchivePath(asset.Path, false)
			if err != nil {
				return value, err
			}
			if seen[clean] {
				return value, model.Err(400, "duplicate skill asset path")
			}
			seen[clean] = true
			for prior := range seen {
				if prior != clean && (strings.HasPrefix(prior, clean+"/") || strings.HasPrefix(clean, prior+"/")) {
					return value, model.Err(400, "skill asset path conflict")
				}
			}
			total += len(asset.Data)
			if total > templateMaxBytes {
				return value, model.Err(413, "skill assets exceed 25 MiB")
			}
			digest := templateDigest(asset.Data)
			if asset.SHA256 != "" && asset.SHA256 != digest {
				return value, model.Err(400, "skill asset digest mismatch")
			}
			source.Assets[i].SHA256 = digest
			// Content types are descriptive data, never used to render active content.
			source.Assets[i].MIME = "application/octet-stream"
		}
		source.ID = ""
		source.Version = 0
		source.Revision = 0
		source.SHA256 = ""
		source.Markdown = redactSecrets(source.Markdown)
		source.Name = redactSecrets(source.Name)
		source.Description = redactSecrets(source.Description)
		definition = source
	default:
		return value, model.Err(400, "unsupported template kind")
	}
	body, err := json.Marshal(definition)
	if err != nil {
		return value, err
	}
	value.Definition = body
	return value, nil
}

// templateOperation is the single automation dispatch entry point. It deliberately
// uses administrator authority, never the active model/tool principal.
func (r *Relay) templateOperation(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch operation {
	case "templates.list":
		var q struct {
			After string `json:"after"`
			Limit int    `json:"limit"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if q.Limit == 0 {
			q.Limit = 50
		}
		if q.Limit < 1 || q.Limit > 100 {
			return nil, model.Err(400, "limit must be 1..100")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed {
			return nil, model.Err(503, "relay is stopping")
		}
		rows, err := r.db.QueryContext(ctx, "SELECT id,revision,kind,coalesce(json_extract(body,'$.definition.name'),''),CASE WHEN kind='skill' THEN json_object('skillId',json_extract(body,'$.skillId'),'version',1) ELSE json_extract(body,'$.definition') END FROM local_templates WHERE id>? ORDER BY id LIMIT ?", q.After, q.Limit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		page := model.AutomationPage[localTemplateSummary]{Items: []localTemplateSummary{}}
		for rows.Next() {
			var value localTemplateSummary
			var definition string
			if err = rows.Scan(&value.ID, &value.Revision, &value.Kind, &value.Name, &definition); err != nil {
				return nil, err
			}
			if len(page.Items) == q.Limit {
				page.HasMore = true
				break
			}
			value.Definition = json.RawMessage(definition)
			page.Items = append(page.Items, value)
		}
		if page.HasMore {
			page.NextAfter = page.Items[len(page.Items)-1].ID
		}
		return page, rows.Err()
	case "templates.export":
		return r.templateExport(ctx, p, raw)
	case "templates.importPreview":
		return r.templateImportPreview(ctx, p, raw)
	case "templates.import":
		return r.templateImport(ctx, p, raw)
	}
	return nil, model.Err(404, "unknown template operation")
}

func (r *Relay) templateExport(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	var q struct {
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		Version int    `json:"version,omitempty"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) || q.Version < 0 {
		return nil, model.Err(400, "invalid template source")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, model.Err(503, "relay is stopping")
	}
	value, err := r.templateExportDefinition(q.Kind, q.ID, q.Version)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	encoded, err := encodeTemplateArchive(value)
	if err != nil {
		return nil, err
	}
	output := bytes.NewBuffer(encoded)
	attachment, err := r.UploadAttachment(ctx, p, "", "meshra-"+q.Kind+"-template.zip", "application/zip", bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		return nil, err
	}
	return map[string]any{"attachment": attachment, "digest": templateDigest(output.Bytes())}, nil
}

// Caller holds relay ownership; all data is captured before streaming an export.
func (r *Relay) templateExportDefinition(kind, id string, version int) (localTemplate, error) {
	var draftBody string
	err := r.db.QueryRow("SELECT body FROM local_templates WHERE id=?", id).Scan(&draftBody)
	if err == nil {
		var draft localTemplate
		if err = json.Unmarshal([]byte(draftBody), &draft); err != nil {
			return localTemplate{}, err
		}
		if draft.Kind != kind {
			return localTemplate{}, model.Err(400, "template kind does not match")
		}
		if version != 0 && version != draft.Revision {
			return localTemplate{}, model.Err(404, "template revision not found")
		}
		return sanitizeTemplate(kind, draft.Definition)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return localTemplate{}, err
	}
	var definition any
	switch kind {
	case "profile":
		value, err := r.profile(id)
		if err != nil {
			return localTemplate{}, err
		}
		definition = value
	case "routine":
		value, _, err := r.routineDefinition(id)
		if err != nil {
			return localTemplate{}, err
		}
		definition = value
	case "flow":
		value, err := r.loadRoutineFlow(id, version)
		if err != nil {
			return localTemplate{}, err
		}
		definition = value
	case "skill":
		if version < 1 {
			return localTemplate{}, model.Err(400, "export an explicit pinned skill version")
		}
		value, err := r.readLocalSkill(id, version)
		if err != nil {
			return localTemplate{}, err
		}
		total := len(value.Markdown)
		for _, asset := range value.Assets {
			total += len(asset.Data)
		}
		for _, attachmentID := range value.AssetAttachmentIDs {
			asset, err := r.attachment(attachmentID)
			if err != nil {
				return localTemplate{}, err
			}
			if asset.Size > int64(templateMaxBytes-total) {
				return localTemplate{}, model.Err(413, "skill assets exceed 25 MiB")
			}
			file, err := r.openAttachment(asset)
			if err != nil {
				return localTemplate{}, err
			}
			data, readErr := io.ReadAll(io.LimitReader(file, int64(templateMaxBytes-total)+1))
			file.Close()
			if readErr != nil {
				return localTemplate{}, readErr
			}
			total += len(data)
			if total > templateMaxBytes {
				return localTemplate{}, model.Err(413, "skill assets exceed 25 MiB")
			}
			value.Assets = append(value.Assets, skillAsset{Path: skillAttachmentPath(asset), MIME: asset.MIME, Data: data, SHA256: templateDigest(data)})
		}
		value.AssetAttachmentIDs = nil
		definition = value
	default:
		return localTemplate{}, model.Err(400, "unsupported template kind")
	}
	body, err := json.Marshal(definition)
	if err != nil {
		return localTemplate{}, err
	}
	return sanitizeTemplate(kind, body)
}

func (r *Relay) templateImportPreview(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	var q struct {
		AttachmentID string `json:"attachmentId"`
		Name         string `json:"name,omitempty"`
		Description  string `json:"description,omitempty"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	attachment, reader, err := r.ReadAttachment(ctx, p, q.AttachmentID)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, templateMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > templateMaxBytes {
		return nil, model.Err(413, "template exceeds 25 MiB")
	}
	if templateDigest(data) != attachment.SHA256 {
		return nil, model.Err(409, "template attachment digest changed")
	}
	var value localTemplate
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		value, err = decodeTemplateDocument(trimmed)
	} else {
		entries, archiveErr := readTemplateArchive(data)
		if archiveErr != nil {
			return nil, archiveErr
		}
		if len(entries) == 1 && entries[0].name == "template.json" && !entries[0].directory {
			value, err = decodeTemplateDocument(entries[0].data)
		} else {
			skill, skillErr := skillFromArchive(entries, q.Name, q.Description)
			if skillErr != nil {
				return nil, skillErr
			}
			body, marshalErr := json.Marshal(skill)
			if marshalErr != nil {
				return nil, marshalErr
			}
			value, err = sanitizeTemplate("skill", body)
		}
	}
	if err != nil {
		return nil, err
	}
	// Even a self-describing skill export needs an explicit human name/description
	// review; neither frontmatter nor an archive metadata file supplies authority.
	if value.Kind == "skill" {
		var skill localSkillVersion
		if err = json.Unmarshal(value.Definition, &skill); err != nil {
			return nil, err
		}
		if strings.TrimSpace(q.Description) == "" {
			return nil, model.Err(400, "reviewed skill description is required")
		}
		if err = validateReviewedSkill(q.Name, q.Description, skill.Markdown); err != nil {
			return nil, err
		}
		skill.Name = q.Name
		skill.Description = q.Description
		value.Definition, err = json.Marshal(skill)
		if err != nil {
			return nil, err
		}
	}
	preview := templatePreview{ID: randomID(), Digest: templateDigest(data), ExpiresAt: now().Add(15 * time.Minute), Template: value}
	if value.Kind == "skill" {
		var skill localSkillVersion
		if err = json.Unmarshal(value.Definition, &skill); err != nil {
			return nil, err
		}
		for _, asset := range skill.Assets {
			preview.AssetManifest = append(preview.AssetManifest, templateAssetSummary{asset.Path, asset.MIME, len(asset.Data), asset.SHA256})
		}
	}
	stored, err := json.Marshal(storedTemplatePreview{Owner: actor(p), Template: value})
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if _, err = r.db.ExecContext(ctx, "DELETE FROM template_import_previews WHERE expires_at<=?", now().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	var count int
	if err = r.db.QueryRow("SELECT count(*) FROM template_import_previews").Scan(&count); err != nil {
		return nil, err
	}
	if count >= 100 {
		return nil, model.Err(429, "too many pending template previews")
	}
	_, err = r.db.ExecContext(ctx, "INSERT INTO template_import_previews(id,digest,expires_at,body) VALUES(?,?,?,?)", preview.ID, preview.Digest, preview.ExpiresAt.Format(time.RFC3339Nano), string(stored))
	return preview, err
}

func decodeTemplateDocument(data []byte) (localTemplate, error) {
	var value localTemplate
	if err := decodeAutomation(data, &value); err != nil {
		return value, err
	}
	if value.SchemaVersion != 1 {
		return value, model.Err(400, "unsupported template schema version")
	}
	return sanitizeTemplate(value.Kind, value.Definition)
}

func (r *Relay) templateImport(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	var q struct {
		PreviewID      string `json:"previewId"`
		ExpectedDigest string `json:"expectedDigest"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.PreviewID) || len(q.ExpectedDigest) != 64 {
		return nil, model.Err(400, "invalid template preview reference")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var body, digest, expires string
	if err = tx.QueryRowContext(ctx, "SELECT body,digest,expires_at FROM template_import_previews WHERE id=?", q.PreviewID).Scan(&body, &digest, &expires); errors.Is(err, sql.ErrNoRows) {
		return nil, model.Err(404, "template preview not found or already consumed")
	} else if err != nil {
		return nil, err
	}
	expiry, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return nil, err
	}
	if !expiry.After(now()) {
		return nil, model.Err(409, "template preview expired")
	}
	if digest != q.ExpectedDigest {
		return nil, model.Err(409, "template preview digest changed")
	}
	var stored storedTemplatePreview
	if err = json.Unmarshal([]byte(body), &stored); err != nil {
		return nil, err
	}
	if stored.Owner != actor(p) {
		return nil, model.Err(403, "template preview belongs to another operator")
	}
	value, err := sanitizeTemplate(stored.Template.Kind, stored.Template.Definition)
	if err != nil {
		return nil, err
	}
	value.ID = randomID()
	value.Revision = 1
	var skill *localSkillVersion
	if value.Kind == "skill" {
		var version localSkillVersion
		if err = json.Unmarshal(value.Definition, &version); err != nil {
			return nil, err
		}
		version.ID = randomID()
		version.Version = 1
		version.Revision = 1
		content, marshalErr := json.Marshal(struct {
			Markdown string
			Assets   []skillAsset
		}{version.Markdown, version.Assets})
		if marshalErr != nil {
			return nil, marshalErr
		}
		version.SHA256 = templateDigest(content)
		encoded, marshalErr := json.Marshal(version)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO local_skills(id,revision,name,description) VALUES(?,1,?,?)", version.ID, version.Name, version.Description); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO local_skill_versions(skill_id,revision,sha256,body) VALUES(?,1,?,?)", version.ID, version.SHA256, string(encoded)); err != nil {
			return nil, err
		}
		skill = &version
		value.SkillID = version.ID
		if err = insertAutomationEvent(tx, "skills.saved", version.ID, "", map[string]any{"revision": 1, "imported": true}); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO local_templates(id,revision,kind,body) VALUES(?,1,?,?)", value.ID, value.Kind, string(encoded)); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM template_import_previews WHERE id=? AND digest=?", q.PreviewID, digest)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, model.Err(409, "template preview already consumed")
	}
	if err = insertAutomationEvent(tx, "templates.imported", value.ID, "", map[string]any{"kind": value.Kind, "revision": 1}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return struct {
		Template localTemplate      `json:"template"`
		Skill    *localSkillVersion `json:"skill,omitempty"`
	}{value, skill}, nil
}
